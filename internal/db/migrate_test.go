package db

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgxpool"
)

// migrateTestSeq gives each migration test a unique throwaway-database name within this process.
var migrateTestSeq atomic.Int64

// migrateTestPool provisions a fresh, uniquely-named throwaway database on the test server and
// returns a pool bound to it, dropping it on cleanup. Migration tests must NOT reset the schema of
// the shared SWITCHBOARD_TEST_DATABASE_URL database: `go test ./...` runs the db and store packages
// in parallel against a single Postgres, so a `DROP SCHEMA public CASCADE` here would race with and
// destroy the store package's tables. A dedicated database per test isolates them fully. Skips when
// no test DB is configured.
func migrateTestPool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := os.Getenv("SWITCHBOARD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set SWITCHBOARD_TEST_DATABASE_URL to run migration tests")
	}
	ctx := context.Background()

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	name := fmt.Sprintf("sb_migrate_test_%d_%d", os.Getpid(), migrateTestSeq.Add(1))

	// Admin connection to the server's default maintenance database creates and drops the throwaway
	// DB (CREATE/DROP DATABASE cannot run from within the target database).
	adminURL := *u
	adminURL.Path = "/postgres"
	admin, err := Connect(ctx, adminURL.String())
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	if _, err := admin.Exec(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		admin.Close()
		t.Fatalf("create test db: %v", err)
	}

	targetURL := *u
	targetURL.Path = "/" + name
	pool, err := Connect(ctx, targetURL.String())
	if err != nil {
		admin.Close()
		t.Fatalf("connect test db: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(ctx, `DROP DATABASE IF EXISTS "`+name+`" WITH (FORCE)`)
		admin.Close()
	})
	return pool, ctx
}

func recorded(t *testing.T, pool *pgxpool.Pool, ctx context.Context, version string) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version = $1)`, version).Scan(&ok); err != nil {
		t.Fatalf("check recorded: %v", err)
	}
	return ok
}

// Governing: SPEC-0004 REQ "Embedded, Ordered, Transactional Migrations" — a migration applies once
// and is skipped idempotently on later starts.
func TestMigrateAppliesOnceAndIsIdempotent(t *testing.T) {
	pool, ctx := migrateTestPool(t)

	// First run applies the real embedded schema.
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	if !recorded(t, pool, ctx, "0001_init.sql") {
		t.Fatal("0001_init.sql should be recorded after first migrate")
	}
	var todos int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM todos`).Scan(&todos); err != nil {
		t.Fatalf("schema not created: %v", err)
	}

	// A second run must be a clean no-op (idempotent, at-most-once) and must not error.
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("second migrate should be idempotent, got %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE version = '0001_init.sql'`).Scan(&n); err != nil {
		t.Fatalf("count versions: %v", err)
	}
	if n != 1 {
		t.Fatalf("0001_init.sql recorded %d times, want exactly 1", n)
	}
}

// Governing: SPEC-0004 REQ "Embedded, Ordered, Transactional Migrations" — a migration that fails
// partway rolls back atomically, records no version, and aborts with an error naming the migration.
func TestMigrateFailedMigrationRollsBackAtomically(t *testing.T) {
	pool, ctx := migrateTestPool(t)

	// A synthetic migration set: a good one, then one that creates a table and THEN hits invalid SQL.
	// If the transaction is honored, the partial table must not survive and the version must not record.
	bad := fstest.MapFS{
		"m/0001_ok.sql":      {Data: []byte(`CREATE TABLE ok_marker (id int);`)},
		"m/0002_partial.sql": {Data: []byte(`CREATE TABLE zzz_partial (id int); THIS IS NOT VALID SQL;`)},
	}
	err := applyMigrations(ctx, pool, bad, "m")
	if err == nil {
		t.Fatal("bad migration should return an error")
	}
	if !strings.Contains(err.Error(), "0002_partial.sql") {
		t.Fatalf("error should name the failing migration, got %v", err)
	}
	// The good migration before it committed and recorded.
	if !recorded(t, pool, ctx, "0001_ok.sql") {
		t.Fatal("preceding good migration should have committed")
	}
	// The failed migration recorded no version...
	if recorded(t, pool, ctx, "0002_partial.sql") {
		t.Fatal("failed migration must not be recorded")
	}
	// ...and left no partial state: zzz_partial must not exist.
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.zzz_partial') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatalf("regclass check: %v", err)
	}
	if exists {
		t.Fatal("failed migration's partial DDL must have been rolled back")
	}
}
