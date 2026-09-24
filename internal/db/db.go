package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PoolOptions tunes the connection pool. Zero values keep pgx's defaults.
type PoolOptions struct {
	MaxConns int32
	MinConns int32
}

// Connect opens a pgxpool connection to the given PostgreSQL URL, pings it,
// and returns the pool. Caller is responsible for calling pool.Close() on shutdown.
func Connect(ctx context.Context, databaseURL string, opts PoolOptions) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	// pgx's default max is max(4, NumCPU), which a small container pins at 4 —
	// too few once HTTP handlers, watchers and workers all hit the DB at once.
	if opts.MaxConns > 0 {
		cfg.MaxConns = opts.MaxConns
	}
	if opts.MinConns > 0 {
		cfg.MinConns = opts.MinConns
	}
	// Recycle connections so a managed Postgres (Neon) failover or pooler
	// restart doesn't leave the pool holding dead sockets indefinitely.
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}
