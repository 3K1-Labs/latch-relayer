package watcher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/stellar/go-stellar-sdk/clients/horizonclient"
	"github.com/stellar/go-stellar-sdk/protocols/horizon/operations"

	"github.com/latch/relayer/internal/config"
	"github.com/latch/relayer/internal/lifecycle"
	"github.com/latch/relayer/internal/memo"
	"github.com/latch/relayer/internal/metrics"
	"github.com/latch/relayer/internal/service/forwarder"
	"github.com/latch/relayer/internal/store"
)

// forwardQueue bounds how many payments can wait for a worker. Polling resumes
// from the saved cursor after a restart, so a full queue means the poll blocks
// briefly rather than payments being dropped.
const forwardQueue = 256

// Watcher polls Horizon for one pool address's payments and hands each inbound
// payment to a bounded pool of forwarder workers.
type Watcher struct {
	pool      config.PoolAccount
	store     watcherStore
	forwarder depositForwarder
	horizon   paymentStreamer
	work      *lifecycle.Tracker
	workers   int

	jobs chan forwardJob
}

// forwardJob is one inbound payment waiting to be forwarded.
type forwardJob struct {
	txHash   string
	landedAt time.Time
	memoID   uint64
	from     string
	amount   string
	asset    string
}

// New builds a watcher. Its workers run on work rather than the stream's
// context, so a shutdown stops new events without abandoning forwards already
// in flight or payments already queued.
//
// workers is how many forwards run at once for this pool. A bound matters:
// unbounded goroutines against a rate-limited RPC produce TRY_AGAIN_LATER storms
// rather than throughput. Without channels, sends are serialised per pool anyway,
// so workers overlap only simulation and the confirmation poll; with channels
// each worker can hold one, so it must be at least the channel count.
func New(pool config.PoolAccount, st *store.Store, fwd *forwarder.Forwarder, hz *horizonclient.Client, work *lifecycle.Tracker, workers int) *Watcher {
	return &Watcher{
		pool:      pool,
		workers:   max(workers, 1),
		store:     st,
		forwarder: fwd,
		horizon:   hz,
		work:      work,
		jobs:      make(chan forwardJob, forwardQueue),
	}
}

// dispatch queues a payment for a worker, blocking only while the queue is
// full — back-pressure on the stream instead of spawning without limit.
// Reports false when intake stopped first; the caller must then not save the
// cursor, so the payment is replayed from Horizon after restart.
func (w *Watcher) dispatch(ctx context.Context, job forwardJob) bool {
	select {
	case w.jobs <- job:
		return true
	case <-ctx.Done():
		return false
	}
}

// startWorkers launches the forward workers on the lifecycle tracker.
//
// intake is the stream's context: when it is cancelled no new payments arrive,
// and each worker finishes whatever is already queued before exiting. That
// matters because a queued payment's cursor is already saved — dropping it
// would mean it is never forwarded. Each forward runs on the tracker's
// context, which the shutdown signal doesn't cancel, so a transaction already
// submitted is followed to completion. Drain bounds all of this by the
// shutdown deadline.
func (w *Watcher) startWorkers(intake context.Context) {
	for range w.workers {
		w.work.Go(func(workCtx context.Context) {
			for {
				select {
				case job := <-w.jobs:
					w.forward(workCtx, job)
				case <-intake.Done():
					for {
						select {
						case job := <-w.jobs:
							w.forward(workCtx, job)
						default:
							return
						}
					}
				}
			}
		})
	}
}

// forward runs one job, whose forwards row handle already inserted. This
// watcher only sees payments to its own pool, so that is the pool holding the
// money for every job it dispatches.
func (w *Watcher) forward(ctx context.Context, job forwardJob) {
	w.forwarder.ForwardRecorded(ctx, w.pool.Address, job.txHash, job.memoID, job.from, job.amount, job.asset, job.landedAt)
}

// pageSize is Horizon's maximum page size.
const pageSize = 200

// pollGap is how long the watcher waits after a page that was not full. New
// payments can only appear when a ledger closes (about every 5s), so a second
// keeps detection well inside one ledger at one request per second per pool.
const pollGap = time.Second

// Run polls the pool's payments and restarts automatically on any error.
// Call in a goroutine: go watcher.Run(ctx).
func (w *Watcher) Run(ctx context.Context) {
	slog.Info("watcher: starting", "pool", w.pool.Address, "workers", w.workers)
	w.startWorkers(ctx)
	for {
		if err := w.poll(ctx); err != nil {
			slog.Error("watcher: poll error, retrying in 5s", "pool", w.pool.Address, "err", err)
		}
		select {
		case <-ctx.Done():
			slog.Info("watcher: stopped", "pool", w.pool.Address)
			return
		case <-time.After(5 * time.Second):
		}
	}
}

// poll reads the pool's payments page by page from the last saved cursor until
// an error or ctx cancel, so a restart resumes exactly where it stopped.
//
// Paged requests rather than Horizon's SSE stream: the stream delivered a burst
// at about 9 payments/s (21/s with a 200-record page), while one paged request
// returns 200 in under 2s. With channels (#48) forwarding 100 per ledger, the
// stream had become the relayer's bottleneck. A full page is followed at once
// by the next, so a backlog drains at request speed.
func (w *Watcher) poll(ctx context.Context) error {
	cursor, err := w.store.GetCursor(ctx, w.pool.Address)
	if err != nil {
		return err
	}
	if cursor == "" {
		// No saved position: start from the pool's latest payment, the paged
		// equivalent of streaming from "now". Starting at the beginning would
		// replay the pool's whole history.
		if cursor, err = w.latestCursor(); err != nil {
			return err
		}
	}
	slog.Info("watcher: polling", "pool", w.pool.Address, "cursor", cursor)

	for {
		page, err := w.horizon.Payments(horizonclient.OperationRequest{
			ForAccount: w.pool.Address,
			Cursor:     cursor,
			Order:      horizonclient.OrderAsc,
			Limit:      pageSize,
			Join:       "transactions", // embeds memo data on each record — no extra HTTP call
		})
		if err != nil {
			return err
		}
		for _, op := range page.Embedded.Records {
			w.handle(ctx, op)
			if ctx.Err() != nil {
				// Intake stopped mid-page; handle saved the cursor only for
				// payments it queued, so the rest are read again after restart.
				return nil
			}
			cursor = op.PagingToken()
		}
		if len(page.Embedded.Records) == pageSize {
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(pollGap):
		}
	}
}

// latestCursor returns the paging token of the pool's most recent payment, or
// "0" for a pool that has never had one.
func (w *Watcher) latestCursor() (string, error) {
	page, err := w.horizon.Payments(horizonclient.OperationRequest{
		ForAccount: w.pool.Address,
		Order:      horizonclient.OrderDesc,
		Limit:      1,
	})
	if err != nil {
		return "", err
	}
	if len(page.Embedded.Records) == 0 {
		return "0", nil
	}
	return page.Embedded.Records[0].PagingToken(), nil
}

// handle processes one payment record from Horizon.
func (w *Watcher) handle(ctx context.Context, op operations.Operation) {
	payment, ok := paymentTo(op, w.pool.Address)
	if !ok {
		// Not a payment into this pool: our own forwards and sweeps, or an
		// operation that moved nothing here. Anything that did credit the pool
		// without being a payment is flagged rather than skipped silently.
		if kind := unhandledCredit(op, w.pool.Address); kind != "" {
			metrics.UnhandledCreditsTotal.WithLabelValues(kind).Inc()
			slog.Error("watcher: pool credited by an operation that cannot be attributed to a deposit; funds need manual handling",
				"pool", w.pool.Address, "kind", kind, "op_id", op.GetBase().ID, "tx_hash", op.GetBase().TransactionHash)
		}
		w.saveCursor(ctx, op.PagingToken())
		return nil
	}

	// The transaction (with memo) is embedded via join=transactions.
	if payment.Transaction == nil {
		slog.Warn("watcher: payment has no transaction data", "op_id", payment.ID)
		w.saveCursor(ctx, op.PagingToken())
		return nil
	}

	job := forwardJob{
		txHash:   depositKey(payment.TransactionHash, payment.ID, payment.Transaction.OperationCount),
		landedAt: payment.LedgerCloseTime,
		from:     payment.From,
		amount:   payment.Amount,
		asset:    assetID(payment),
	}

	memoID, err := routingID(payment)
	if err != nil {
		// No usable tag — memo 0 matches no intent, so the forwarder sweeps it
		// to recovery.
		slog.Warn("watcher: no routing tag, dispatching to forwarder for sweep",
			"deposit", job.txHash, "memo_type", payment.Transaction.MemoType,
			"memo", payment.Transaction.Memo, "to_muxed_id", payment.ToMuxedID, "err", err)
	} else {
		job.memoID = memoID
		slog.Info("watcher: dispatching forward",
			"deposit", job.txHash, "memo_id", memoID,
			"from", payment.From, "amount", payment.Amount, "asset", job.asset)
	}

	// Record the deposit before the cursor moves past it. The in-memory queue
	// then only ever holds deposits that are already in the table: if the
	// process dies with them queued, the retry worker finds them 'pending'.
	inserted, err := w.store.InsertForward(ctx, job.txHash, job.memoID, w.pool.Address, job.from, job.amount, job.asset, job.landedAt)
	if err != nil {
		return fmt.Errorf("record deposit %s: %w", job.txHash, err)
	}
	if !inserted {
		// Recorded before: a replay after a restart, or a second instance
		// streaming this pool. If that run never finished it, the retry
		// worker does.
		slog.Warn("watcher: deposit already recorded, not dispatching again", "deposit", job.txHash)
		w.saveCursor(ctx, op.PagingToken())
		return nil
	}

	// Hand off to a worker so the stream is not blocked by submission latency.
	// The forwarder owns all retry logic and DB updates. If intake stops
	// first the cursor is not saved; the row is already there, so the retry
	// worker forwards it either way.
	if !w.dispatch(ctx, job) {
		return nil
	}
	w.saveCursor(ctx, op.PagingToken())
	return nil
}

// paymentTo returns op as a payment into pool. Path payments count: the
// embedded Payment carries the destination side — the asset and amount the pool
// actually received — so they are credited exactly like a plain payment.
func paymentTo(op operations.Operation, pool string) (operations.Payment, bool) {
	var p operations.Payment
	switch o := op.(type) {
	case operations.Payment:
		p = o
	case operations.PathPayment:
		p = o.Payment
	case operations.PathPaymentStrictSend:
		p = o.Payment
	default:
		return p, false
	}
	return p, p.To == pool
}

// unhandledCredit names an operation that moved value into pool without being
// a payment the relayer can attribute, or returns "" if op credited nothing.
func unhandledCredit(op operations.Operation, pool string) string {
	switch o := op.(type) {
	case operations.AccountMerge:
		if o.Into == pool {
			return "account_merge"
		}
	case operations.InvokeHostFunction:
		for _, c := range o.AssetBalanceChanges {
			if c.To == pool && (c.Type == "transfer" || c.Type == "mint") {
				return "contract_transfer"
			}
		}
	}
	return ""
}

var errTagConflict = errors.New("memo and muxed id name different intents")

// routingID finds the memo_id a payment is tagged with. A muxed destination
// (M-address) carries it in the address itself, which is the protocol's own
// version of "address + memo", so it wins over a memo that is not a number —
// an exchange may add its own text reference. A numeric memo that names a
// different id is a contradiction; the payment is swept rather than guessed.
func routingID(p operations.Payment) (uint64, error) {
	tx := p.Transaction
	fromMemo, memoErr := memo.ParseID(tx.MemoType, tx.Memo)
	if p.ToMuxed == "" {
		return fromMemo, memoErr
	}
	if memoErr == nil && fromMemo != p.ToMuxedID {
		return 0, fmt.Errorf("%w: memo %d, muxed id %d", errTagConflict, fromMemo, p.ToMuxedID)
	}
	return p.ToMuxedID, nil
}

// depositKey identifies one inbound payment. For a single-operation
// transaction — every deposit recorded so far — it is the transaction hash.
// A transaction can pay the pool more than once, though (an exchange batching
// withdrawals to several M-addresses), and keying on the hash alone dropped
// every payment after the first as a replay; those keys are suffixed with the
// operation's position, taken from the low 12 bits of Horizon's operation ID.
func depositKey(txHash, opID string, opCount int32) string {
	if opCount == 1 {
		return txHash
	}
	id, err := strconv.ParseInt(opID, 10, 64)
	if err != nil {
		return txHash + ":" + opID
	}
	return txHash + ":" + strconv.FormatInt(id&0xFFF, 10)
}

// saveCursor persists the SSE paging token after each handled event.
// On reconnect, stream() reads this back so we resume exactly where we left off.
func (w *Watcher) saveCursor(ctx context.Context, pagingToken string) {
	if err := w.store.UpsertCursor(ctx, w.pool.Address, pagingToken); err != nil {
		slog.Error("watcher: save cursor", "pool", w.pool.Address, "err", err)
	}
}

// assetID returns a compact asset identifier for a payment.
// "native" for XLM, "CODE:ISSUER" for issued assets.
// base.Asset (embedded in Payment) uses Type/Code/Issuer, not AssetType/AssetCode/AssetIssuer.
func assetID(p operations.Payment) string {
	if p.Asset.Type == "native" {
		return "native"
	}
	return p.Asset.Code + ":" + p.Asset.Issuer
}
