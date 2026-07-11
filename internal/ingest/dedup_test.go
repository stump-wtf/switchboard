// Handler-level idempotency dedup & enqueue-as-todo coverage: an accepted delivery inserts an
// event, creates a referencing todo, publishes to the hub only when newly created, and returns 202
// with the todo id + queue; a redelivery collapses onto the existing non-terminal todo and creates
// no duplicate event or todo.
//
// Governing: SPEC-0001 REQ "Idempotency Key Extraction and Dedup",
// SPEC-0001 REQ "Enqueue Accepted Delivery as Todo".
package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joestump/switchboard/internal/db"
	"github.com/joestump/switchboard/internal/store"
)

// idempotencyKey MUST prefer the provider delivery id and MUST fall back to sha256(body) where the
// provider supplies none — never an empty key. Governing: SPEC-0001 REQ "Idempotency Key Extraction
// and Dedup".
func TestIdempotencyKeyDerivation(t *testing.T) {
	body := []byte(`{"action":"opened"}`)

	// Provider delivery id wins when present.
	if got := idempotencyKey("guid-123", body); got != "guid-123" {
		t.Fatalf("delivery id should win: got %q", got)
	}
	// No delivery id → sha256(body) fallback, never empty.
	fallback := idempotencyKey("", body)
	if !strings.HasPrefix(fallback, "sha256:") || len(fallback) != len("sha256:")+64 {
		t.Fatalf("fallback should be a sha256 body hash: got %q", fallback)
	}
	// Same body → same key (redeliveries collapse); distinct bodies → distinct keys.
	if idempotencyKey("", body) != fallback {
		t.Fatal("same body must derive the same fallback key")
	}
	if idempotencyKey("", []byte(`{"action":"closed"}`)) == fallback {
		t.Fatal("distinct bodies must derive distinct fallback keys")
	}
}

// ingestTestPool connects to an ingest-package-OWNED database derived from
// SWITCHBOARD_TEST_DATABASE_URL (created on first use), migrated and truncated. `go test ./...`
// runs packages in parallel against the same test DSN, so truncating the shared database here
// races the store package's tests; a dedicated database isolates the two. Skips cleanly when no
// test DB is configured (Gitea CI is the gate).
func ingestTestPool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := os.Getenv("SWITCHBOARD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set SWITCHBOARD_TEST_DATABASE_URL to run ingest accept-path tests")
	}
	ctx := context.Background()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse test dsn: %v", err)
	}
	const testDB = "switchboard_ingest_test"
	if u.Path != "/"+testDB {
		admin, err := db.Connect(ctx, dsn)
		if err != nil {
			t.Fatalf("connect (admin): %v", err)
		}
		// CREATE DATABASE has no IF NOT EXISTS; a duplicate from an earlier run is fine. Tests
		// within one package run sequentially, so no concurrent CREATE races this.
		if _, err := admin.Exec(ctx, `CREATE DATABASE `+testDB); err != nil {
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "42P04" { // duplicate_database
				admin.Close()
				t.Fatalf("create ingest test database: %v", err)
			}
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
	// Truncate every table the accept-path tests seed — not just todos/events. The self-managed
	// receiver tests seed humans → agents → endpoints → endpoint_webhooks (via seedWebhook), and the
	// test database persists across runs; leaving those rows behind makes a second `go test` run fail
	// on a duplicate endpoint credhash. Clearing the full set keeps repeated runs idempotent.
	if _, err := pool.Exec(ctx,
		`TRUNCATE humans, agents, endpoints, endpoint_webhooks, todos, events RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return pool, ctx
}

// testIngestDeps builds an Ingest against the real store, with a hub whose publishes the test can
// observe and the pool for row-count asserts.
func testIngestDeps(t *testing.T, cfg Config) (*Ingest, *Hub, *pgxpool.Pool, context.Context) {
	t.Helper()
	pool, ctx := ingestTestPool(t)
	hub := NewHub()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(store.New(pool), hub, log, cfg), hub, pool, ctx
}

// post drives a handler with the given raw body and headers, returning the recorder.
func post(t *testing.T, handler http.HandlerFunc, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

// accepted202 decodes the 202 response contract: {id, queue, verified}.
func accepted202(t *testing.T, rec *httptest.ResponseRecorder) (id, queue string) {
	t.Helper()
	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d, want 202 (body: %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		ID    string `json:"id"`
		Queue string `json:"queue"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode 202 body: %v (%s)", err, rec.Body.String())
	}
	if resp.ID == "" || resp.Queue == "" {
		t.Fatalf("202 must carry the todo id and queue: %s", rec.Body.String())
	}
	return resp.ID, resp.Queue
}

// countRows returns the row count of a fixed, test-owned query.
func countRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, query string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, query).Scan(&n); err != nil {
		t.Fatalf("count (%s): %v", query, err)
	}
	return n
}

// drainHub counts todos currently buffered on a hub subscription without blocking.
func drainHub(ch <-chan store.Todo) int {
	n := 0
	for {
		select {
		case <-ch:
			n++
		default:
			return n
		}
	}
}

// A GitHub redelivery (same X-GitHub-Delivery GUID) collapses onto the existing non-terminal todo:
// the second delivery returns that todo, creates no duplicate todo or event, and publishes to the
// hub only for the first (newly-created) delivery. A distinct delivery id creates a distinct todo.
// Governing: SPEC-0001 scenario "Redelivery of the same webhook creates one todo",
// scenario "Distinct deliveries create distinct todos", REQ "Enqueue Accepted Delivery as Todo".
func TestGitHubRedeliveryDedup(t *testing.T) {
	const secret = "s3cr3t"
	ing, hub, pool, ctx := testIngestDeps(t, Config{GitHubSecret: secret})
	ch, cancel := hub.Subscribe([]string{"reviews"})
	defer cancel()

	body := `{"action":"opened","pull_request":{"number":1,"title":"One"},"repository":{"full_name":"joestump/switchboard"}}`
	headers := func(delivery string) map[string]string {
		return map[string]string{
			"X-Hub-Signature-256": sign(secret, []byte(body)),
			"X-GitHub-Event":      "pull_request",
			"X-GitHub-Delivery":   delivery,
			"Content-Type":        "application/json",
		}
	}

	// First delivery: event + todo created, published to the hub, 202 {id, queue}.
	id1, queue := accepted202(t, post(t, ing.GitHub, "/webhooks/github", body, headers("guid-1")))
	if queue != "reviews" {
		t.Fatalf("queue = %q, want reviews (default GitHub queue)", queue)
	}

	// Redelivery of the SAME delivery GUID: returns the existing todo, creates nothing new.
	id2, _ := accepted202(t, post(t, ing.GitHub, "/webhooks/github", body, headers("guid-1")))
	if id2 != id1 {
		t.Fatalf("redelivery must return the existing todo: got %s, want %s", id2, id1)
	}
	if n := countRows(t, ctx, pool, `SELECT count(*) FROM todos`); n != 1 {
		t.Fatalf("redelivery must not create a duplicate todo: %d rows", n)
	}
	// Events dedup on (source, external_id): still exactly one event row.
	if n := countRows(t, ctx, pool, `SELECT count(*) FROM events`); n != 1 {
		t.Fatalf("redelivery must not create a duplicate event: %d rows", n)
	}
	// The todo references the persisted event.
	if n := countRows(t, ctx, pool, `SELECT count(*) FROM todos WHERE event_id IS NOT NULL`); n != 1 {
		t.Fatal("todo must reference the persisted event")
	}
	// Hub was nudged exactly once — only the newly-created todo publishes.
	if n := drainHub(ch); n != 1 {
		t.Fatalf("hub publishes = %d, want 1 (publish only when newly created)", n)
	}

	// A DISTINCT delivery id derives a distinct key and creates its own todo.
	id3, _ := accepted202(t, post(t, ing.GitHub, "/webhooks/github", body, headers("guid-2")))
	if id3 == id1 {
		t.Fatal("distinct delivery ids must create distinct todos")
	}
	if n := countRows(t, ctx, pool, `SELECT count(*) FROM todos`); n != 2 {
		t.Fatalf("distinct delivery should add a todo: %d rows, want 2", n)
	}
	if n := drainHub(ch); n != 1 {
		t.Fatalf("hub publishes for distinct delivery = %d, want 1", n)
	}
}

// A GitHub delivery with NO X-GitHub-Delivery header still derives a key (sha256 of the body), so
// identical redeliveries without the GUID still collapse to one todo instead of bypassing dedup
// with a NULL key. Governing: SPEC-0001 REQ "Idempotency Key Extraction and Dedup" (body-hash
// fallback where the provider supplies no delivery id).
func TestGitHubMissingDeliveryIDFallsBackToBodyHash(t *testing.T) {
	const secret = "s3cr3t"
	ing, _, pool, ctx := testIngestDeps(t, Config{GitHubSecret: secret})

	body := `{"action":"opened"}`
	headers := map[string]string{
		"X-Hub-Signature-256": sign(secret, []byte(body)),
		"X-GitHub-Event":      "ping",
	}
	id1, _ := accepted202(t, post(t, ing.GitHub, "/webhooks/github", body, headers))
	id2, _ := accepted202(t, post(t, ing.GitHub, "/webhooks/github", body, headers))
	if id2 != id1 {
		t.Fatalf("same body without a delivery id must dedup: %s vs %s", id1, id2)
	}
	if n := countRows(t, ctx, pool, `SELECT count(*) FROM todos`); n != 1 {
		t.Fatalf("todos = %d, want 1", n)
	}
	// The persisted key is the body-hash fallback, never empty/NULL.
	var key string
	if err := pool.QueryRow(ctx, `SELECT idempotency_key FROM todos`).Scan(&key); err != nil {
		t.Fatalf("read idempotency_key: %v", err)
	}
	if !strings.HasPrefix(key, "sha256:") {
		t.Fatalf("idempotency_key = %q, want sha256 body-hash fallback", key)
	}
}

// The generic (token) endpoint has no provider delivery id, so identical redeliveries dedup on the
// body hash; the todo lands in the provider's configured queue. Extends the token-mode path without
// regressing verification. Governing: SPEC-0001 REQ "Idempotency Key Extraction and Dedup",
// REQ "Enqueue Accepted Delivery as Todo" (queue selection per provider config).
func TestGenericTokenRedeliveryDedup(t *testing.T) {
	ing, hub, pool, ctx := testIngestDeps(t, Config{
		Generic: map[string]GenericProvider{
			"dockerhub": {Mode: "token", Token: "tok", Queue: "builds"},
		},
	})
	ch, cancel := hub.Subscribe([]string{"builds"})
	defer cancel()

	body := `{"push_data":{"tag":"latest"}}`
	headers := map[string]string{"X-Webhook-Token": "tok"}
	deliver := func(payload string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		ing.Generic(rec, genericRequest("dockerhub", payload, headers, ""))
		return rec
	}

	id1, queue := accepted202(t, deliver(body))
	if queue != "builds" {
		t.Fatalf("queue = %q, want builds (provider-configured queue)", queue)
	}
	id2, _ := accepted202(t, deliver(body))
	if id2 != id1 {
		t.Fatalf("identical generic redelivery must dedup on body hash: %s vs %s", id1, id2)
	}
	if n := countRows(t, ctx, pool, `SELECT count(*) FROM todos`); n != 1 {
		t.Fatalf("todos = %d, want 1", n)
	}
	if n := countRows(t, ctx, pool, `SELECT count(*) FROM events`); n != 1 {
		t.Fatalf("events = %d, want 1", n)
	}
	if n := drainHub(ch); n != 1 {
		t.Fatalf("hub publishes = %d, want 1", n)
	}

	// A different body derives a different key → a second todo.
	id3, _ := accepted202(t, deliver(`{"push_data":{"tag":"v2"}}`))
	if id3 == id1 {
		t.Fatal("distinct bodies must create distinct todos")
	}
}
