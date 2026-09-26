package handler

import (
	"testing"
	"time"
)

type fakeStream struct {
	pool  string
	heard time.Time
}

func (f fakeStream) Pool() string         { return f.pool }
func (f fakeStream) LastHeard() time.Time { return f.heard }

// Staleness is judged on the stream, not on deposits: a pool with no traffic
// whose stream is connected stays healthy however long it has been quiet.
func TestStaleStreams(t *testing.T) {
	now := time.Now()
	h := &Handler{}
	h.WatchStreams(5*time.Minute,
		fakeStream{"A", now.Add(-10 * time.Second)},
		fakeStream{"B", now.Add(-4 * time.Minute)},
		fakeStream{"C", now.Add(-6 * time.Minute)},
		fakeStream{"D", now.Add(-90 * 24 * time.Hour)},
	)
	if got := h.staleStreams(now); got != 2 {
		t.Fatalf("staleStreams = %d, want 2 (C and D)", got)
	}
	if got := (&Handler{}).staleStreams(now); got != 0 {
		t.Fatalf("no streams watched: staleStreams = %d, want 0", got)
	}
}
