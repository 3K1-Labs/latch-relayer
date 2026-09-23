// Package migrations applies each service's embedded SQL schema on startup.
//
// Every service (deposit bridge, gasless sponsor) has its own Set, and each
// should point at its own database — a set only ever creates its own tables.
package migrations

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed deposit/*.up.sql
var depositFiles embed.FS

//go:embed gasless/*.up.sql
var gaslessFiles embed.FS

// Deposit is the deposit bridge's schema (intents, forwards, cursors).
var Deposit = Set{Name: "deposit", FS: mustSub(depositFiles, "deposit")}

// Gasless is the gasless sponsor's schema (channel accounts, quotes, sponsored txs).
var Gasless = Set{Name: "gasless", FS: mustSub(gaslessFiles, "gasless")}

// Set is one service's migrations: `NNN_name.up.sql` files at the FS root.
type Set struct {
	Name string
	FS   fs.FS
}

// advisoryLockKey serializes migration runs across instances that boot at the
// same time. Arbitrary constant; only needs to be stable.
const advisoryLockKey int64 = 0x6c617463686d6967 // "latchmig"

// Migration is one embedded `.up.sql` file.
type Migration struct {
	Version string // "<set>/<file stem>", e.g. "deposit/001_init"
	SQL     string
}

// Load returns the set's migrations ordered by filename.
func (s Set) Load() ([]Migration, error) {
	names, err := fs.Glob(s.FS, "*.up.sql")
	if err != nil {
		return nil, err
	}
	sort.Strings(names)

	out := make([]Migration, 0, len(names))
	for _, name := range names {
		body, err := fs.ReadFile(s.FS, name)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		out = append(out, Migration{
			Version: s.Name + "/" + strings.TrimSuffix(name, ".up.sql"),
			SQL:     string(body),
		})
	}
	return out, nil
}

// Run applies every migration in set not yet recorded in schema_migrations,
// in one transaction, under an advisory lock so concurrent instances can't race.
//
// Databases created before versioning existed already hold the deposit
// tables; 001 uses IF NOT EXISTS throughout, so applying it again just
// records it.
func Run(ctx context.Context, pool *pgxpool.Pool, set Set) error {
	migs, err := set.Load()
	if err != nil {
		return fmt.Errorf("load %s migrations: %w", set.Name, err)
	}

	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, advisoryLockKey); err != nil {
			return fmt.Errorf("acquire migration lock: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			CREATE TABLE IF NOT EXISTS schema_migrations (
				version    TEXT        PRIMARY KEY,
				applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
			)`); err != nil {
			return fmt.Errorf("create schema_migrations: %w", err)
		}

		rows, err := tx.Query(ctx, `SELECT version FROM schema_migrations`)
		if err != nil {
			return fmt.Errorf("read schema_migrations: %w", err)
		}
		versions, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return fmt.Errorf("read schema_migrations: %w", err)
		}
		applied := make(map[string]bool, len(versions))
		for _, v := range versions {
			applied[v] = true
		}

		for _, m := range migs {
			if applied[m.Version] {
				continue
			}
			// No arguments → pgx uses the simple protocol, which allows the
			// multi-statement bodies migration files contain.
			if _, err := tx.Exec(ctx, m.SQL); err != nil {
				return fmt.Errorf("apply %s: %w", m.Version, err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, m.Version); err != nil {
				return fmt.Errorf("record %s: %w", m.Version, err)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("run %s migrations: %w", set.Name, err)
	}
	return nil
}

func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err) // dir is a compile-time constant matching the embed pattern
	}
	return sub
}
