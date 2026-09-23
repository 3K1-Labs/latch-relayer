package lifecycle

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestDrainWaitsForWork(t *testing.T) {
	tr := NewTracker()
	var finished atomic.Bool
	tr.Go(func(ctx context.Context) {
		time.Sleep(20 * time.Millisecond)
		if ctx.Err() == nil {
			finished.Store(true)
		}
	})

	if !tr.Drain(time.Second) {
		t.Fatal("drain reported timeout for work that finishes quickly")
	}
	if !finished.Load() {
		t.Fatal("work saw a cancelled context before finishing")
	}
}

func TestDrainCancelsStragglers(t *testing.T) {
	tr := NewTracker()
	var cancelled atomic.Bool
	tr.Go(func(ctx context.Context) {
		<-ctx.Done()
		cancelled.Store(true)
	})

	start := time.Now()
	if tr.Drain(30 * time.Millisecond) {
		t.Fatal("drain should report timeout when work outlives the deadline")
	}
	if !cancelled.Load() {
		t.Fatal("straggler was not cancelled")
	}
	if time.Since(start) > time.Second {
		t.Fatal("drain did not respect its timeout")
	}
}
