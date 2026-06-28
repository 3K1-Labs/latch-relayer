package store

import (
	"context"
	"fmt"
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

// Registration is a row from the registrations table.
type Registration struct {
	MemoID      uint64
	CAddress    string
	PoolAddress string
	CreatedAt   time.Time
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

// ── Registrations ────────────────────────────────────────────────────────────

// RegisterAccount inserts a new memo_id → c_address mapping.
// Returns an error if the memo_id or c_address already exists.
func (s *Store) RegisterAccount(ctx context.Context, memoID uint64, cAddress, poolAddress string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO registrations (memo_id, c_address, pool_address)
		VALUES ($1, $2, $3)
	`, int64(memoID), cAddress, poolAddress)
	if err != nil {
		return fmt.Errorf("register account: %w", err)
	}
	return nil
}

// GetRegistration returns the registration for a given memo_id, or pgx.ErrNoRows.
func (s *Store) GetRegistration(ctx context.Context, memoID uint64) (*Registration, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT memo_id, c_address, pool_address, created_at
		FROM registrations WHERE memo_id = $1
	`, int64(memoID))

	var r Registration
	var rawID int64
	if err := row.Scan(&rawID, &r.CAddress, &r.PoolAddress, &r.CreatedAt); err != nil {
		return nil, err // pgx.ErrNoRows flows through as-is
	}
	r.MemoID = uint64(rawID)
	return &r, nil
}

// ── Forwards ─────────────────────────────────────────────────────────────────

// InsertForward records an inbound payment seen by the watcher.
// Uses ON CONFLICT DO NOTHING so replaying the same tx_hash is safe.
func (s *Store) InsertForward(ctx context.Context, txHash string, memoID uint64, fromAddress, amount, asset string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO forwards (tx_hash, memo_id, from_address, amount, asset)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (tx_hash) DO NOTHING
	`, txHash, int64(memoID), fromAddress, amount, asset)
	if err != nil {
		return fmt.Errorf("insert forward: %w", err)
	}
	return nil
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

// MarkForwardFailed sets the status and records the error message.
// If retries < 3 the caller passes StatusPendingRetry; after 3 it passes StatusFailed.
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

// GetPendingRetries returns all forwards that need to be retried by the background worker.
func (s *Store) GetPendingRetries(ctx context.Context) ([]Forward, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, tx_hash, memo_id, from_address, amount, asset,
		       forward_tx, status, retries, error, created_at, updated_at
		FROM forwards WHERE status = $1
		ORDER BY created_at ASC
	`, StatusPendingRetry)
	if err != nil {
		return nil, fmt.Errorf("get pending retries: %w", err)
	}
	defer rows.Close()
	return scanForwards(rows)
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
