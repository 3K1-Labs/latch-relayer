package watcher

import (
	"context"
	"log/slog"
	"time"

	"github.com/stellar/go-stellar-sdk/clients/horizonclient"
	"github.com/stellar/go-stellar-sdk/protocols/horizon/operations"

	"github.com/latch/relayer/internal/config"
	"github.com/latch/relayer/internal/lifecycle"
	"github.com/latch/relayer/internal/memo"
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
	payment, ok := op.(operations.Payment)
	if !ok {
		// Not a simple payment (could be path payment, etc.) — save cursor and skip.
		w.saveCursor(ctx, op.PagingToken())
		return
	}

	// Only process payments arriving at our pool address.
	if payment.To != w.pool.Address {
		w.saveCursor(ctx, op.PagingToken())
		return
	}

	// The transaction (with memo) is embedded via join=transactions.
	if payment.Transaction == nil {
		slog.Warn("watcher: payment has no transaction data", "op_id", payment.ID)
		w.saveCursor(ctx, op.PagingToken())
		return
	}
	tx := payment.Transaction

	job := forwardJob{
		txHash: payment.TransactionHash,
		from:   payment.From,
		amount: payment.Amount,
		asset:  assetID(payment),
	}

	// Parse the memo — MEMO_ID or a numeric MEMO_TEXT.
	memoID, err := memo.ParseID(tx.MemoType, tx.Memo)
	if err != nil {
		// No memo or wrong type — memo 0 matches no intent, so the forwarder
		// sweeps it to recovery.
		slog.Warn("watcher: invalid memo, dispatching to forwarder for sweep",
			"tx_hash", payment.TransactionHash, "memo_type", tx.MemoType, "memo", tx.Memo)
	} else {
		job.memoID = memoID
		slog.Info("watcher: dispatching forward",
			"tx_hash", payment.TransactionHash, "memo_id", memoID,
			"from", payment.From, "amount", payment.Amount)
	}

	// Hand off to a worker so the stream is not blocked by submission latency.
	// The forwarder owns all retry logic and DB updates.
	if !w.dispatch(ctx, job) {
		return
	}
	w.saveCursor(ctx, op.PagingToken())
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
