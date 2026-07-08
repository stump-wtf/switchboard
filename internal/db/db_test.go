package db

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// Governing: SPEC-0004 REQ "Connection Pool Lifecycle and Timeouts".

// An empty DSN must fail fast with the wrapped db: empty DATABASE_URL error, without touching
// the network. This case needs no database and always runs.
func TestConnectEmptyDSNFailsFast(t *testing.T) {
	_, err := Connect(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "db: empty DATABASE_URL") {
		t.Fatalf("empty DSN should fail with db: empty DATABASE_URL, got %v", err)
	}
}

// An unreachable database must fail fast with a wrapped db: ping error rather than hanging. Pointing
// at a closed port with a short connect_timeout exercises the Ping guard without a test DB.
func TestConnectUnreachableFailsFast(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// 127.0.0.1:1 is reserved/closed; connect_timeout bounds the attempt.
	_, err := Connect(ctx, "postgres://sb:sb@127.0.0.1:1/nope?sslmode=disable&connect_timeout=1")
	if err == nil {
		t.Fatal("connect to a dead port should fail")
	}
	if !strings.Contains(err.Error(), "db: connect") && !strings.Contains(err.Error(), "db: ping") {
		t.Fatalf("want wrapped db: connect/db: ping, got %v", err)
	}
}

// A live pool must Ping successfully, then serve context-governed queries, and a cancelled context
// must propagate to the driver rather than blocking. Requires the test DB.
func TestConnectPingsAndContextGovernsQueries(t *testing.T) {
	dsn := os.Getenv("SWITCHBOARD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set SWITCHBOARD_TEST_DATABASE_URL to run pool lifecycle tests")
	}
	ctx := context.Background()
	pool, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	// A cancelled context must cause the in-flight query to return promptly, not block.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := pool.Exec(cctx, "SELECT pg_sleep(5)"); err == nil {
		t.Fatal("query under a cancelled context should error, not block")
	}

	// After a clean shutdown the pool must refuse further work.
	pool.Close()
	if _, err := pool.Exec(ctx, "SELECT 1"); err == nil {
		t.Fatal("closed pool should not serve queries")
	}
}
