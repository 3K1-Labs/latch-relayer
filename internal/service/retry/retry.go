package retry

import (
	"context"
	"github.com/latch/relayer/internal/metrics"
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

// tick expires stale intents and retries any pending_retry forwards.
func (w *Worker) tick(ctx context.Context) {
	if n, err := w.store.ExpireStaleIntents(ctx); err != nil {
		slog.Error("retry worker: expire stale intents", "err", err)
	} else if n > 0 {
		slog.Info("retry worker: expired stale intents", "count", n)
	}

	forwards, err := w.store.GetPendingRetries(ctx)
	if err != nil {
		slog.Error("retry worker: fetch pending retries", "err", err)
		return
	}
	if len(forwards) == 0 {
		metrics.PendingRetryDepth.Set(0)
		return
	}

	// Sampled here rather than continuously: this is the one place that already
	// knows the true backlog, and a rising line is the signal that deposits are
	// arriving faster than the pool accounts can forward them.
	metrics.PendingRetryDepth.Set(float64(len(forwards)))

	slog.Info("retry worker: retrying", "count", len(forwards))
	for _, fwd := range forwards {
		// Sequential to avoid Stellar sequence number conflicts under load.
		w.forwarder.Retry(ctx, fwd)
	}
}
