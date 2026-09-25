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

// forwardWorkers is how many forwards run at once per pool account.
//
// Previously every inbound payment got its own goroutine with no ceiling, so a
// burst of deposits produced a burst of concurrent submissions on one account.
// A bound still matters: unbounded goroutines against a rate-limited Soroban
// RPC produce TRY_AGAIN_LATER storms rather than throughput. Sends are already
// serialised per pool by the sequencer, so these workers overlap the slow part
// — simulation and the confirmation poll — and the number mainly sets how many
// forwards may sit waiting for ledger inclusion at once.
const forwardWorkers = 32

// forwardQueue bounds how many payments can wait for a worker. Horizon replays
// from the saved cursor on reconnect, so a full queue means the stream blocks
// briefly rather than events being dropped.
const forwardQueue = 256

// Watcher opens a Horizon SSE stream for one pool address and hands each inbound
// payment to a bounded pool of forwarder workers.
type Watcher struct {
	pool      config.PoolAccount
	store     *store.Store
	forwarder *forwarder.Forwarder
	horizon   *horizonclient.Client
	work      *lifecycle.Tracker

	jobs chan forwardJob
}

// forwardJob is one inbound payment waiting to be forwarded.
type forwardJob struct {
	txHash string
	memoID uint64
	from   string
	amount string
	asset  string
}

// New builds a watcher. Its workers run on work rather than the stream's
// context, so a shutdown stops new events without abandoning forwards already
// in flight or payments already queued.
func New(pool config.PoolAccount, st *store.Store, fwd *forwarder.Forwarder, hz *horizonclient.Client, work *lifecycle.Tracker) *Watcher {
	return &Watcher{
		pool:      pool,
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
	for range forwardWorkers {
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

// forward runs one job. This watcher only sees payments to its own pool, so
// that is the pool holding the money for every job it dispatches.
func (w *Watcher) forward(ctx context.Context, job forwardJob) {
	w.forwarder.Forward(ctx, w.pool.Address, job.txHash, job.memoID, job.from, job.amount, job.asset)
}

// Run starts the SSE stream and reconnects automatically on any error.
// Call in a goroutine: go watcher.Run(ctx).
func (w *Watcher) Run(ctx context.Context) {
	slog.Info("watcher: starting", "pool", w.pool.Address, "workers", forwardWorkers)
	w.startWorkers(ctx)
	for {
		if err := w.stream(ctx); err != nil {
			slog.Error("watcher: stream error, reconnecting in 5s", "pool", w.pool.Address, "err", err)
		}
		select {
		case <-ctx.Done():
			slog.Info("watcher: stopped", "pool", w.pool.Address)
			return
		case <-time.After(5 * time.Second):
		}
	}
}

// stream opens one SSE connection and processes events until an error or ctx cancel.
// On reconnect, Run calls this again from the last saved cursor so no events are missed.
func (w *Watcher) stream(ctx context.Context) error {
	cursor, err := w.store.GetCursor(ctx, w.pool.Address)
	if err != nil {
		return err
	}
	if cursor == "" {
		// "now" tells Horizon to stream only new events from this moment forward.
		// Using "0" would replay all historical payments — expensive and unnecessary.
		cursor = "now"
	}

	slog.Info("watcher: connecting", "pool", w.pool.Address, "cursor", cursor)

	req := horizonclient.OperationRequest{
		ForAccount: w.pool.Address,
		Cursor:     cursor,
		Join:       "transactions", // embeds memo data on each event — no extra HTTP call
	}

	return w.horizon.StreamPayments(ctx, req, func(op operations.Operation) {
		w.handle(ctx, op)
	})
}

// handle processes one payment event from the SSE stream.
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
		return
	}

	// The transaction (with memo) is embedded via join=transactions.
	if payment.Transaction == nil {
		slog.Warn("watcher: payment has no transaction data", "op_id", payment.ID)
		w.saveCursor(ctx, op.PagingToken())
		return
	}

	job := forwardJob{
		txHash: depositKey(payment.TransactionHash, payment.ID, payment.Transaction.OperationCount),
		from:   payment.From,
		amount: payment.Amount,
		asset:  assetID(payment),
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

	// Hand off to a worker so the stream is not blocked by submission latency.
	// The forwarder owns all retry logic and DB updates.
	if !w.dispatch(ctx, job) {
		return
	}
	w.saveCursor(ctx, op.PagingToken())
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
