// Package testdb provisions the per-package test databases the DB-backed suites create from
// SWITCHBOARD_TEST_DATABASE_URL.
//
// Each DB-backed package's first test used to run CREATE DATABASE for its own database straight
// away, so `go test ./...` fired up to GOMAXPROCS concurrent creates against one Postgres.
// Concurrent creates from the same template race: one fails fast with SQLSTATE 55006 (the source
// template is "accessed by other users") or the creates wedge each other and the affected
// packages hang to the 10m test timeout. Both shapes passed on re-run, which is why the flake
// survived.
//
// Create runs every suite create under one session-level advisory lock on the server's /postgres
// maintenance database, so exactly one CREATE DATABASE runs at a time. The wait is bounded, so a
// wedged creator fails its waiters with a diagnosable error instead of hanging them to the
// package timeout. Test helper only: nothing here is linked into a request path.
//
// Governing: issue #543 (CI test-DB creation races across packages).
package testdb

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// createLockKey is a fixed, application-chosen key for the advisory lock that serializes test
// database creation, in the style of db.migrationLockKey. Any stable constant works; it must be
// identical across every package that creates a test database.
const createLockKey int64 = 0x5357425444424352 // "SWBTDBCR", arbitrary but stable

// lockWait bounds how long a package blocks for the create lock. A single CREATE DATABASE takes
// well under a second on CI, so 90s is a generous ceiling: a wedged creator trips it and the
// waiter fails fast with a clear error instead of a 10m package timeout.
const lockWait = 90 * time.Second

// Create ensures the named test database exists on the server for baseDSN, serialized across
// concurrently running test packages. It connects to the server's /postgres maintenance database
// (CREATE DATABASE can run from any database other than the target), so the lock always lives on
// one fixed database no matter what the base DSN itself points at. A duplicate name from an
// earlier run (SQLSTATE 42P04) is not an error, matching the per-package helpers'
// "already provisioned" tolerance.
func Create(ctx context.Context, baseDSN, name string) error {
	maintenance, err := maintenanceDSN(baseDSN)
	if err != nil {
		return err
	}
	conn, err := pgx.Connect(ctx, maintenance)
	if err != nil {
		return fmt.Errorf("testdb: connect (admin): %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	acquired, err := waitCreateLock(ctx, conn)
	if err != nil {
		return err
	}
	if !acquired {
		return fmt.Errorf("testdb: could not acquire the create lock after %s: another test process is wedged in CREATE DATABASE", lockWait)
	}
	defer func() {
		// Explicit release; if this fails, the session end in conn.Close is the backstop.
		_, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", createLockKey)
	}()

	// CREATE DATABASE has no IF NOT EXISTS; a duplicate from an earlier run is fine.
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "42P04" { // duplicate_database: already provisioned
			return nil
		}
		return fmt.Errorf("testdb: create %s: %w", name, err)
	}
	return nil
}

// waitCreateLock polls for the session-level advisory lock until it is held or the bound elapses,
// returning whether it was acquired. A plain pg_advisory_lock would block without bound, turning
// one wedged creator into a 10m hang for every waiting package — the exact shape issue #543
// reported — so the wait is bounded and the waiter fails with a diagnosable error.
func waitCreateLock(ctx context.Context, conn *pgx.Conn) (bool, error) {
	deadline := time.Now().Add(lockWait)
	for {
		var ok bool
		if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", createLockKey).Scan(&ok); err != nil {
			return false, fmt.Errorf("testdb: try create lock: %w", err)
		}
		if ok {
			return true, nil
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// maintenanceDSN returns baseDSN pointed at the server's /postgres maintenance database: the one
// fixed database every suite create locks, regardless of what the base DSN itself targets.
func maintenanceDSN(baseDSN string) (string, error) {
	u, err := url.Parse(baseDSN)
	if err != nil {
		return "", fmt.Errorf("testdb: parse dsn: %w", err)
	}
	u.Path = "/postgres"
	return u.String(), nil
}
