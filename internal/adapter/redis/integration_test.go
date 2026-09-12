package redis

// End-to-end integration coverage for the store-then-ack coupling (SPEC-0002 REQ "Store-Then-Ack
// Coupling", scenarios "Crash before todo-store redelivers and dedups to one todo" and "Ack happens
// only after durable store") against REAL PostgreSQL and Redis — the transport front half, the
// StoreSink back half, and the actual XACK/XPENDING bookkeeping, with no fakes in the loop.
//
// Gated like the store package's DB tests (issue #133 pattern): every test here skips unless BOTH
// SWITCHBOARD_TEST_DATABASE_URL and SWITCHBOARD_TEST_REDIS_URL are set, so `go test ./...` stays
// green without either service. The DB half provisions a package-owned database
// (switchboard_test_adapterredis) so parallel package runs never race a shared TRUNCATE.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"

	"github.com/stump-wtf/switchboard/internal/adapter"
	"github.com/stump-wtf/switchboard/internal/db"
	"github.com/stump-wtf/switchboard/internal/store"
)

// integrationDeps connects to the gated test Postgres (package-owned database, migrated,
// truncated) and Redis, skipping when either DSN is unset.
func integrationDeps(t *testing.T) (*store.Store, *pgxpool.Pool, *goredis.Client, context.Context) {
	t.Helper()
	dsn := os.Getenv("SWITCHBOARD_TEST_DATABASE_URL")
	redisURL := os.Getenv("SWITCHBOARD_TEST_REDIS_URL")
	if dsn == "" || redisURL == "" {
		t.Skip("set SWITCHBOARD_TEST_DATABASE_URL and SWITCHBOARD_TEST_REDIS_URL to run redis integration tests")
	}
	ctx := context.Background()

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse test dsn: %v", err)
	}
	const testDB = "switchboard_test_adapterredis"
	if u.Path != "/"+testDB {
		admin, err := db.Connect(ctx, dsn)
		if err != nil {
			t.Fatalf("connect (admin): %v", err)
		}
		if _, err := admin.Exec(ctx, "CREATE DATABASE "+testDB); err != nil &&
			!strings.Contains(err.Error(), "42P04") { // duplicate_database: already provisioned
			admin.Close()
			t.Fatalf("create adapter test database: %v", err)
		}
		admin.Close()
		u.Path = "/" + testDB
	}
	pool, err := db.Connect(ctx, u.String())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE todos, events, adapters RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	client, err := NewClient(redisURL)
	if err != nil {
		t.Fatalf("redis client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatalf("redis ping (SWITCHBOARD_TEST_REDIS_URL set but unreachable): %v", err)
	}
	return store.New(pool), pool, client, ctx
}

// crashingSink models a crash between consume and durable store: Deliver fails WITHOUT touching the
// store, so the entry must stay un-acked (pending) for redelivery.
type crashingSink struct{ calls atomic.Int32 }

func (s *crashingSink) Deliver(context.Context, adapter.Envelope) error {
	s.calls.Add(1)
	return errors.New("simulated crash before store")
}

// consumeUntil runs one Consume in the background until cond() holds (or the deadline passes),
// then cancels it and reports the observed condition. Consume must return context.Canceled — the
// graceful-shutdown contract.
func consumeUntil(t *testing.T, s *Stream, sink adapter.Sink, cond func() bool) bool {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Consume(ctx, sink) }()

	deadline := time.Now().Add(10 * time.Second)
	ok := false
	for time.Now().Before(deadline) {
		if cond() {
			ok = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Consume = %v, want context.Canceled", err)
	}
	return ok
}

// SPEC-0002 scenarios "Crash before todo-store redelivers and dedups to one todo" and "Ack happens
// only after durable store", end to end: a real stream entry is consumed, the store "crashes"
// before the todo persists (entry stays pending, zero todos), a restarted adapter redelivers it
// through the real StoreSink into real PostgreSQL (exactly one todo, XACKed), and a further
// redelivery of the same entry (crash-after-store / failed-ack case) dedups onto that one todo.
func TestIntegrationStreamStoreThenAck(t *testing.T) {
	st, pool, client, ctx := integrationDeps(t)

	stream := fmt.Sprintf("sb-it-%d", time.Now().UnixNano())
	const group = "switchboard-it"
	t.Cleanup(func() { client.Del(context.Background(), stream) })

	entryID, err := client.XAdd(ctx, &goredis.XAddArgs{
		Stream: stream, Values: map[string]any{"payload": `{"job":"deploy","sha":"abc123"}`},
	}).Result()
	if err != nil {
		t.Fatalf("xadd: %v", err)
	}

	newStream := func() *Stream {
		s, err := NewStream(client, testLogger(), StreamConfig{
			Stream: stream, Group: group, Consumer: "it-consumer",
			TrustDetail: "redis acl: integration-test",
			Block:       100 * time.Millisecond, // keep the loop responsive for the test
		})
		if err != nil {
			t.Fatalf("NewStream: %v", err)
		}
		return s
	}
	pendingCount := func() int64 {
		p, err := client.XPending(ctx, stream, group).Result()
		if err != nil {
			return -1
		}
		return p.Count
	}
	todoCount := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM todos`).Scan(&n); err != nil {
			t.Fatalf("count todos: %v", err)
		}
		return n
	}

	// Phase 1 — crash before store: the entry is consumed but Deliver fails before anything is
	// persisted. Store-then-ack means NO ack: the entry must be left pending for redelivery.
	crash := &crashingSink{}
	if !consumeUntil(t, newStream(), crash, func() bool {
		return crash.calls.Load() >= 1 && pendingCount() == 1
	}) {
		t.Fatalf("entry never went pending after failed store (pending=%d, deliveries=%d)",
			pendingCount(), crash.calls.Load())
	}
	if n := todoCount(); n != 0 {
		t.Fatalf("todos after crash-before-store = %d, want 0", n)
	}

	// Phase 2 — restart redelivers: a fresh adapter (same group+consumer) drains its pending
	// entries through the REAL StoreSink; the todo persists durably and only then is the entry
	// XACKed out of the pending list.
	sink, err := adapter.NewStoreSink(st, testLogger(), adapter.StoreSinkConfig{
		TrustDetail: "redis acl: integration-test",
	})
	if err != nil {
		t.Fatalf("NewStoreSink: %v", err)
	}
	if !consumeUntil(t, newStream(), sink, func() bool { return pendingCount() == 0 }) {
		t.Fatalf("entry never acked after durable store (pending=%d)", pendingCount())
	}
	if n := todoCount(); n != 1 {
		t.Fatalf("todos after redelivery = %d, want exactly 1", n)
	}

	// The single todo carries the entry-id idempotency key and routed to the stream-named queue.
	wantKey := "redis:" + stream + ":" + entryID
	var gotQueue, gotKey string
	if err := pool.QueryRow(ctx,
		`SELECT queue, idempotency_key FROM todos`).Scan(&gotQueue, &gotKey); err != nil {
		t.Fatalf("read todo: %v", err)
	}
	if gotKey != wantKey || gotQueue != stream {
		t.Fatalf("todo = queue %q key %q, want queue %q key %q", gotQueue, gotKey, stream, wantKey)
	}
	// The persisted event carries the queue trust metadata (SPEC-0002 scenario "Pull event carries
	// queue trust metadata").
	var family, trustMode, detail string
	var verified bool
	if err := pool.QueryRow(ctx,
		`SELECT family, trust_mode, verified, verify_detail FROM events WHERE source = 'redis'`).
		Scan(&family, &trustMode, &verified, &detail); err != nil {
		t.Fatalf("read event: %v", err)
	}
	if family != "queue" || trustMode != "queue" || verified || detail != "redis acl: integration-test" {
		t.Fatalf("event trust = (%s, %s, %v, %q), want (queue, queue, false, redis acl: integration-test)",
			family, trustMode, verified, detail)
	}

	// Phase 3 — redelivery after durable store (crash-after-store / failed-XACK case): delivering
	// the SAME entry again collapses onto the existing todo via the idempotency key. Deliver
	// returns nil (the transport may ack the redelivered copy) and no second todo appears.
	env := adapter.Envelope{
		Source: Source, Name: stream, ExternalID: entryID,
		Payload: []byte(`{"job":"deploy","sha":"abc123"}`), ReceivedAt: time.Now(),
	}
	if err := sink.Deliver(ctx, env); err != nil {
		t.Fatalf("redelivered Deliver = %v, want nil (dedup ack)", err)
	}
	if n := todoCount(); n != 1 {
		t.Fatalf("todos after dedup redelivery = %d, want still exactly 1", n)
	}
}
