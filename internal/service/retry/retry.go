package retry

import (
	"context"
	"log/slog"
	"time"

	"github.com/latch/relayer/internal/service/forwarder"
	"github.com/latch/relayer/internal/store"
)

// Worker polls the DB every interval for pending_retry forwards and retries them.
// It runs as a background goroutine started by main and stops when ctx is cancelled.
type Worker struct {
	store     *store.Store
	forwarder *forwarder.Forwarder
	interval  time.Duration
}

func NewWorker(st *store.Store, fwd *forwarder.Forwarder, interval time.Duration) *Worker {
	return &Worker{store: st, forwarder: fwd, interval: interval}
}

// Run starts the polling loop. Call in a goroutine: go worker.Run(ctx).
func (w *Worker) Run(ctx context.Context) {
	slog.Info("retry worker: started", "interval", w.interval)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("retry worker: stopped")
			return
		case <-ticker.C:
			w.tick(ctx)
		}
	}
}

// tick fetches all pending_retry rows and retries each one.
func (w *Worker) tick(ctx context.Context) {
	forwards, err := w.store.GetPendingRetries(ctx)
	if err != nil {
		slog.Error("retry worker: fetch pending retries", "err", err)
		return
	}
	if len(forwards) == 0 {
		return
	}

	slog.Info("retry worker: retrying", "count", len(forwards))
	for _, fwd := range forwards {
		// Each retry is synchronous inside the tick — we're already background.
		// If the pool has concurrency issues under high load this can be goroutine-per-forward,
		// but for now sequential is simpler and avoids sequence number conflicts.
		w.forwarder.Retry(ctx, fwd)
	}
}
