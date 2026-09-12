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
	"github.com/stump-wtf/switchboard/internal/db"
	"github.com/stump-wtf/switchboard/internal/store"
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
	// adapters joined the set when it became the provider registry (ADR-0020): the registry-dispatch
	// tests seed provider rows, and leftovers from a prior run must never hijack an env-configured
	// receiver test (a stale "github" row would override the test's env secret).
	if _, err := pool.Exec(ctx,
		`TRUNCATE humans, agents, endpoints, endpoint_webhooks, todos, events, adapters RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return pool, ctx
}

// seedEndpoint mints a real human → agent → endpoint chain and returns the human and the endpoint.
//
// Every todo is now owned by exactly one endpoint (ADR-0022; todos.endpoint_id is NOT NULL and
// references endpoints), so any test that expects a delivery to BECOME a todo must have a genuine
// endpoint row for it to be pinned to. An invented uuid would fail the foreign key and an empty one
// the not-null, so there is no shortcut: the fixture has to be real. label makes the human subject,
// agent name and slug unique so a single test can seed two distinct tenants and assert they do not
// see each other's work.
// Governing: ADR-0022, SPEC-0003 REQ "Endpoint Ownership (Tenant Isolation)".
func seedEndpoint(t *testing.T, st *store.Store, ctx context.Context, label string, queues []string) (store.Human, store.Endpoint) {
	t.Helper()
	h, err := st.UpsertHuman(ctx, "pocket|"+label, "Joe "+label, "")
	if err != nil {
		t.Fatalf("upsert human (%s): %v", label, err)
	}
	ag, err := st.CreateAgent(ctx, h.ID, "bot-"+label, "")
	if err != nil {
		t.Fatalf("create agent (%s): %v", label, err)
	}
	slug, err := store.MintSlug(ag.Name)
	if err != nil {
		t.Fatalf("mint slug (%s): %v", label, err)
	}
	ep, err := st.CreateEndpoint(ctx, ag.ID, "credhash-"+label, "sbk_"+label, slug, queues,
		[]string{"list_todos", "claim", "create_webhook"})
	if err != nil {
		t.Fatalf("vend endpoint (%s): %v", label, err)
	}
	return h, ep
}

// legacyReceiverQueues are the queues the operator-configured receivers target across this package's
// accept-path tests. The seeded legacy endpoint is scoped to all of them so one fixture serves every
// receiver.
var legacyReceiverQueues = []string{"reviews", "stripe", "slack", "builds", "wizq", "regq", "lan", "dockerhub", "homelab", "wizard"}

// seedLegacyEndpoint mints the operator-designated endpoint that owns todos minted by the
// OPERATOR-CONFIGURED receivers (/webhooks/github, /webhooks/stripe, /webhooks/slack,
// /webhooks/generic/{name}), and returns its id for Config.LegacyEndpointID.
//
// Those receivers are configured by an operator rather than vended to an agent, so nothing in their
// configuration names a tenant — Ingest.legacyEndpoint answers 503 and persists NOTHING when the id
// is unset. Without this fixture every accept-path test below would get a 503 instead of a 202, so
// the seed is what keeps them testing ingestion rather than testing the misconfiguration branch.
// INTERIM alongside Config.LegacyEndpointID itself; both die with those receivers in PR 2.
// Governing: ADR-0022.
func seedLegacyEndpoint(t *testing.T, st *store.Store, ctx context.Context) string {
	t.Helper()
	_, ep := seedEndpoint(t, st, ctx, "legacy", legacyReceiverQueues)
	return ep.ID
}

// testIngestDeps builds an Ingest against the real store, with a hub whose publishes the test can
// observe, the pool for row-count asserts, and the id of the seeded legacy endpoint that owns every
// todo the operator-configured receivers mint.
//
// The endpoint id is returned, not hidden, because hub fan-out is now endpoint-scoped: Hub.Subscribe
// takes the owning endpoint and a subscription with the wrong (or empty) endpoint matches NOTHING.
// A test that subscribed without it would drain zero todos and pass vacuously while asserting
// nothing at all. Governing: ADR-0022, SPEC-0011 REQ "Scope-Filtered Fan-Out".
func testIngestDeps(t *testing.T, cfg Config) (*Ingest, *Hub, *pgxpool.Pool, context.Context, string) {
	t.Helper()
	pool, ctx := ingestTestPool(t)
	st := store.New(pool)
	hub := NewHub()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	endpointID := seedLegacyEndpoint(t, st, ctx)
	cfg.LegacyEndpointID = endpointID
	return New(st, hub, log, cfg), hub, pool, ctx, endpointID
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

// acceptedTodo is one entry of the 202 body's `todos` array: where a delivery actually landed.
// endpoint_id is part of the contract because a fan-out is only auditable if the response says which
// tenant each todo was minted for. Governing: SPEC-0001 REQ "Deterministic Route Fan-Out
// (Token-Free)".
type acceptedTodo struct {
	ID         string `json:"id"`
	EndpointID string `json:"endpoint_id"`
	Queue      string `json:"queue"`
	Created    bool   `json:"created"`
}

// accepted202 decodes the 202 accept contract and returns the OWNER endpoint's todo id and queue.
//
// Fan-out changed this body's shape (ADR-0022): one delivery now mints one todo PER TARGET endpoint,
// so the self-managed receiver's payload carries a `todos` array and a `created` count alongside the
// top-level `id`/`queue`. Those top-level fields still name the OWNER's todo — ResolveWebhookTargets
// returns the owner first — so the operator-configured receivers, which mint exactly one todo and
// emit no array at all, stay byte-identical to the pre-fan-out contract and this helper still reads
// them unchanged.
//
// Where the array IS present the helper asserts the two views agree, so a future change that lets
// the compatibility `id` drift away from todos[0] fails loudly here instead of silently misreporting
// which tenant the work landed in.
// Governing: SPEC-0001 REQ "Enqueue Accepted Delivery as Endpoint-Owned Todo".
func accepted202(t *testing.T, rec *httptest.ResponseRecorder) (id, queue string) {
	t.Helper()
	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d, want 202 (body: %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		ID    string         `json:"id"`
		Queue string         `json:"queue"`
		Todos []acceptedTodo `json:"todos"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode 202 body: %v (%s)", err, rec.Body.String())
	}
	if resp.ID == "" || resp.Queue == "" {
		t.Fatalf("202 must carry the todo id and queue: %s", rec.Body.String())
	}
	if len(resp.Todos) > 0 {
		owner := resp.Todos[0]
		if owner.ID != resp.ID || owner.Queue != resp.Queue {
			t.Fatalf("202 top-level id/queue (%s/%s) must name the OWNER todo todos[0] (%s/%s): %s",
				resp.ID, resp.Queue, owner.ID, owner.Queue, rec.Body.String())
		}
		if owner.EndpointID == "" {
			t.Fatalf("202 fan-out entries must name their owning endpoint: %s", rec.Body.String())
		}
	}
	return resp.ID, resp.Queue
}

// fanOut202 decodes the full fan-out view of a 202: every target's todo plus the count of newly
// minted rows. Tests that assert WHERE a delivery landed (and that a redelivery collapsed
// per-target) use this rather than accepted202's owner-only view.
// Governing: ADR-0022, SPEC-0001 REQ "Deterministic Route Fan-Out (Token-Free)",
// SPEC-0003 REQ "Per-Endpoint Idempotency and Dedup".
func fanOut202(t *testing.T, rec *httptest.ResponseRecorder) (todos []acceptedTodo, created int) {
	t.Helper()
	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d, want 202 (body: %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Todos   []acceptedTodo `json:"todos"`
		Created int            `json:"created"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode 202 body: %v (%s)", err, rec.Body.String())
	}
	if len(resp.Todos) == 0 {
		t.Fatalf("202 must report the todos the delivery fanned out to: %s", rec.Body.String())
	}
	return resp.Todos, resp.Created
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
	ing, hub, pool, ctx, endpointID := testIngestDeps(t, Config{GitHubSecret: secret})
	// Subscribe as the endpoint that OWNS these todos. Passing the real id is load-bearing: the hub
	// filters on owning endpoint before queue (ADR-0022), so a subscription with the wrong or empty
	// id would match nothing and every drainHub assertion below would read 0 and pass vacuously.
	ch, cancel := hub.Subscribe(endpointID, []string{"reviews"})
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

// The Gitea receiver rides the same HMAC scheme as GitHub but keys off its own headers
// (X-Gitea-Event / X-Gitea-Delivery) and defaults to the "gitea" queue: a verified delivery
// persists event + todo atomically, a redelivered GUID collapses onto the existing todo, and a
// distinct GUID mints a new one. Governing: SPEC-0001 REQ "Signed Webhook Verification", REQ
// "Idempotency Key Extraction and Dedup"; SPEC-0002/0004 REQ atomic ingestion.
func TestGiteaRedeliveryDedup(t *testing.T) {
	const secret = "s3cr3t"
	ing, hub, pool, ctx, endpointID := testIngestDeps(t, Config{GiteaSecret: secret})
	ch, cancel := hub.Subscribe(endpointID, []string{"gitea"})
	defer cancel()

	body := `{"action":"opened","issue":{"number":96,"title":"Live list"},"repository":{"full_name":"stump.wtf/switchboard"}}`
	headers := func(delivery string) map[string]string {
		return map[string]string{
			"X-Hub-Signature-256": sign(secret, []byte(body)),
			"X-Gitea-Event":       "issues",
			"X-Gitea-Delivery":    delivery,
			"Content-Type":        "application/json",
		}
	}

	// First delivery: event + todo created on the default gitea queue, published, 202 {id, queue}.
	id1, queue := accepted202(t, post(t, ing.Gitea, "/webhooks/gitea", body, headers("guid-1")))
	if queue != "gitea" {
		t.Fatalf("queue = %q, want gitea (default Gitea queue)", queue)
	}
	if n := drainHub(ch); n != 1 {
		t.Fatalf("hub publishes = %d, want 1", n)
	}

	// Redelivery of the SAME delivery GUID: returns the existing todo, creates nothing new.
	id2, _ := accepted202(t, post(t, ing.Gitea, "/webhooks/gitea", body, headers("guid-1")))
	if id2 != id1 {
		t.Fatalf("redelivery must return the existing todo: got %s, want %s", id2, id1)
	}
	if n := countRows(t, ctx, pool, `SELECT count(*) FROM todos`); n != 1 {
		t.Fatalf("redelivery must not create a duplicate todo: %d rows", n)
	}
	if n := drainHub(ch); n != 0 {
		t.Fatalf("redelivery must not publish: %d", n)
	}

	// A DISTINCT delivery id derives a distinct key and creates its own todo.
	id3, _ := accepted202(t, post(t, ing.Gitea, "/webhooks/gitea", body, headers("guid-2")))
	if id3 == id1 {
		t.Fatal("distinct delivery ids must create distinct todos")
	}
	if n := countRows(t, ctx, pool, `SELECT count(*) FROM todos`); n != 2 {
		t.Fatalf("distinct delivery should add a todo: %d rows, want 2", n)
	}
}

// A GitHub delivery with NO X-GitHub-Delivery header still derives a key (sha256 of the body), so
// identical redeliveries without the GUID still collapse to one todo instead of bypassing dedup
// with a NULL key. Governing: SPEC-0001 REQ "Idempotency Key Extraction and Dedup" (body-hash
// fallback where the provider supplies no delivery id).
func TestGitHubMissingDeliveryIDFallsBackToBodyHash(t *testing.T) {
	const secret = "s3cr3t"
	ing, _, pool, ctx, _ := testIngestDeps(t, Config{GitHubSecret: secret})

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
	ing, hub, pool, ctx, endpointID := testIngestDeps(t, Config{
		Generic: map[string]GenericProvider{
			"dockerhub": {Mode: "token", Token: "tok", Queue: "builds"},
		},
	})
	// Endpoint-scoped subscription (see TestGitHubRedeliveryDedup): the owning endpoint is what makes
	// the drainHub counts below mean anything.
	ch, cancel := hub.Subscribe(endpointID, []string{"builds"})
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

// grantFriendEdge records an APPROVED friend edge from fromHuman to toHuman, so a cross-human
// webhook route between them is genuinely authorized.
//
// This exists because ResolveWebhookTargets re-evaluates authorization on EVERY delivery, not just
// at grant time (ADR-0022, SPEC-0010 REQ "Per-Direction, Revocable, Non-Transitive Edges"): a
// cross-human route with no approved edge is dropped from the fan-out. A fixture that wires such a
// route with a bare AddWebhookRoute call is therefore not modelling a state the system can reach —
// add_webhook_route refuses to create it (mcp.authorizeRouteTarget) — and any fan-out it asserted
// would be testing a delivery that production would never make. Seeding the edge keeps these tests
// cross-HUMAN (which is the point: the tenant boundary is between humans) while making the route
// one the authorization layer would actually have granted.
//
// The edge direction is requester→target, matching FriendEdgeAuthorizesDelivery: an approved
// fromHuman→toHuman edge means "fromHuman may hand work to toHuman", which is exactly what routing
// fromHuman's webhook into toHuman's endpoint does.
func grantFriendEdge(t *testing.T, st *store.Store, ctx context.Context, label, fromHuman, toHuman string) {
	t.Helper()
	edge, err := st.CreateFriendRequest(ctx, store.CreateFriendRequestParams{
		FromPersona: "from@" + label, ToPersona: "to@" + label,
		FromHuman: fromHuman, ToHuman: toHuman,
		RequestedQueues: []string{"reviews"}, RequestedVerbs: []string{"create_for"},
	})
	if err != nil {
		t.Fatalf("create friend request (%s): %v", label, err)
	}
	// Approval is the vend, so it needs one of the TARGET human's agents to mint onto. This agent is
	// incidental to the fan-out under test — the route targets an endpoint seeded separately.
	ag, err := st.CreateAgent(ctx, toHuman, "friend-agent-"+label, "")
	if err != nil {
		t.Fatalf("create friend vend agent (%s): %v", label, err)
	}
	slug, err := store.MintSlug(ag.Name)
	if err != nil {
		t.Fatalf("mint friend slug (%s): %v", label, err)
	}
	if _, _, err := st.ApproveFriendRequest(ctx, store.ApproveFriendRequestParams{
		EdgeID: edge.ID, OwnerHumanID: toHuman, AgentID: ag.ID,
		CredentialHash: "friendhash-" + label, CredentialPrefix: "sbk_fr", Slug: slug,
	}); err != nil {
		t.Fatalf("approve friend edge (%s): %v", label, err)
	}
}
