package metrics

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stump-wtf/switchboard/internal/db"
	"github.com/stump-wtf/switchboard/internal/store"
)

// queueTestStore connects to a migrated, truncated database dedicated to this package. `go test
// ./...` runs packages in parallel against one DSN, so sharing the store package's database would
// race its truncates; a per-package database isolates the two. Skips when no test DB is configured.
func queueTestStore(t *testing.T) (*store.Store, *pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := os.Getenv("SWITCHBOARD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set SWITCHBOARD_TEST_DATABASE_URL to run the DB-backed queue liveness tests")
	}
	ctx := context.Background()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse test dsn: %v", err)
	}
	const testDB = "switchboard_metrics_test"
	if u.Path != "/"+testDB {
		admin, err := db.Connect(ctx, dsn)
		if err != nil {
			t.Fatalf("connect (admin): %v", err)
		}
		// CREATE DATABASE has no IF NOT EXISTS; a duplicate from an earlier run is fine. Tests
		// within one package run sequentially, so no concurrent CREATE races this.
		if _, err := admin.Exec(ctx, "CREATE DATABASE "+testDB); err != nil &&
			!strings.Contains(err.Error(), "42P04") { // duplicate_database: already provisioned
			admin.Close()
			t.Fatalf("create metrics test database: %v", err)
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
	if _, err := pool.Exec(ctx,
		`TRUNCATE humans, agents, endpoints, todos, events RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return store.New(pool), pool, ctx
}

// queueSeedEndpoint provisions human → agent → endpoint scoped on queues and returns the endpoint
// id todos are pinned to (todos.endpoint_id is NOT NULL; ADR-0022).
func queueSeedEndpoint(t *testing.T, st *store.Store, ctx context.Context, label string, queues ...string) string {
	t.Helper()
	h, err := st.UpsertHuman(ctx, "pocket|"+label, label, "")
	if err != nil {
		t.Fatalf("seed human: %v", err)
	}
	ag, err := st.CreateAgent(ctx, h.ID, label, "")
	if err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	slug, err := store.MintSlug(label)
	if err != nil {
		t.Fatalf("seed slug: %v", err)
	}
	ep, err := st.CreateEndpoint(ctx, ag.ID, "credhash-"+label, "sbk_"+label, slug, queues, []string{"list_todos", "claim"})
	if err != nil {
		t.Fatalf("seed endpoint: %v", err)
	}
	return ep.ID
}

// scrapeQueueText performs a real authenticated GET /metrics through the scrape handler and returns
// the text exposition's sample lines keyed by series (`name{labels}` → raw value text).
func scrapeQueueText(t *testing.T, m *Metrics) map[string]string {
	t.Helper()
	token := strings.Repeat("t", 32)
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	m.Handler(token).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape status = %d, want 200", rec.Code)
	}
	out := make(map[string]string)
	sc := bufio.NewScanner(rec.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.LastIndexByte(line, ' ')
		if i < 0 {
			t.Fatalf("malformed exposition line %q", line)
		}
		out[line[:i]] = line[i+1:]
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read scrape: %v", err)
	}
	return out
}

// The 2026-09-14 outage, pinned as a test against the real store: forge holds 50 pending todos,
// none claimed, the head 20 hours old; deploys is only scoped on an endpoint. The scrape carries
// exactly the series the alert `pending > 0 and ignoring(state) claimed == 0` reads, and deploys
// reports all four states at zero.
// Governing: SPEC-0023 REQ-2 (scenario "the 2026-09-14 outage, as it would have appeared");
// design.md "Testing", "Zero-value series".
func TestQueueLivenessIncidentScrape(t *testing.T) {
	st, pool, ctx := queueTestStore(t)
	ep := queueSeedEndpoint(t, st, ctx, "incident", "forge", "deploys")
	const pending = 50
	var headID string
	for i := range pending {
		td, _, err := st.CreateTodo(ctx, store.CreateTodoParams{EndpointID: ep, Queue: "forge",
			Title: "review", IdempotencyKey: fmt.Sprintf("incident-%d", i)})
		if err != nil {
			t.Fatalf("create todo: %v", err)
		}
		if i == 0 {
			headID = td.ID
		}
	}
	if _, err := pool.Exec(ctx,
		`UPDATE todos SET created_at = now() - interval '20 hours' WHERE id = $1`, headID); err != nil {
		t.Fatalf("backdate head: %v", err)
	}

	m := New(Options{})
	m.RegisterQueueStats(st)
	got := scrapeQueueText(t, m)

	want := map[string]string{
		`switchboard_queue_todos{queue="forge",state="pending"}`:         strconv.Itoa(pending),
		`switchboard_queue_todos{queue="forge",state="claimed"}`:         "0",
		`switchboard_queue_todos{queue="forge",state="done"}`:            "0",
		`switchboard_queue_todos{queue="forge",state="failed"}`:          "0",
		`switchboard_queue_todos{queue="deploys",state="pending"}`:       "0",
		`switchboard_queue_todos{queue="deploys",state="claimed"}`:       "0",
		`switchboard_queue_todos{queue="deploys",state="done"}`:          "0",
		`switchboard_queue_todos{queue="deploys",state="failed"}`:        "0",
		`switchboard_queue_oldest_pending_seconds{queue="deploys"}`:      "0",
		`switchboard_metrics_collection_errors_total{collector="queue"}`: "0",
	}
	for series, v := range want {
		if got[series] != v {
			t.Errorf("scrape: %s = %q, want %q", series, got[series], v)
		}
	}
	age, err := strconv.ParseFloat(got[`switchboard_queue_oldest_pending_seconds{queue="forge"}`], 64)
	if err != nil {
		t.Fatalf("forge oldest pending: %v", err)
	}
	if age < 20*3600 {
		t.Errorf("forge oldest pending = %.0fs, want >= 20h (the backdated head)", age)
	}
}

// A database that cannot answer omits both gauge families from a real scrape and increments the
// collector's error counter — the outage must not read as a fleet of healthy, empty queues.
// Governing: SPEC-0023 REQ-6 "Honest absence"; design.md "Testing" (collector-failure test).
func TestQueueLivenessDatabaseFailureOmitsGauges(t *testing.T) {
	st, pool, ctx := queueTestStore(t)
	ep := queueSeedEndpoint(t, st, ctx, "outage", "forge")
	if _, _, err := st.CreateTodo(ctx, store.CreateTodoParams{EndpointID: ep, Queue: "forge", Title: "t", IdempotencyKey: "o1"}); err != nil {
		t.Fatalf("create todo: %v", err)
	}
	m := New(Options{})
	m.RegisterQueueStats(st)
	if got := scrapeQueueText(t, m)[`switchboard_queue_todos{queue="forge",state="pending"}`]; got != "1" {
		t.Fatalf("healthy scrape: forge pending = %q, want 1", got)
	}

	pool.Close() // the database goes away; Cleanup's second Close is a no-op
	got := scrapeQueueText(t, m)
	for series := range got {
		if strings.HasPrefix(series, "switchboard_queue_") {
			t.Errorf("scrape with the database down still carries %s; want both families omitted", series)
		}
	}
	if v := queueCollectionErrors(t, m); v != 1 {
		t.Errorf("collection errors = %v, want 1", v)
	}
	// The next scrape's exposition carries the first failure's increment at least; its own (the
	// database is still down) may or may not be counted yet, because collectors run concurrently.
	next := scrapeQueueText(t, m)[`switchboard_metrics_collection_errors_total{collector="queue"}`]
	if v, err := strconv.ParseFloat(next, 64); err != nil || v < 1 || v > 2 {
		t.Errorf("collection errors on the following scrape = %q, want 1 or 2", next)
	}
}
