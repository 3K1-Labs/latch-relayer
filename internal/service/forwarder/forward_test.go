package forwarder

import (
	"context"
	"log/slog"
	"time"
)

// Forward records a deposit and forwards it: what the watcher does, in one
// call. In production the watcher inserts the row itself before saving its
// cursor and its workers call ForwardRecorded; the tests use this to drive
// that same path.
func (f *Forwarder) Forward(ctx context.Context, poolAddress, txHash string, memoID uint64, fromAddress, amount, asset string, landedAt time.Time) {
	landed := landedAt
	if landed.IsZero() {
		landed = time.Now()
	}
	inserted, err := f.store.InsertForward(ctx, txHash, memoID, poolAddress, fromAddress, amount, asset, landed)
	if err != nil {
		slog.Error("forwarder: insert forward", "tx_hash", txHash, "err", err)
		return
	}
	// A row already existed: this payment was recorded before, by a run that
	// died before saving the SSE cursor or by a second instance streaming the
	// same pool. Forwarding again could pay twice; if that earlier run never
	// finished it, the retry worker picks the row up.
	if !inserted {
		slog.Warn("forwarder: duplicate tx_hash, already dispatched — skipping",
			"tx_hash", txHash, "memo_id", memoID)
		return
	}
	f.ForwardRecorded(ctx, poolAddress, txHash, memoID, fromAddress, amount, asset, landed)
}
