// Package db owns the PostgreSQL connection pool and schema migrations.
//
// Migrations are plain .sql files embedded into the binary and applied on startup in filename order,
// each in its own transaction, tracked in schema_migrations. No external migration tool — this keeps
// switchboard a single static binary (ADR-0015) with raw SQL (ADR-0002).
package db

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Connect opens a pgx connection pool and verifies connectivity.
func Connect(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	if dsn == "" {
		return nil, fmt.Errorf("db: empty DATABASE_URL")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("db: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}
	return pool, nil
}

// migrationLockKey is a fixed, application-chosen key for the advisory lock that serializes
// migration runs. Any stable constant works; it must be identical across every binary that migrates
// this database.
const migrationLockKey int64 = 0x5357_4254_4d49_4752 // "SWBTMIGR", arbitrary but stable

// Migrate applies every embedded migration not yet recorded in schema_migrations, holding a Postgres
// advisory lock so only one migrator runs at a time.
//
// applyMigrations is check-then-act (SELECT EXISTS … then apply) with no atomicity across the set,
// and even CREATE TABLE IF NOT EXISTS races under concurrency (Postgres raises a pg_type
// duplicate_object, SQLSTATE 23505). Multiple app instances call Migrate at startup (server.go), and
// `go test ./...` runs several packages' migrators against one shared database — so without
// serialization two migrators can both apply the same step: the 0006 published→discoverable rename
// runs twice and the loser fails with `column "published" does not exist`. A session-scoped advisory
// lock lets exactly one migrator apply while the rest block, then find every version already recorded
// and no-op; Postgres auto-releases it if the holder's connection dies, so a crashed migrator never
// wedges startup. Governing: SPEC-0004 REQ "Embedded, Ordered, Transactional Migrations".
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("db: acquire migration conn: %w", err)
	}
	defer conn.Release()
	// Session-level lock on this single connection; the whole sequence then runs on the same
	// connection so the lock is held for its full duration and the apply cannot deadlock on pool
	// exhaustion. Blocks until any peer migrator releases.
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockKey); err != nil {
		return fmt.Errorf("db: acquire migration lock: %w", err)
	}
	defer func() {
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, migrationLockKey)
	}()

	return applyMigrations(ctx, conn, migrationsFS, "migrations")
}

// execConn is the slice of pgx used to apply migrations: satisfied by both *pgxpool.Pool and
// *pgxpool.Conn, so Migrate can run the sequence on its lock-holding connection while the
// rollback test drives applyMigrations with a bare pool.
type execConn interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Begin(ctx context.Context) (pgx.Tx, error)
}

// applyMigrations is the testable core of Migrate: it applies every .sql file under dir in fsys, in
// lexical order, each in its own transaction, recording applied versions in schema_migrations and
// skipping ones already recorded. Splitting the migration source out as an fs.FS lets tests inject a
// deliberately-failing migration to prove the transactional-rollback guarantee. Callers other than
// Migrate must guarantee no concurrent migrator (it is not internally serialized); Migrate holds an
// advisory lock for exactly that reason.
func applyMigrations(ctx context.Context, q execConn, fsys fs.FS, dir string) error {
	if _, err := q.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    text PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		return fmt.Errorf("db: ensure schema_migrations: %w", err)
	}

	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return fmt.Errorf("db: read migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		var exists bool
		if err := q.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version = $1)`, name,
		).Scan(&exists); err != nil {
			return fmt.Errorf("db: check migration %s: %w", name, err)
		}
		if exists {
			continue
		}
		sqlBytes, err := fs.ReadFile(fsys, path.Join(dir, name))
		if err != nil {
			return fmt.Errorf("db: read migration %s: %w", name, err)
		}
		tx, err := q.Begin(ctx)
		if err != nil {
			return fmt.Errorf("db: begin %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, string(sqlBytes)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("db: apply %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, name); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("db: record %s: %w", name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("db: commit %s: %w", name, err)
		}
	}
	return nil
}
