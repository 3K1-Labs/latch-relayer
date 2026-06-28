package watcher

import (
	"context"
	"log/slog"
	"time"

	"github.com/stellar/go-stellar-sdk/clients/horizonclient"
	"github.com/stellar/go-stellar-sdk/protocols/horizon/operations"

	"github.com/latch/relayer/internal/config"
	"github.com/latch/relayer/internal/memo"
	"github.com/latch/relayer/internal/service/forwarder"
	"github.com/latch/relayer/internal/store"
)

// Watcher opens a Horizon SSE stream for one pool address and dispatches each
// inbound payment to the forwarder in its own goroutine.
type Watcher struct {
	pool      config.PoolAccount
	store     *store.Store
	forwarder *forwarder.Forwarder
	horizon   *horizonclient.Client
}

func New(pool config.PoolAccount, st *store.Store, fwd *forwarder.Forwarder, hz *horizonclient.Client) *Watcher {
	return &Watcher{pool: pool, store: st, forwarder: fwd, horizon: hz}
}

// Run starts the SSE stream and reconnects automatically on any error.
// Call in a goroutine: go watcher.Run(ctx).
func (w *Watcher) Run(ctx context.Context) {
	slog.Info("watcher: starting", "pool", w.pool.Address)
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

	// Parse the memo — must be MEMO_ID type.
	memoID, err := memo.ParseID(tx.MemoType, tx.Memo)
	if err != nil {
		// No memo or wrong type — sweep to recovery via the forwarder.
		slog.Warn("watcher: invalid memo, dispatching to forwarder for sweep",
			"tx_hash", payment.TransactionHash, "memo_type", tx.MemoType, "memo", tx.Memo)
		go w.forwarder.Forward(ctx,
			payment.TransactionHash, 0,
			payment.From, payment.Amount, assetID(payment),
		)
		w.saveCursor(ctx, op.PagingToken())
		return
	}

	slog.Info("watcher: dispatching forward",
		"tx_hash", payment.TransactionHash, "memo_id", memoID,
		"from", payment.From, "amount", payment.Amount)

	// Dispatch in a goroutine so the stream is never blocked.
	// The forwarder owns all retry logic and DB updates.
	go w.forwarder.Forward(ctx,
		payment.TransactionHash, memoID,
		payment.From, payment.Amount, assetID(payment),
	)

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
