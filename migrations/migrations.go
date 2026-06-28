package migrations

import (
	"context"
	_ "embed"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed 001_init.up.sql
var initSQL string

// Run applies all migrations against the given pool.
// Safe to call on every startup — all statements use IF NOT EXISTS.
func Run(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, initSQL); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}
	return nil
}
