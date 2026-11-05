package migrations

import (
	"context"
	"testing"
	"testing/fstest"

	"github.com/latch/relayer/internal/testdb"
)

func TestDepositSetOrdersByVersion(t *testing.T) {
	migs, err := Deposit.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(migs) == 0 || migs[0].Version != "deposit/001_init" {
		t.Fatalf("first migration = %+v, want deposit/001_init", migs)
	}
	for i := 1; i < len(migs); i++ {
		if migs[i-1].Version >= migs[i].Version {
			t.Fatalf("migrations out of order: %s before %s", migs[i-1].Version, migs[i].Version)
		}
	}
}

func TestDepositSetOrdersDownMigrations(t *testing.T) {
	migs, err := Deposit.LoadDown()
	if err != nil {
		t.Fatal(err)
	}
	if len(migs) == 0 {
		t.Fatal("expected down migrations for Deposit set, got 0")
	}
	if migs[len(migs)-1].Version != "deposit/001_init" {
		t.Fatalf("last down migration = %+v, want deposit/001_init", migs[len(migs)-1].Version)
	}
	for i := 1; i < len(migs); i++ {
		if migs[i-1].Version <= migs[i].Version {
			t.Fatalf("down migrations out of reverse order: %s before %s", migs[i-1].Version, migs[i].Version)
		}
	}
}

func TestGaslessSetOrdersDownMigrations(t *testing.T) {
	migs, err := Gasless.LoadDown()
	if err != nil {
		t.Fatal(err)
	}
	if len(migs) == 0 || migs[0].Version != "gasless/001_channel_accounts" {
		t.Fatalf("first gasless down migration = %+v, want gasless/001_channel_accounts", migs)
	}
}

func TestRunIsIdempotent(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if err := Run(ctx, pool, Deposit); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
	}

	migs, _ := Deposit.Load()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != len(migs) {
		t.Fatalf("schema_migrations has %d rows, want %d", n, len(migs))
	}
}

// A database created before versioning already has 001's tables but no
// schema_migrations row; Run must adopt it rather than fail.
func TestRunAdoptsPreVersioningDatabase(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	migs, _ := Deposit.Load()
	if _, err := pool.Exec(ctx, migs[0].SQL); err != nil {
		t.Fatal(err)
	}
	if err := Run(ctx, pool, Deposit); err != nil {
		t.Fatalf("run on legacy db: %v", err)
	}
	var ok bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version='deposit/001_init')`).Scan(&ok); err != nil || !ok {
		t.Fatalf("deposit/001_init not recorded (err=%v)", err)
	}
}

// A set only creates its own tables, and a later file in the set is applied
// on the next run without re-running earlier ones.
func TestRunAppliesOnlyItsOwnSetIncrementally(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	other := Set{Name: "other", FS: fstest.MapFS{
		"001_a.up.sql": {Data: []byte(`CREATE TABLE a (id int)`)},
	}}
	if err := Run(ctx, pool, other); err != nil {
		t.Fatal(err)
	}

	var depositTables int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_name IN ('intents','forwards','cursors')`).Scan(&depositTables); err != nil {
		t.Fatal(err)
	}
	if depositTables != 0 {
		t.Fatalf("running another set created %d deposit tables", depositTables)
	}

	// Add a second file. 001_a must not re-run (it isn't idempotent: CREATE
	// TABLE without IF NOT EXISTS would fail).
	other.FS.(fstest.MapFS)["002_b.up.sql"] = &fstest.MapFile{Data: []byte(`CREATE TABLE b (id int)`)}
	if err := Run(ctx, pool, other); err != nil {
		t.Fatalf("second run: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE version LIKE 'other/%'`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("other/* versions recorded = %d (err=%v), want 2", n, err)
	}
}

func TestRollback(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	set := Set{Name: "custom", FS: fstest.MapFS{
		"001_first.up.sql":    {Data: []byte(`CREATE TABLE custom_first (id int)`)},
		"001_first.down.sql":  {Data: []byte(`DROP TABLE IF EXISTS custom_first`)},
		"002_second.up.sql":   {Data: []byte(`CREATE TABLE custom_second (id int)`)},
		"002_second.down.sql": {Data: []byte(`DROP TABLE IF EXISTS custom_second`)},
	}}

	if err := Run(ctx, pool, set); err != nil {
		t.Fatalf("run: %v", err)
	}

	var tablesBefore int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_name IN ('custom_first', 'custom_second')`).Scan(&tablesBefore); err != nil {
		t.Fatal(err)
	}
	if tablesBefore != 2 {
		t.Fatalf("expected 2 tables created, got %d", tablesBefore)
	}

	if err := Rollback(ctx, pool, set); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	var tablesAfter int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_name IN ('custom_first', 'custom_second')`).Scan(&tablesAfter); err != nil {
		t.Fatal(err)
	}
	if tablesAfter != 0 {
		t.Fatalf("expected 0 tables after rollback, got %d", tablesAfter)
	}

	var remainingMigrations int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE version LIKE 'custom/%'`).Scan(&remainingMigrations); err != nil {
		t.Fatal(err)
	}
	if remainingMigrations != 0 {
		t.Fatalf("expected 0 recorded migrations after rollback, got %d", remainingMigrations)
	}
}

func TestRollbackStep(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	set := Set{Name: "step", FS: fstest.MapFS{
		"001_one.up.sql":   {Data: []byte(`CREATE TABLE step_one (id int)`)},
		"001_one.down.sql": {Data: []byte(`DROP TABLE IF EXISTS step_one`)},
		"002_two.up.sql":   {Data: []byte(`CREATE TABLE step_two (id int)`)},
		"002_two.down.sql": {Data: []byte(`DROP TABLE IF EXISTS step_two`)},
	}}

	if err := Run(ctx, pool, set); err != nil {
		t.Fatalf("run: %v", err)
	}

	if err := RollbackStep(ctx, pool, set); err != nil {
		t.Fatalf("rollback step: %v", err)
	}

	// Only 002_two should have been dropped; 001_one should still exist
	var hasOne, hasTwo bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = current_schema() AND table_name = 'step_one')`).Scan(&hasOne); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = current_schema() AND table_name = 'step_two')`).Scan(&hasTwo); err != nil {
		t.Fatal(err)
	}
	if !hasOne || hasTwo {
		t.Fatalf("expected step_one=true, step_two=false, got step_one=%v, step_two=%v", hasOne, hasTwo)
	}

	var remainingMigrations int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE version LIKE 'step/%'`).Scan(&remainingMigrations); err != nil {
		t.Fatal(err)
	}
	if remainingMigrations != 1 {
		t.Fatalf("expected 1 recorded migration after single rollback step, got %d", remainingMigrations)
	}
}
