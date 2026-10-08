package sponsor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Status is a submission's state, as stored.
type Status string

const (
	StatusPending     Status = "pending"
	StatusSuccess     Status = "success"
	StatusFailed      Status = "failed"
	StatusUnconfirmed Status = "unconfirmed"
	StatusRejected    Status = "rejected"
)

// Final reports whether s will not change again.
func (s Status) Final() bool {
	return s == StatusSuccess || s == StatusFailed || s == StatusRejected
}

// Record is one submission's stored state: what latch-api gets back.
type Record struct {
	RequestID         string    `json:"request_id"`
	Wallet            string    `json:"wallet"`
	Mode              Mode      `json:"mode"`
	Status            Status    `json:"status"`
	TxHash            string    `json:"tx_hash,omitempty"`
	FeeChargedStroops *int64    `json:"fee_charged_stroops,omitempty"`
	ErrorCode         string    `json:"error_code,omitempty"`
	ErrorMessage      string    `json:"error_message,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`

	payloadHash string
}

// Limits are the sponsorship caps.
type Limits struct {
	MaxTxPerWallet      int   // sponsored transactions per wallet, ever
	MaxStroopsPerWallet int64 // sponsored spend per wallet, ever
	MaxStroopsPerDay    int64 // sponsored spend across all wallets, rolling 24h
}

var (
	// ErrConflict: request_id was already used for a different body.
	ErrConflict = errors.New("request_id was already used for a different transaction")
	// ErrWalletCap: the wallet has used its sponsorship allowance.
	ErrWalletCap = errors.New("wallet sponsorship cap reached")
	// ErrDailyBudget: Latch's daily sponsorship budget is spent.
	ErrDailyBudget = errors.New("daily sponsorship budget reached")
)

// Store keeps sponsored_transactions.
type Store struct{ db *pgxpool.Pool }

func NewStore(db *pgxpool.Pool) *Store { return &Store{db: db} }

const recordColumns = `request_id, payload_hash, wallet, mode, status, COALESCE(tx_hash, ''),
	fee_charged_stroops, COALESCE(error_code, ''), COALESCE(error_message, ''), created_at, updated_at`

func scanRecord(row pgx.Row) (Record, error) {
	var r Record
	err := row.Scan(&r.RequestID, &r.payloadHash, &r.Wallet, &r.Mode, &r.Status, &r.TxHash,
		&r.FeeChargedStroops, &r.ErrorCode, &r.ErrorMessage, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}

// Get returns the record for requestID, or found=false.
func (s *Store) Get(ctx context.Context, requestID string) (Record, bool, error) {
	r, err := scanRecord(s.db.QueryRow(ctx, `SELECT `+recordColumns+`
		FROM sponsored_transactions WHERE request_id = $1`, requestID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, fmt.Errorf("get sponsored transaction: %w", err)
	}
	return r, true, nil
}

// Reservation is what Reserve needs to know about a new submission.
type Reservation struct {
	RequestID     string
	PayloadHash   string
	Wallet        string
	Mode          Mode
	Contract      string
	Function      string
	MaxFeeStroops int64
}

// spendSQL is what a row counts against a cap: the charged fee once known,
// else the reservation. Rejected rows never sent anything and count nothing.
const spendSQL = `COALESCE(SUM(COALESCE(fee_charged_stroops, fee_reserved_stroops)), 0)`

// Reserve records a new pending submission if it fits every cap. If
// request_id exists with the same body it returns that record and
// existing=true; with a different body, ErrConflict.
//
// The checks and the insert run under one advisory lock, so concurrent
// submissions — across instances — can't overrun a cap together.
func (s *Store) Reserve(ctx context.Context, res Reservation, lim Limits) (rec Record, existing bool, err error) {
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('sponsored_transactions'))`); err != nil {
			return fmt.Errorf("lock: %w", err)
		}

		prev, err := scanRecord(tx.QueryRow(ctx, `SELECT `+recordColumns+`
			FROM sponsored_transactions WHERE request_id = $1`, res.RequestID))
		switch {
		case err == nil:
			if prev.payloadHash != res.PayloadHash {
				return ErrConflict
			}
			rec, existing = prev, true
			return nil
		case !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("look up request: %w", err)
		}

		var count int
		var walletSpend int64
		if err := tx.QueryRow(ctx, `SELECT count(*), `+spendSQL+`
			FROM sponsored_transactions
			WHERE wallet = $1 AND mode = $2 AND status <> 'rejected'`,
			res.Wallet, res.Mode).Scan(&count, &walletSpend); err != nil {
			return fmt.Errorf("wallet usage: %w", err)
		}
		if count >= lim.MaxTxPerWallet || walletSpend+res.MaxFeeStroops > lim.MaxStroopsPerWallet {
			return ErrWalletCap
		}

		var daySpend int64
		if err := tx.QueryRow(ctx, `SELECT `+spendSQL+`
			FROM sponsored_transactions
			WHERE mode = $1 AND status <> 'rejected' AND created_at > NOW() - INTERVAL '24 hours'`,
			res.Mode).Scan(&daySpend); err != nil {
			return fmt.Errorf("daily usage: %w", err)
		}
		if daySpend+res.MaxFeeStroops > lim.MaxStroopsPerDay {
			return ErrDailyBudget
		}

		rec, err = scanRecord(tx.QueryRow(ctx, `
			INSERT INTO sponsored_transactions
				(request_id, payload_hash, wallet, mode, target_contract, target_function, fee_reserved_stroops)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			RETURNING `+recordColumns,
			res.RequestID, res.PayloadHash, res.Wallet, res.Mode, res.Contract, res.Function, res.MaxFeeStroops))
		if err != nil {
			return fmt.Errorf("insert sponsored transaction: %w", err)
		}
		return nil
	})
	return rec, existing, err
}

// Outcome is how a submission ended (or where it stands).
type Outcome struct {
	Status            Status
	TxHash            string
	FeeChargedStroops *int64
	ErrorCode         string
	ErrorMessage      string
}

// Finish records an outcome. A final record is never changed again.
func (s *Store) Finish(ctx context.Context, requestID string, o Outcome) (Record, error) {
	rec, err := scanRecord(s.db.QueryRow(ctx, `
		UPDATE sponsored_transactions SET
			status              = $2,
			tx_hash             = COALESCE(NULLIF($3, ''), tx_hash),
			fee_charged_stroops = COALESCE($4, fee_charged_stroops),
			error_code          = NULLIF($5, ''),
			error_message       = NULLIF($6, ''),
			updated_at          = NOW()
		WHERE request_id = $1 AND status NOT IN ('success', 'failed', 'rejected')
		RETURNING `+recordColumns,
		requestID, o.Status, o.TxHash, o.FeeChargedStroops, o.ErrorCode, o.ErrorMessage))
	if errors.Is(err, pgx.ErrNoRows) {
		// Already final (or unknown): return what is stored.
		r, found, gerr := s.Get(ctx, requestID)
		if gerr != nil {
			return Record{}, gerr
		}
		if !found {
			return Record{}, fmt.Errorf("finish %s: no such request", requestID)
		}
		return r, nil
	}
	if err != nil {
		return Record{}, fmt.Errorf("finish sponsored transaction: %w", err)
	}
	return rec, nil
}

// SetHash records the hash of a transaction about to be sent, so a crash or a
// lost response can be resolved by looking it up instead of paying again.
func (s *Store) SetHash(ctx context.Context, requestID, hash string) error {
	_, err := s.db.Exec(ctx, `UPDATE sponsored_transactions SET tx_hash = $2, updated_at = NOW()
		WHERE request_id = $1 AND status = 'pending'`, requestID, hash)
	if err != nil {
		return fmt.Errorf("record hash: %w", err)
	}
	return nil
}
