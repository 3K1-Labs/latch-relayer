// Package channels leases channel accounts to in-flight sponsored transactions.
//
// Stellar accepts one pending transaction per source account, so each channel
// carries at most one transaction at a time and throughput scales with the
// number of channels. Leases live in Postgres (not process memory), so any
// number of gasless instances can share one pool: FOR UPDATE SKIP LOCKED
// guarantees two acquirers never get the same channel, and a lease that
// outlives its holder (crash, lost network) expires and is reclaimed.
package channels

import (
	"context"
	crand "crypto/rand" // aliased: math/rand/v2 is also imported as rand
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/latch/relayer/internal/gasless/keys"
)

var (
	// ErrPoolCapacity means every active channel is leased. Callers should
	// answer "retry later" rather than queue: the user's signature expires.
	ErrPoolCapacity = errors.New("all channels are busy")
	// ErrLeaseLost means the lease expired and another holder took the
	// channel; the caller must not record anything against it.
	ErrLeaseLost = errors.New("channel lease lost")
)

// Pool is the Postgres-backed channel lease table.
type Pool struct {
	db       *pgxpool.Pool
	instance string
}

func NewPool(db *pgxpool.Pool, instance string) *Pool {
	return &Pool{db: db, instance: instance}
}

// Lease is exclusive use of one channel until Release or expiry.
type Lease struct {
	ChannelID int
	Index     int
	Address   string
	Token     string
	// Seq is the last sequence number known consumed; valid only when
	// NeedsResync is false. Build the next transaction with Seq+1.
	Seq         int64
	NeedsResync bool
}

// Sync makes the table match the configured channels: inserts new indexes,
// retires indexes beyond the configured count, and re-activates retired ones
// that are configured again. A changed address at an index (a new
// CHANNEL_SEED) resets that row's sequence and status.
func (p *Pool) Sync(ctx context.Context, chans []keys.Channel) error {
	return pgx.BeginFunc(ctx, p.db, func(tx pgx.Tx) error {
		for _, ch := range chans {
			if _, err := tx.Exec(ctx, `
				INSERT INTO channel_accounts (hd_index, address) VALUES ($1, $2)
				ON CONFLICT (hd_index) DO UPDATE SET
					address         = EXCLUDED.address,
					status          = CASE WHEN channel_accounts.address <> EXCLUDED.address
					                         OR channel_accounts.status = 'retired'
					                       THEN 'active' ELSE channel_accounts.status END,
					disabled_reason = CASE WHEN channel_accounts.address <> EXCLUDED.address
					                       THEN NULL ELSE channel_accounts.disabled_reason END,
					seq             = CASE WHEN channel_accounts.address <> EXCLUDED.address
					                       THEN NULL ELSE channel_accounts.seq END,
					needs_resync    = channel_accounts.needs_resync
					                  OR channel_accounts.address <> EXCLUDED.address,
					updated_at      = NOW()`,
				ch.Index, ch.Address()); err != nil {
				return fmt.Errorf("sync channel %d: %w", ch.Index, err)
			}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE channel_accounts SET status = 'retired', updated_at = NOW()
			WHERE hd_index >= $1 AND status <> 'retired'`, len(chans)); err != nil {
			return fmt.Errorf("retire channels: %w", err)
		}
		return nil
	})
}

// Acquire leases the least-recently-used active channel for ttl, or returns
// ErrPoolCapacity if none is free. Taking over an expired lease forces a
// sequence resync: the previous holder may have consumed a sequence number.
func (p *Pool) Acquire(ctx context.Context, ttl time.Duration) (*Lease, error) {
	token, err := newToken()
	if err != nil {
		return nil, err
	}
	l := &Lease{Token: token}
	var seq *int64
	err = p.db.QueryRow(ctx, `
		WITH pick AS (
			SELECT id, lease_expires_at AS prev_expiry
			FROM channel_accounts
			WHERE status = 'active'
			  AND (lease_token IS NULL OR lease_expires_at < NOW())
			ORDER BY last_used_at NULLS FIRST, hd_index
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		UPDATE channel_accounts c SET
			lease_token      = $1,
			leased_by        = $2,
			lease_expires_at = NOW() + make_interval(secs => $3),
			last_used_at     = NOW(),
			needs_resync     = c.needs_resync OR pick.prev_expiry IS NOT NULL,
			updated_at       = NOW()
		FROM pick
		WHERE c.id = pick.id
		RETURNING c.id, c.hd_index, c.address, c.seq, c.needs_resync`,
		token, p.instance, ttl.Seconds(),
	).Scan(&l.ChannelID, &l.Index, &l.Address, &seq, &l.NeedsResync)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrPoolCapacity
	}
	if err != nil {
		return nil, fmt.Errorf("acquire channel: %w", err)
	}
	if seq == nil {
		l.NeedsResync = true
	} else {
		l.Seq = *seq
	}
	return l, nil
}

// AcquireWait retries Acquire until a channel frees up or wait elapses.
// Retries are jittered so many waiting requests don't poll in lockstep.
func (p *Pool) AcquireWait(ctx context.Context, ttl, wait time.Duration) (*Lease, error) {
	deadline := time.Now().Add(wait)
	for {
		l, err := p.Acquire(ctx, ttl)
		if !errors.Is(err, ErrPoolCapacity) || time.Now().After(deadline) {
			return l, err
		}
		pause := 50*time.Millisecond + rand.N(200*time.Millisecond)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pause):
		}
	}
}

// Release returns the channel to the pool. seq, when non-nil, records the
// sequence number the finished transaction consumed; resync forces the next
// holder to reload it from the network (outcome unknown, or tx never landed).
func (p *Pool) Release(ctx context.Context, l *Lease, seq *int64, resync bool) error {
	tag, err := p.db.Exec(ctx, `
		UPDATE channel_accounts SET
			lease_token = NULL, leased_by = NULL, lease_expires_at = NULL,
			seq = COALESCE($3, seq), needs_resync = $4, updated_at = NOW()
		WHERE id = $1 AND lease_token = $2`,
		l.ChannelID, l.Token, seq, resync)
	if err != nil {
		return fmt.Errorf("release channel %d: %w", l.Index, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	return nil
}

// RecordBalance stores a channel's balance and moves it in or out of rotation:
// disabled with reason when missing on-chain (balance < 0) or under min,
// re-activated once healthy. Retired channels are left alone.
func (p *Pool) RecordBalance(ctx context.Context, index int, balanceStroops, minStroops int64) error {
	_, err := p.db.Exec(ctx, `
		UPDATE channel_accounts SET
			balance_stroops    = CASE WHEN $2 < 0 THEN NULL ELSE $2 END,
			balance_checked_at = NOW(),
			status = CASE
				WHEN status = 'retired' THEN status
				WHEN $2 < 0 OR $2 < $3 THEN 'disabled'
				ELSE 'active' END,
			disabled_reason = CASE
				WHEN status = 'retired' THEN disabled_reason
				WHEN $2 < 0 THEN 'account not found on network'
				WHEN $2 < $3 THEN 'balance below CHANNEL_MIN_XLM'
				ELSE NULL END,
			updated_at = NOW()
		WHERE hd_index = $1`, index, balanceStroops, minStroops)
	if err != nil {
		return fmt.Errorf("record channel %d balance: %w", index, err)
	}
	return nil
}

// Stats counts channels for metrics and /health.
type Stats struct {
	Active   int `json:"active"` // includes leased
	Leased   int `json:"leased"`
	Disabled int `json:"disabled"`
	Retired  int `json:"retired"`
}

func (p *Pool) Stats(ctx context.Context) (Stats, error) {
	var s Stats
	err := p.db.QueryRow(ctx, `
		SELECT
			count(*) FILTER (WHERE status = 'active'),
			count(*) FILTER (WHERE status = 'active' AND lease_token IS NOT NULL AND lease_expires_at >= NOW()),
			count(*) FILTER (WHERE status = 'disabled'),
			count(*) FILTER (WHERE status = 'retired')
		FROM channel_accounts`).Scan(&s.Active, &s.Leased, &s.Disabled, &s.Retired)
	if err != nil {
		return s, fmt.Errorf("channel stats: %w", err)
	}
	return s, nil
}

func newToken() (string, error) {
	var b [16]byte
	if _, err := crand.Read(b[:]); err != nil {
		return "", fmt.Errorf("lease token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
