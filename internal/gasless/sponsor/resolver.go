package sponsor

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	protocol "github.com/stellar/go-stellar-sdk/protocols/rpc"
)

// StaleRecord is a submission whose outcome was never seen: still pending
// (the process died mid-flight) or unconfirmed (sent, outcome not observed).
type StaleRecord struct {
	RequestID string
	TxHash    string
	UpdatedAt time.Time
}

// Stale lists pending and unconfirmed submissions not touched for olderThan.
func (s *Store) Stale(ctx context.Context, olderThan time.Duration) ([]StaleRecord, error) {
	rows, err := s.db.Query(ctx, `
		SELECT request_id, COALESCE(tx_hash, ''), updated_at
		FROM sponsored_transactions
		WHERE status IN ('pending', 'unconfirmed')
		  AND updated_at < NOW() - make_interval(secs => $1)
		ORDER BY updated_at
		LIMIT 100`, olderThan.Seconds())
	if err != nil {
		return nil, fmt.Errorf("list stale: %w", err)
	}
	defer rows.Close()
	var out []StaleRecord
	for rows.Next() {
		var r StaleRecord
		if err := rows.Scan(&r.RequestID, &r.TxHash, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan stale: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Resolver settles stale submissions by looking their hash up, so a lost
// response or a crash never leaves a cap reservation or an idempotency key
// stuck.
type Resolver struct {
	Submitter *Submitter
	Interval  time.Duration
}

// staleAfter is how long a record sits before the resolver looks at it: past
// the submitter's own poll, so the two don't race.
const staleAfter = pollTimeout + 30*time.Second

// Run resolves every Interval until ctx is cancelled.
func (r *Resolver) Run(ctx context.Context) {
	t := time.NewTicker(r.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.Tick(ctx)
		}
	}
}

// Tick resolves one batch.
func (r *Resolver) Tick(ctx context.Context) {
	s := r.Submitter
	stale, err := s.Records.Stale(ctx, staleAfter)
	if err != nil {
		slog.Error("sponsor resolver: list stale", "err", err)
		return
	}
	for _, rec := range stale {
		o, done := r.resolve(ctx, rec)
		if !done {
			continue
		}
		if _, err := s.Records.Finish(ctx, rec.RequestID, o); err != nil {
			slog.Error("sponsor resolver: record outcome", "request_id", rec.RequestID, "err", err)
			continue
		}
		slog.Info("sponsor resolver: settled", "request_id", rec.RequestID, "status", o.Status)
	}
}

// resolve decides a stale record's outcome. done=false leaves it for the
// next tick.
func (r *Resolver) resolve(ctx context.Context, rec StaleRecord) (Outcome, bool) {
	if rec.TxHash == "" {
		// Nothing was ever signed: the process stopped before sending.
		return Outcome{Status: StatusRejected, ErrorCode: "abandoned", ErrorMessage: "stopped before sending; nothing was charged"}, true
	}
	resp, err := r.Submitter.RPC.GetTransaction(ctx, protocol.GetTransactionRequest{Hash: rec.TxHash})
	if err != nil {
		slog.Warn("sponsor resolver: look up", "request_id", rec.RequestID, "err", err)
		return Outcome{}, false
	}
	if resp.Status == protocol.TransactionStatusNotFound {
		// Every transaction expires txValidity after it is built. Past that
		// (plus a ledger's margin) it can never land.
		if time.Since(rec.UpdatedAt) > txValidity+ledgerTime {
			return Outcome{Status: StatusRejected, TxHash: rec.TxHash, ErrorCode: "expired",
				ErrorMessage: "never landed before its time bounds expired; nothing was charged"}, true
		}
		return Outcome{}, false
	}
	o := landed(rec.TxHash, resp)
	return o, o.Status != StatusUnconfirmed
}
