package store

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Status values for the forwards table.
const (
	StatusPending      = "pending"
	StatusDone         = "done"
	StatusFailed       = "failed"
	StatusPendingRetry = "pending_retry"
)

// Status values for the intents table.
const (
	IntentPending   = "pending"
	IntentCompleted = "completed"
	IntentExpired   = "expired"
	IntentFailed    = "failed"
)

// Intent is a row from the intents table.
// Each represents one funding session: a unique memo_id that routes a deposit
// to a specific C-address, valid until ExpiresAt.
type Intent struct {
	ID          string
	MemoID      uint64
	CAddress    string
	PoolAddress string
	ExpectedAmt *string
	ExpiresAt   time.Time
	Status      string
	ExternalID  *string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Forward is a row from the forwards table.
type Forward struct {
	ID          int64
	TxHash      string
	MemoID      uint64
	FromAddress string
	Amount      string
	Asset       string
	ForwardTx   *string
	Status      string
	Retries     int
	Error       *string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Store wraps a pgxpool and exposes all database operations the relayer needs.
type Store struct {
	pool *pgxpool.Pool
}

// New returns a Store backed by the given connection pool.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// ── Intents ───────────────────────────────────────────────────────────────────

// CreateIntent generates a unique random memo_id and inserts an intent row.
// Retries up to 5 times on memo_id collision (astronomically unlikely in practice).
func (s *Store) CreateIntent(ctx context.Context, cAddress, poolAddress string, expectedAmt *string, expiresAt time.Time, externalID *string) (*Intent, error) {
	for range 5 {
		// rand.Int64 returns a non-negative int64 — safe to store in BIGINT and cast to uint64.
		memoID := uint64(rand.Int64())

		var id string
		err := s.pool.QueryRow(ctx, `
			INSERT INTO intents (memo_id, c_address, pool_address, expected_amt, expires_at, external_id)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (memo_id) DO NOTHING
			RETURNING id
		`, int64(memoID), cAddress, poolAddress, expectedAmt, expiresAt, externalID).Scan(&id)

		if err == pgx.ErrNoRows {
			continue // memo_id collision, regenerate
		}
		if err != nil {
			return nil, fmt.Errorf("create intent: %w", err)
		}

		return &Intent{
			ID:          id,
			MemoID:      memoID,
			CAddress:    cAddress,
			PoolAddress: poolAddress,
			ExpectedAmt: expectedAmt,
			ExpiresAt:   expiresAt,
			Status:      IntentPending,
			ExternalID:  externalID,
		}, nil
	}
	return nil, fmt.Errorf("create intent: could not generate unique memo_id")
}

// GetIntentByMemoID returns the intent for a given memo_id, or pgx.ErrNoRows.
func (s *Store) GetIntentByMemoID(ctx context.Context, memoID uint64) (*Intent, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, memo_id, c_address, pool_address, expected_amt,
		       expires_at, status, external_id, created_at, updated_at
		FROM intents WHERE memo_id = $1
	`, int64(memoID))

	var i Intent
	var rawID int64
	if err := row.Scan(
		&i.ID, &rawID, &i.CAddress, &i.PoolAddress, &i.ExpectedAmt,
		&i.ExpiresAt, &i.Status, &i.ExternalID, &i.CreatedAt, &i.UpdatedAt,
	); err != nil {
		return nil, err
	}
	i.MemoID = uint64(rawID)
	return &i, nil
}

// ErrExternalIDConflict is returned when an intent is already bound to a
// different provider order.
var ErrExternalIDConflict = errors.New("intent already bound to a different external_id")

// SetIntentExternalID binds an on-ramp provider's order ID to an existing intent.
//
// The ID cannot be supplied at mint time — MoonPay only issues one once the user
// completes checkout, long after the widget URL was built — so it has to be
// attached afterwards by whoever receives the provider's webhook.
//
// Re-binding to a different order is refused rather than overwritten: an intent
// silently re-pointed from order A to order B would reconcile A's deposit against
// B's record. Setting the same ID twice succeeds, so a redelivered webhook is safe.
func (s *Store) SetIntentExternalID(ctx context.Context, memoID uint64, externalID string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE intents SET external_id = $1, updated_at = NOW()
		WHERE memo_id = $2 AND (external_id IS NULL OR external_id = $1)
	`, externalID, int64(memoID))
	if err != nil {
		return fmt.Errorf("set intent external_id: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}

	// No row updated: either the intent does not exist, or it is bound to another
	// order. Distinguish so the caller can answer 404 rather than 409.
	var existing *string
	if err := s.pool.QueryRow(ctx,
		`SELECT external_id FROM intents WHERE memo_id = $1`, int64(memoID),
	).Scan(&existing); err != nil {
		return err // pgx.ErrNoRows when the intent does not exist
	}
	return ErrExternalIDConflict
}

// CompleteIntent marks a pending intent as completed (deposit forwarded successfully).
func (s *Store) CompleteIntent(ctx context.Context, memoID uint64) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE intents SET status = $1, updated_at = NOW()
		WHERE memo_id = $2 AND status = 'pending'
	`, IntentCompleted, int64(memoID))
	if err != nil {
		return fmt.Errorf("complete intent: %w", err)
	}
	return nil
}

// FailIntent marks a pending intent as permanently failed (all retries exhausted).
func (s *Store) FailIntent(ctx context.Context, memoID uint64) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE intents SET status = $1, updated_at = NOW()
		WHERE memo_id = $2 AND status = 'pending'
	`, IntentFailed, int64(memoID))
	if err != nil {
		return fmt.Errorf("fail intent: %w", err)
	}
	return nil
}

// ExpireStaleIntents marks all pending intents past their expires_at as expired.
// Called by the retry worker on each tick.
func (s *Store) ExpireStaleIntents(ctx context.Context) (int64, error) {
	result, err := s.pool.Exec(ctx, `
		UPDATE intents SET status = 'expired', updated_at = NOW()
		WHERE status = 'pending' AND expires_at < NOW()
	`)
	if err != nil {
		return 0, fmt.Errorf("expire stale intents: %w", err)
	}
	return result.RowsAffected(), nil
}

// ── Forwards ─────────────────────────────────────────────────────────────────

// InsertForward records an inbound payment seen by the watcher.
//
// Returns true when this call created the row, false when one already existed
// for txHash. Callers must stop on false: ON CONFLICT DO NOTHING makes the
// insert idempotent, but it does not make the outbound transfer idempotent, and
// the same deposit reaching submit() twice pays the C-address twice.
func (s *Store) InsertForward(ctx context.Context, txHash string, memoID uint64, fromAddress, amount, asset string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO forwards (tx_hash, memo_id, from_address, amount, asset)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (tx_hash) DO NOTHING
	`, txHash, int64(memoID), fromAddress, amount, asset)
	if err != nil {
		return false, fmt.Errorf("insert forward: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// MarkForwardDone records the outbound tx hash and flips status to done.
func (s *Store) MarkForwardDone(ctx context.Context, txHash, forwardTx string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE forwards
		SET status = $1, forward_tx = $2, updated_at = NOW()
		WHERE tx_hash = $3
	`, StatusDone, forwardTx, txHash)
	if err != nil {
		return fmt.Errorf("mark forward done: %w", err)
	}
	return nil
}

// MarkForwardFailed sets the status and records the error message, incrementing retries.
// Use for transient failures that should go back into pending_retry.
func (s *Store) MarkForwardFailed(ctx context.Context, txHash, status, errMsg string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE forwards
		SET status = $1, error = $2, retries = retries + 1, updated_at = NOW()
		WHERE tx_hash = $3
	`, status, errMsg, txHash)
	if err != nil {
		return fmt.Errorf("mark forward failed: %w", err)
	}
	return nil
}

// RequeueForContention puts a forward back in the retry queue without charging
// the retry budget. Used when the failure was losing the race for the pool
// account's ledger slot rather than anything wrong with the transaction: the
// deposit is waiting its turn, not failing, and counting it would permanently
// fail good deposits precisely when the system is busiest.
func (s *Store) RequeueForContention(ctx context.Context, txHash, errMsg string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE forwards
		SET status = $1, error = $2, updated_at = NOW()
		WHERE tx_hash = $3
	`, StatusPendingRetry, errMsg, txHash)
	if err != nil {
		return fmt.Errorf("requeue for contention: %w", err)
	}
	return nil
}

// PermanentlyFail marks a forward as permanently failed without incrementing retries.
// Use when the retry ceiling is hit or a permanent error is detected, so the final
// error message is recorded cleanly without inflating the counter.
func (s *Store) PermanentlyFail(ctx context.Context, txHash, errMsg string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE forwards
		SET status = 'failed', error = $1, updated_at = NOW()
		WHERE tx_hash = $2
	`, errMsg, txHash)
	if err != nil {
		return fmt.Errorf("permanently fail forward: %w", err)
	}
	return nil
}

// GetForwardByMemoID returns all forwards for a memo_id, newest first.
func (s *Store) GetForwardByMemoID(ctx context.Context, memoID uint64) ([]Forward, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, tx_hash, memo_id, from_address, amount, asset,
		       forward_tx, status, retries, error, created_at, updated_at
		FROM forwards WHERE memo_id = $1
		ORDER BY created_at DESC
	`, int64(memoID))
	if err != nil {
		return nil, fmt.Errorf("get forwards: %w", err)
	}
	defer rows.Close()
	return scanForwards(rows)
}

// GetPendingRetries returns all forwards the background worker should attempt.
// This covers two cases:
//   - pending_retry: explicit retry queue after Forward() exhausted its in-process attempts
//   - pending older than 5 minutes: crash-recovery for forwards whose goroutine was killed
//     before submit() ran, leaving the row stuck in the initial pending state (Gap 5)
func (s *Store) GetPendingRetries(ctx context.Context) ([]Forward, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, tx_hash, memo_id, from_address, amount, asset,
		       forward_tx, status, retries, error, created_at, updated_at
		FROM forwards
		WHERE status = 'pending_retry'
		   OR (status = 'pending' AND created_at < NOW() - INTERVAL '5 minutes')
		ORDER BY retries ASC, created_at ASC
		LIMIT 200
	`)
	if err != nil {
		return nil, fmt.Errorf("get pending retries: %w", err)
	}
	defer rows.Close()
	return scanForwards(rows)
}

// Ping checks that the DB connection is alive. Used by the health endpoint.
func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

// ── Cursors ──────────────────────────────────────────────────────────────────

// GetCursor returns the last saved Horizon SSE cursor for a pool address.
// Returns "" and no error if no cursor has been saved yet.
func (s *Store) GetCursor(ctx context.Context, poolAddress string) (string, error) {
	var cursor string
	err := s.pool.QueryRow(ctx, `
		SELECT cursor FROM cursors WHERE pool_address = $1
	`, poolAddress).Scan(&cursor)
	if err == pgx.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get cursor: %w", err)
	}
	return cursor, nil
}

// UpsertCursor saves (or updates) the SSE cursor for a pool address.
func (s *Store) UpsertCursor(ctx context.Context, poolAddress, cursor string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO cursors (pool_address, cursor, updated_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (pool_address)
		DO UPDATE SET cursor = EXCLUDED.cursor, updated_at = NOW()
	`, poolAddress, cursor)
	if err != nil {
		return fmt.Errorf("upsert cursor: %w", err)
	}
	return nil
}

// ── helpers ──────────────────────────────────────────────────────────────────

func scanForwards(rows pgx.Rows) ([]Forward, error) {
	var results []Forward
	for rows.Next() {
		var f Forward
		var rawID int64
		if err := rows.Scan(
			&f.ID, &f.TxHash, &rawID, &f.FromAddress, &f.Amount, &f.Asset,
			&f.ForwardTx, &f.Status, &f.Retries, &f.Error,
			&f.CreatedAt, &f.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan forward: %w", err)
		}
		f.MemoID = uint64(rawID)
		results = append(results, f)
	}
	return results, rows.Err()
}
