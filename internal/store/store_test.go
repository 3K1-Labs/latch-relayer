package store

import (
	"context"
	"testing"
	"time"

	"github.com/latch/relayer/internal/testdb"
	"github.com/latch/relayer/migrations"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	pool := testdb.New(t)
	if err := migrations.Run(context.Background(), pool, migrations.Deposit); err != nil {
		t.Fatal(err)
	}
	return New(pool)
}

// An in-flight transfer must survive a restart: the retry worker reads it back
// from GetPendingRetries and resolves it instead of paying again (#50).
func TestSubmissionRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if _, err := s.InsertForward(ctx, "in-1", 7, "GPOOL", "GFROM", "10.0000000", "native"); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(2 * time.Minute).Truncate(time.Second)
	if err := s.RecordSubmission(ctx, "in-1", "out-1", until); err != nil {
		t.Fatal(err)
	}
	if err := s.RequeueForContention(ctx, "in-1", "awaiting out-1"); err != nil {
		t.Fatal(err)
	}

	got := pendingForward(t, s, "in-1")
	if got.SubmittedTx == nil || *got.SubmittedTx != "out-1" {
		t.Fatalf("submitted_tx = %v, want out-1", got.SubmittedTx)
	}
	if got.SubmittedUntil == nil || !got.SubmittedUntil.Equal(until) {
		t.Fatalf("submitted_until = %v, want %v", got.SubmittedUntil, until)
	}
	if got.ForwardTx != nil {
		t.Fatalf("forward_tx = %v before the transfer settled", *got.ForwardTx)
	}

	if err := s.ClearSubmission(ctx, "in-1"); err != nil {
		t.Fatal(err)
	}
	got = pendingForward(t, s, "in-1")
	if got.SubmittedTx != nil || got.SubmittedUntil != nil {
		t.Fatalf("submission not cleared: %v %v", got.SubmittedTx, got.SubmittedUntil)
	}
}

func TestMarkForwardDoneClearsSubmission(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if _, err := s.InsertForward(ctx, "in-2", 8, "GPOOL", "GFROM", "1.0000000", "native"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordSubmission(ctx, "in-2", "out-2", time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkForwardDone(ctx, "in-2", "out-2"); err != nil {
		t.Fatal(err)
	}

	fwds, err := s.GetForwardByMemoID(ctx, 8)
	if err != nil || len(fwds) != 1 {
		t.Fatalf("forwards = %v, err = %v", fwds, err)
	}
	f := fwds[0]
	if f.Status != StatusDone || f.ForwardTx == nil || *f.ForwardTx != "out-2" {
		t.Fatalf("status=%s forward_tx=%v", f.Status, f.ForwardTx)
	}
	if f.SubmittedTx != nil || f.SubmittedUntil != nil {
		t.Fatalf("done forward still has an in-flight transfer: %v %v", f.SubmittedTx, f.SubmittedUntil)
	}
}

func pendingForward(t *testing.T, s *Store, txHash string) Forward {
	t.Helper()
	fwds, err := s.GetPendingRetries(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fwds {
		if f.TxHash == txHash {
			return f
		}
	}
	t.Fatalf("%s not in pending retries", txHash)
	return Forward{}
}
