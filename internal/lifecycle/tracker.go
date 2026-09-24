// Package lifecycle lets in-flight background work outlive the shutdown
// signal long enough to finish.
package lifecycle

import (
	"context"
	"sync"
	"time"
)

// Tracker runs units of work (a deposit forward, later a sponsored-tx
// confirmation) on a context that is NOT cancelled by the shutdown signal, so
// a transaction already submitted to the network is followed to completion
// instead of being abandoned mid-flight. Cancel only fires once the drain
// deadline passes.
type Tracker struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func NewTracker() *Tracker {
	ctx, cancel := context.WithCancel(context.Background())
	return &Tracker{ctx: ctx, cancel: cancel}
}

// Go runs f in a new goroutine with the tracker's context.
func (t *Tracker) Go(f func(ctx context.Context)) {
	t.wg.Go(func() { f(t.ctx) })
}

// Drain waits up to timeout for tracked work, then cancels whatever is left
// and waits for it to return. Reports whether everything finished in time.
func (t *Tracker) Drain(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() { t.wg.Wait(); close(done) }()

	select {
	case <-done:
		t.cancel()
		return true
	case <-time.After(timeout):
		t.cancel()
		<-done
		return false
	}
}
