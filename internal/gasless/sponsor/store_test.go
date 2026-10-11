package sponsor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/latch/relayer/internal/testdb"
	"github.com/latch/relayer/migrations"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	db := testdb.New(t)
	if err := migrations.Run(context.Background(), db, migrations.Gasless); err != nil {
		t.Fatal(err)
	}
	return NewStore(db)
}

func reservation(id, wallet string, fee int64) Reservation {
	return Reservation{RequestID: id, PayloadHash: "h-" + id, Wallet: wallet, Mode: ModeSponsored,
		Contract: "C", Function: "add_context_rule", MaxFeeStroops: fee}
}

var roomy = Limits{MaxTxPerWallet: 100, MaxStroopsPerWallet: 1 << 40, MaxStroopsPerDay: 1 << 40}

func TestStore_ReserveIsIdempotent(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	rec, existing, err := s.Reserve(ctx, reservation("req_1", "W", 100), roomy)
	if err != nil || existing || rec.Status != StatusPending {
		t.Fatalf("first: %+v existing=%v err=%v", rec, existing, err)
	}
	rec, existing, err = s.Reserve(ctx, reservation("req_1", "W", 100), roomy)
	if err != nil || !existing || rec.RequestID != "req_1" {
		t.Fatalf("repeat: %+v existing=%v err=%v", rec, existing, err)
	}
	other := reservation("req_1", "W", 100)
	other.PayloadHash = "different"
	if _, _, err := s.Reserve(ctx, other, roomy); !errors.Is(err, ErrConflict) {
		t.Fatalf("different body: err = %v", err)
	}
}

func TestStore_WalletCaps(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	lim := Limits{MaxTxPerWallet: 2, MaxStroopsPerWallet: 1_000, MaxStroopsPerDay: 1 << 40}

	if _, _, err := s.Reserve(ctx, reservation("a1", "A", 400), lim); err != nil {
		t.Fatal(err)
	}
	// Spend: 400 reserved + 700 > 1,000.
	if _, _, err := s.Reserve(ctx, reservation("a2", "A", 700), lim); !errors.Is(err, ErrWalletCap) {
		t.Fatalf("spend cap: err = %v", err)
	}
	// Once charged, the actual fee counts instead of the reservation.
	charged := int64(100)
	if _, err := s.Finish(ctx, "a1", Outcome{Status: StatusSuccess, FeeChargedStroops: &charged}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Reserve(ctx, reservation("a2", "A", 700), lim); err != nil {
		t.Fatalf("after charge settled: %v", err)
	}
	// Count: two used.
	if _, _, err := s.Reserve(ctx, reservation("a3", "A", 1), lim); !errors.Is(err, ErrWalletCap) {
		t.Fatalf("count cap: err = %v", err)
	}
	// Rejected submissions cost nothing and don't count.
	if _, err := s.Finish(ctx, "a2", Outcome{Status: StatusRejected, ErrorCode: "channels_busy"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Reserve(ctx, reservation("a3", "A", 1), lim); err != nil {
		t.Fatalf("after a rejection: %v", err)
	}
	// Another wallet is unaffected.
	if _, _, err := s.Reserve(ctx, reservation("b1", "B", 400), lim); err != nil {
		t.Fatalf("other wallet: %v", err)
	}
}

func TestStore_DailyBudgetHoldsUnderConcurrency(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	lim := Limits{MaxTxPerWallet: 100, MaxStroopsPerWallet: 1 << 40, MaxStroopsPerDay: 1_000}

	// 20 wallets race for a budget that fits 10 reservations of 100.
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := s.Reserve(ctx, reservation(fmt.Sprintf("d%02d", i), fmt.Sprintf("W%d", i), 100), lim)
			if err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			} else if !errors.Is(err, ErrDailyBudget) {
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()
	if ok != 10 {
		t.Fatalf("%d reservations fit, want exactly 10", ok)
	}
}

func TestStore_FinishIsFinal(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	if _, _, err := s.Reserve(ctx, reservation("f1", "W", 100), roomy); err != nil {
		t.Fatal(err)
	}
	if err := s.SetHash(ctx, "f1", "abc"); err != nil {
		t.Fatal(err)
	}
	fee := int64(90)
	rec, err := s.Finish(ctx, "f1", Outcome{Status: StatusSuccess, FeeChargedStroops: &fee})
	if err != nil || rec.Status != StatusSuccess || rec.TxHash != "abc" || *rec.FeeChargedStroops != 90 {
		t.Fatalf("finish: %+v err=%v", rec, err)
	}
	rec, err = s.Finish(ctx, "f1", Outcome{Status: StatusFailed, ErrorCode: "late"})
	if err != nil || rec.Status != StatusSuccess {
		t.Fatalf("a final record changed: %+v err=%v", rec, err)
	}
}

func TestStore_Stale(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	for _, id := range []string{"s1", "s2", "s3"} {
		if _, _, err := s.Reserve(ctx, reservation(id, "W", 1), roomy); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Finish(ctx, "s2", Outcome{Status: StatusUnconfirmed, TxHash: "h2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Finish(ctx, "s3", Outcome{Status: StatusSuccess}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(ctx, `UPDATE sponsored_transactions SET updated_at = NOW() - INTERVAL '10 minutes'`); err != nil {
		t.Fatal(err)
	}
	stale, err := s.Stale(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range stale {
		got[r.RequestID] = r.TxHash
	}
	if len(got) != 2 || got["s2"] != "h2" {
		t.Fatalf("stale = %+v, want s1 (pending) and s2 (unconfirmed)", stale)
	}
}
