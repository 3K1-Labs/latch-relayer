package forwarder

import (
	"fmt"
	"sync"

	"github.com/stellar/go-stellar-sdk/clients/horizonclient"
)

// sequencer hands out transaction sequence numbers for the pool accounts.
//
// Why this exists: Horizon's AccountDetail reports the account state as of the
// last closed ledger, roughly every five seconds. Every forward that fetched
// its own sequence inside that window therefore read the same number and built
// the same sequence, so exactly one could win and the rest died with txBadSeq,
// fell into pending_retry, and drained one-per-ledger from the retry worker.
// Measured on testnet that capped the pipeline at about 13 credits/minute
// regardless of how many deposits arrived.
//
// Instead the sequence is read from Horizon once per pool and then advanced in
// memory, so a burst of forwards gets consecutive numbers and can settle
// together rather than colliding.
//
// Every submitter on a pool account must draw from here — the forward path, the
// recovery sweep, and the retry worker all sign as the same account, so one of
// them fetching independently would reintroduce exactly the collision this
// removes.
type sequencer struct {
	horizon horizonClient

	mu    sync.Mutex
	pools map[string]*poolSequence
}

type poolSequence struct {
	mu     sync.Mutex
	next   int64 // next transaction sequence to hand out
	loaded bool

	// send serialises the build-and-send window for this pool. Sequence numbers
	// only work if the transactions carrying them actually reach the network in
	// order: submitting N+1 before N gets N+1 rejected outright, because Stellar
	// validates against the account's current sequence. Holding this across draw,
	// simulate, sign and send keeps them ordered.
	//
	// Deliberately not held across the confirmation poll. Polling is the slow
	// part — up to a minute — and it does not touch the sequence, so keeping it
	// outside is what lets many forwards be in flight while sends stay ordered.
	send sync.Mutex
}

func newSequencer(hz horizonClient) *sequencer {
	return &sequencer{horizon: hz, pools: make(map[string]*poolSequence)}
}

func (s *sequencer) forPool(address string) *poolSequence {
	s.mu.Lock()
	defer s.mu.Unlock()
	ps, ok := s.pools[address]
	if !ok {
		ps = &poolSequence{}
		s.pools[address] = ps
	}
	return ps
}

// next returns the sequence number the caller's transaction must carry, seeding
// from Horizon on first use.
//
// The caller owns that number: it must either land it on-chain or call resync,
// because a number handed out and never used leaves a gap, and every later
// transaction on the account is invalid until the gap is repaired.
func (s *sequencer) next(address string) (int64, error) {
	ps := s.forPool(address)
	ps.mu.Lock()
	defer ps.mu.Unlock()

	if !ps.loaded {
		if err := s.seedLocked(address, ps); err != nil {
			return 0, err
		}
	}
	seq := ps.next
	ps.next++
	return seq, nil
}

// resync drops the in-memory position and reloads from Horizon. Called whenever
// a submission fails, since a failed transaction leaves its number unused and
// every subsequent one stranded behind the gap.
func (s *sequencer) resync(address string) {
	ps := s.forPool(address)
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.loaded = false
}

func (s *sequencer) seedLocked(address string, ps *poolSequence) error {
	acct, err := s.horizon.AccountDetail(horizonclient.AccountRequest{AccountID: address})
	if err != nil {
		return fmt.Errorf("fetch pool account %s: %w", address, err)
	}
	current, err := acct.GetSequenceNumber()
	if err != nil {
		return fmt.Errorf("parse sequence for %s: %w", address, err)
	}
	// GetSequenceNumber reports the last sequence the account used, so the next
	// transaction takes the one after it.
	ps.next = current + 1
	ps.loaded = true
	return nil
}

// lockSend serialises the ordered build-and-send window for a pool, returning
// the unlock function. Callers must release it before polling for confirmation.
func (s *sequencer) lockSend(address string) func() {
	ps := s.forPool(address)
	ps.send.Lock()
	return ps.send.Unlock
}
