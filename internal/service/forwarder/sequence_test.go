package forwarder

import (
	"sync"
	"testing"

	"github.com/stellar/go-stellar-sdk/clients/horizonclient"
	hProtocol "github.com/stellar/go-stellar-sdk/protocols/horizon"
	"github.com/stellar/go-stellar-sdk/txnbuild"
)

// countingHorizon reports a fixed account and counts how often it was asked.
// Fixed on purpose: Horizon reports the last closed ledger, so every caller
// inside one ~5s window really does see the same sequence number.
type countingHorizon struct {
	mu    sync.Mutex
	calls int
	seq   int64
}

func (c *countingHorizon) AccountDetail(_ horizonclient.AccountRequest) (hProtocol.Account, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return hProtocol.Account{AccountID: "GPOOL", Sequence: c.seq}, nil
}

func (c *countingHorizon) SubmitTransaction(_ *txnbuild.Transaction) (hProtocol.Transaction, error) {
	return hProtocol.Transaction{}, nil
}

// The regression this package exists to prevent: concurrent forwards on one
// pool account must never be handed the same sequence number. Before the
// sequencer each caller read Horizon directly, so 50 concurrent forwards all
// built sequence N+1, one landed, and 49 died with txBadSeq — which is what
// held the pipeline at roughly 13 credits/minute on testnet.
func TestSequencer_ConcurrentCallersGetDistinctSequences(t *testing.T) {
	const n = 50
	hz := &countingHorizon{seq: 100}
	s := newSequencer(hz)

	got := make([]int64, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			seq, err := s.next("GPOOL")
			if err != nil {
				t.Errorf("next: %v", err)
				return
			}
			got[i] = seq
		}()
	}
	wg.Wait()

	seen := make(map[int64]int, n)
	for _, seq := range got {
		seen[seq]++
	}
	if len(seen) != n {
		t.Fatalf("expected %d distinct sequence numbers, got %d (duplicates: %v)", n, len(seen), duplicates(seen))
	}

	// Contiguous from the seeded value: a gap would strand every transaction
	// queued behind it until a resync.
	for want := int64(101); want < 101+n; want++ {
		if seen[want] != 1 {
			t.Fatalf("sequence %d handed out %d times, expected exactly once", want, seen[want])
		}
	}

	// One Horizon read for the whole burst, not one per caller. That collapse is
	// the entire point: the old path made n round trips and got n identical
	// answers.
	if hz.calls != 1 {
		t.Fatalf("expected 1 Horizon read for %d callers, got %d", n, hz.calls)
	}
}

// After a failed submission the number it held is unused, so everything queued
// behind the gap is invalid. resync must force a fresh read.
func TestSequencer_ResyncRereadsHorizon(t *testing.T) {
	hz := &countingHorizon{seq: 100}
	s := newSequencer(hz)

	first, err := s.next("GPOOL")
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if first != 101 {
		t.Fatalf("first sequence = %d, want 101", first)
	}

	s.resync("GPOOL")
	hz.seq = 205 // the account moved on while we were away

	again, err := s.next("GPOOL")
	if err != nil {
		t.Fatalf("next after resync: %v", err)
	}
	if again != 206 {
		t.Fatalf("sequence after resync = %d, want 206", again)
	}
	if hz.calls != 2 {
		t.Fatalf("expected 2 Horizon reads, got %d", hz.calls)
	}
}

// Pools are independent: one account resyncing must not disturb another's
// position.
func TestSequencer_PoolsAreIndependent(t *testing.T) {
	hz := &countingHorizon{seq: 100}
	s := newSequencer(hz)

	a, _ := s.next("GPOOL_A")
	b, _ := s.next("GPOOL_B")
	if a != 101 || b != 101 {
		t.Fatalf("independent pools should each start at 101, got a=%d b=%d", a, b)
	}

	a2, _ := s.next("GPOOL_A")
	if a2 != 102 {
		t.Fatalf("second draw on pool A = %d, want 102", a2)
	}
}

func duplicates(seen map[int64]int) []int64 {
	var out []int64
	for seq, n := range seen {
		if n > 1 {
			out = append(out, seq)
		}
	}
	return out
}
