package mcp

// tools/call integration tests for the ADR-0022 webhook ROUTING verbs: add_webhook_route,
// list_webhook_routes, remove_webhook_route. These run the real SDK client and server over HTTP
// against the REAL store and a REAL PostgreSQL database — not the fake — because the property under
// test is an authorization decision made out of joined rows (endpoint → agent → owner human, and the
// per-direction friend edge). A fake that answered those joins from a map would be asserting my own
// idea of the schema back at me; the friend-edge DIRECTION in particular is only meaningfully tested
// against the real table and the real lifecycle transitions.
//
// Skips cleanly without SWITCHBOARD_TEST_DATABASE_URL, in the house style (Gitea CI runs DB-less).
//
// Governing: ADR-0022, SPEC-0001 REQ "Deterministic Route Fan-Out (Token-Free)",
// SPEC-0006 REQ "Webhook Route Fan-Out Under Ownership and Friendship", ADR-0010, ADR-0008.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/joestump/switchboard/internal/cred"
	"github.com/joestump/switchboard/internal/db"
	"github.com/joestump/switchboard/internal/store"
)

// --- fakeStore routing stubs ---------------------------------------------------------------------
//
// fakeStore (mcp_test.go) satisfies ToolStore for every OTHER test in this package, so it has to
// carry the routing methods too. It deliberately models nothing: routing authorization is a set of
// SQL joins, and a map-backed imitation of them would pass while the real predicates were wrong.
// Every routing assertion in this file runs against the real store instead. A fake session that
// calls a routing verb therefore gets a clean not_found rather than a misleading success.

func (f *fakeStore) AddWebhookRoute(_ context.Context, _, _, _ string) error {
	return store.ErrNotFound
}

func (f *fakeStore) RemoveWebhookRoute(_ context.Context, _, _ string) error {
	return store.ErrNotFound
}

func (f *fakeStore) ListWebhookRoutes(_ context.Context, _ string) ([]store.WebhookRoute, error) {
	return nil, nil
}

func (f *fakeStore) WebhookOwnerEndpointForHuman(_ context.Context, _, _ string) (string, error) {
	return "", store.ErrNotFound
}

func (f *fakeStore) EndpointOwnerHuman(_ context.Context, _ string) (string, error) {
	return "", store.ErrNotFound
}

func (f *fakeStore) FriendEdgeAuthorizesDelivery(_ context.Context, _, _ string) (bool, error) {
	return false, nil
}

// scopedTodo is the fake's tenant lookup: a todo whose EndpointID is pinned (ADR-0022) is visible
// only to that endpoint. Fixtures that leave EndpointID empty pre-date the pin and pass through, so
// this adds isolation where a test asks for it without silently rewriting tests that do not.
func (f *fakeStore) scopedTodo(endpointID, id string) (store.Todo, bool) {
	t, ok := f.todos[id]
	if !ok {
		return store.Todo{}, false
	}
	if t.EndpointID != "" && t.EndpointID != endpointID {
		return store.Todo{}, false
	}
	return t, true
}

// --- real-database fixture -----------------------------------------------------------------------

// routeTestPool connects to an mcp-package-OWNED database derived from SWITCHBOARD_TEST_DATABASE_URL
// (created on first use), migrated and truncated. `go test ./...` runs packages in parallel against
// one DSN, so a dedicated database keeps this package from racing store's and ingest's truncates.
func routeTestPool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := os.Getenv("SWITCHBOARD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set SWITCHBOARD_TEST_DATABASE_URL to run the webhook-route authorization tests")
	}
	ctx := context.Background()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse test dsn: %v", err)
	}
	const testDB = "switchboard_mcproute_test"
	if u.Path != "/"+testDB {
		admin, err := db.Connect(ctx, dsn)
		if err != nil {
			t.Fatalf("connect (admin): %v", err)
		}
		if _, err := admin.Exec(ctx, `CREATE DATABASE `+testDB); err != nil {
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "42P04" { // duplicate_database
				admin.Close()
				t.Fatalf("create mcp route test database: %v", err)
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
	// The test database persists across runs; clear every table these fixtures seed so a repeat run
	// does not trip the endpoint credential-hash or friend-edge live-pair unique indexes.
	if _, err := pool.Exec(ctx, `TRUNCATE humans, agents, endpoints, endpoint_webhooks,
		webhook_routes, friend_edges, todos, events RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return pool, ctx
}

// allRouteVerbs is the full routing surface, granted to the caller endpoint in most tests.
var allRouteVerbs = []string{"add_webhook_route", "list_webhook_routes", "remove_webhook_route"}

// routeFixture is the two-human world these tests reason about: human A drives the session and owns
// the webhook; human B is the stranger whose endpoint A may only reach across an approved friend
// edge. A owns TWO endpoints so "route to my own second endpoint" is expressible.
type routeFixture struct {
	st *store.Store

	humanA, humanB     string
	agentA1, agentA2   string
	agentB             string
	epA1, epA2, epB    string // endpoint ids
	slugA1             string
	tokenA1            string
	webhookA, webhookB string // webhook ids owned by epA1 / epB
}

func newRouteFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *routeFixture {
	t.Helper()
	st := store.New(pool)
	f := &routeFixture{st: st}

	f.humanA = mustHuman(t, ctx, st, "sub-a", "Human A")
	f.humanB = mustHuman(t, ctx, st, "sub-b", "Human B")
	f.agentA1 = mustAgent(t, ctx, st, f.humanA, "agent-a1")
	f.agentA2 = mustAgent(t, ctx, st, f.humanA, "agent-a2")
	f.agentB = mustAgent(t, ctx, st, f.humanB, "agent-b")

	f.slugA1 = "agent-a1-11111111"
	f.epA1, f.tokenA1 = mustEndpoint(t, ctx, st, f.agentA1, f.slugA1, allRouteVerbs)
	f.epA2, _ = mustEndpoint(t, ctx, st, f.agentA2, "agent-a2-22222222", allRouteVerbs)
	f.epB, _ = mustEndpoint(t, ctx, st, f.agentB, "agent-b-33333333", allRouteVerbs)

	f.webhookA = mustWebhook(t, ctx, st, f.epA1, "tok-a")
	f.webhookB = mustWebhook(t, ctx, st, f.epB, "tok-b")
	return f
}

func mustHuman(t *testing.T, ctx context.Context, st *store.Store, subject, name string) string {
	t.Helper()
	h, err := st.UpsertHuman(ctx, subject, name, subject+"@example.test")
	if err != nil {
		t.Fatalf("upsert human %s: %v", subject, err)
	}
	return h.ID
}

func mustAgent(t *testing.T, ctx context.Context, st *store.Store, humanID, name string) string {
	t.Helper()
	a, err := st.CreateAgent(ctx, humanID, name, "")
	if err != nil {
		t.Fatalf("create agent %s: %v", name, err)
	}
	return a.ID
}

// mustEndpoint vends an endpoint and returns its id plus the bearer token that authenticates to it.
func mustEndpoint(t *testing.T, ctx context.Context, st *store.Store, agentID, slug string, verbs []string) (string, string) {
	t.Helper()
	token, hash, prefix, err := cred.Mint()
	if err != nil {
		t.Fatalf("mint credential: %v", err)
	}
	ep, err := st.CreateEndpoint(ctx, agentID, hash, prefix, slug, []string{"reviews"}, verbs)
	if err != nil {
		t.Fatalf("create endpoint %s: %v", slug, err)
	}
	return ep.ID, token
}

func mustWebhook(t *testing.T, ctx context.Context, st *store.Store, endpointID, ingestToken string) string {
	t.Helper()
	w, err := st.CreateWebhook(ctx, endpointID, "github", "reviews", "signed", ingestToken, "whsec_test", 10)
	if err != nil {
		t.Fatalf("create webhook on %s: %v", endpointID, err)
	}
	return w.ID
}

// routeSession serves the MCP surface over the REAL store and connects a client authenticated as the
// endpoint the token belongs to.
func routeSession(t *testing.T, ctx context.Context, st *store.Store, slug, token string) *sdk.ClientSession {
	t.Helper()
	h := New(st, slog.New(slog.NewTextHandler(discardWriter{}, nil)))
	r := chi.NewRouter()
	r.Mount("/mcp", h.Routes())
	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)
	t.Cleanup(h.Close)
	h.SetBaseURL("https://switchboard.example")

	client := sdk.NewClient(&sdk.Implementation{Name: "route-test-client", Version: "0.0.1"}, nil)
	cs, err := client.Connect(ctx, &sdk.StreamableClientTransport{
		Endpoint:   ts.URL + "/mcp/" + slug,
		HTTPClient: &http.Client{Transport: bearerTransport{token}},
	}, nil)
	if err != nil {
		t.Fatalf("initialize handshake: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// friendRequest opens a PENDING edge from fromHuman to toHuman. Per-direction: this is "fromHuman
// asks to hand work to toHuman" (0005_friend_edges.sql).
func friendRequest(t *testing.T, ctx context.Context, st *store.Store, fromPersona, toPersona, fromHuman, toHuman string) string {
	t.Helper()
	e, err := st.CreateFriendRequest(ctx, store.CreateFriendRequestParams{
		FromPersona: fromPersona, ToPersona: toPersona,
		FromHuman: fromHuman, ToHuman: toHuman,
		RequestedQueues: []string{"reviews"}, RequestedVerbs: []string{"create_for"},
	})
	if err != nil {
		t.Fatalf("create friend request %s->%s: %v", fromPersona, toPersona, err)
	}
	return e.ID
}

// approveFriend approves a pending edge as the TARGET human (the only human who may), minting the
// vended endpoint under one of that human's agents — approval is the vend (ADR-0008/SPEC-0010).
func approveFriend(t *testing.T, ctx context.Context, st *store.Store, edgeID, toHuman, vendAgentID, slug string) {
	t.Helper()
	_, hash, prefix, err := cred.Mint()
	if err != nil {
		t.Fatalf("mint friend credential: %v", err)
	}
	if _, _, err := st.ApproveFriendRequest(ctx, store.ApproveFriendRequestParams{
		EdgeID: edgeID, OwnerHumanID: toHuman, AgentID: vendAgentID,
		CredentialHash: hash, CredentialPrefix: prefix, Slug: slug,
	}); err != nil {
		t.Fatalf("approve friend edge: %v", err)
	}
}

// --- tests ---------------------------------------------------------------------------------------

// TestAddWebhookRouteToOwnEndpointFansOut is the headline happy path from Joe's requirement: an agent
// creates a webhook and routes it to itself — here, to a second endpoint its own human owns, which is
// the only version of "to itself" that is observable (the owning endpoint is already an implicit
// target). It then proves the route actually CHANGES DELIVERY: ResolveWebhookTargets grows to two
// endpoints, a delivery mints one endpoint-owned todo per target, list_webhook_routes reports the
// route, and remove_webhook_route drops the fan-out back to owner-only.
// Governing: SPEC-0001 REQ "Deterministic Route Fan-Out (Token-Free)", ADR-0022.
func TestAddWebhookRouteToOwnEndpointFansOut(t *testing.T) {
	pool, ctx := routeTestPool(t)
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	f := newRouteFixture(t, ctx, pool)
	cs := routeSession(t, ctx, f.st, f.slugA1, f.tokenA1)

	// Before any route the webhook fans out to its owner alone.
	if got := mustTargets(t, ctx, f, f.webhookA); !slices.Equal(got, []string{f.epA1}) {
		t.Fatalf("initial targets = %v, want [%s]", got, f.epA1)
	}

	var added webhookRouteOut
	callOK(t, ctx, cs, "add_webhook_route",
		map[string]any{"webhook_id": f.webhookA, "target_endpoint_id": f.epA2}, &added)
	if !added.Routed || added.TargetEndpointID != f.epA2 {
		t.Fatalf("add_webhook_route = %+v, want routed to %s", added, f.epA2)
	}

	// The fan-out set now names both endpoints, owner first.
	targets := mustTargets(t, ctx, f, f.webhookA)
	if !slices.Equal(targets, []string{f.epA1, f.epA2}) {
		t.Fatalf("targets after route = %v, want [%s %s]", targets, f.epA1, f.epA2)
	}

	// A delivery over that target set mints one todo per target, each PINNED to its own endpoint —
	// the whole point of the route (ADR-0022: no shared-queue visibility, ever).
	_, created, err := f.st.CreateEventTodos(ctx,
		store.EventInput{Source: "github", Family: "webhook", TrustMode: "signed", Verified: true,
			ExternalID: "delivery-1", Payload: []byte(`{"action":"opened"}`)},
		targets,
		store.CreateTodoParams{Queue: "reviews", Source: "github", Title: "PR opened",
			IdempotencyKey: "wh:delivery-1"})
	if err != nil {
		t.Fatalf("create event todos: %v", err)
	}
	if len(created) != 2 {
		t.Fatalf("created %d todos, want 2 (one per target)", len(created))
	}
	var owners []string
	for _, ct := range created {
		if !ct.New {
			t.Fatalf("todo %s reported as not new on a first delivery", ct.Todo.ID)
		}
		owners = append(owners, ct.Todo.EndpointID)
	}
	sort.Strings(owners)
	want := []string{f.epA1, f.epA2}
	sort.Strings(want)
	if !slices.Equal(owners, want) {
		t.Fatalf("todo owners = %v, want %v", owners, want)
	}

	// list_webhook_routes reports the explicit route AND the implicit owner target.
	var listed listWebhookRoutesOut
	callOK(t, ctx, cs, "list_webhook_routes", map[string]any{"webhook_id": f.webhookA}, &listed)
	if listed.OwnerEndpointID != f.epA1 {
		t.Fatalf("owner_endpoint_id = %q, want %q", listed.OwnerEndpointID, f.epA1)
	}
	if len(listed.Routes) != 1 || listed.Routes[0].TargetEndpointID != f.epA2 {
		t.Fatalf("routes = %+v, want one route to %s", listed.Routes, f.epA2)
	}

	// Removing drops the fan-out back to owner-only.
	var removed removeWebhookRouteOut
	callOK(t, ctx, cs, "remove_webhook_route",
		map[string]any{"webhook_id": f.webhookA, "target_endpoint_id": f.epA2}, &removed)
	if !removed.Removed {
		t.Fatalf("remove_webhook_route = %+v, want removed", removed)
	}
	if got := mustTargets(t, ctx, f, f.webhookA); !slices.Equal(got, []string{f.epA1}) {
		t.Fatalf("targets after remove = %v, want [%s]", got, f.epA1)
	}
}

// TestAddWebhookRouteCrossHumanRequiresApprovedFriendEdge walks one edge through its whole lifecycle
// against the same target, asserting the ONLY state that authorizes delivery is `approved`. Reusing
// one edge (rather than three separate pairs) also respects the live-edge unique index and mirrors
// how a real friendship actually moves. Governing: ADR-0010, SPEC-0010 REQ "Per-Direction,
// Revocable, Non-Transitive Edges" (a pending edge grants nothing; revocation is terminal).
func TestAddWebhookRouteCrossHumanRequiresApprovedFriendEdge(t *testing.T) {
	pool, ctx := routeTestPool(t)
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	f := newRouteFixture(t, ctx, pool)
	cs := routeSession(t, ctx, f.st, f.slugA1, f.tokenA1)

	args := map[string]any{"webhook_id": f.webhookA, "target_endpoint_id": f.epB}

	// 1. No edge at all.
	callErr(t, ctx, cs, "add_webhook_route", args, "forbidden")
	assertNoRoute(t, ctx, f, f.webhookA, f.epB)

	// 2. Pending edge — a request grants nothing until a human acts on it.
	edgeID := friendRequest(t, ctx, f.st, "persona-a", "persona-b", f.humanA, f.humanB)
	callErr(t, ctx, cs, "add_webhook_route", args, "forbidden")
	assertNoRoute(t, ctx, f, f.webhookA, f.epB)

	// 3. Approved — B consented to receive A's work, so the route is allowed.
	approveFriend(t, ctx, f.st, edgeID, f.humanB, f.agentB, "friend-a-44444444")
	var added webhookRouteOut
	callOK(t, ctx, cs, "add_webhook_route", args, &added)
	if got := mustTargets(t, ctx, f, f.webhookA); !slices.Equal(got, []string{f.epA1, f.epB}) {
		t.Fatalf("targets after approved route = %v, want [%s %s]", got, f.epA1, f.epB)
	}

	// 4. Revoked — terminal. The existing route survives (removing it is the owner's call, and the
	// delivery path is where a dead endpoint stops mattering), but NO NEW route may be granted.
	if _, err := f.st.RevokeFriendEdge(ctx, edgeID, f.humanB); err != nil {
		t.Fatalf("revoke friend edge: %v", err)
	}
	if err := f.st.RemoveWebhookRoute(ctx, f.webhookA, f.epB); err != nil {
		t.Fatalf("remove route: %v", err)
	}
	callErr(t, ctx, cs, "add_webhook_route", args, "forbidden")
	assertNoRoute(t, ctx, f, f.webhookA, f.epB)
}

// TestAddWebhookRouteWrongDirectionEdgeForbidden is the privilege-escalation guard. B asks to hand
// work to A and A approves: the approved edge is B→A. That says nothing about A handing work to B,
// so A routing ITS webhook into B's endpoint MUST still be refused. An implementation that checked
// (from_human = target, to_human = caller) would pass every other test in this file and fail only
// this one. Governing: store.FriendEdgeAuthorizesDelivery, SPEC-0010 REQ "Per-Direction, Revocable,
// Non-Transitive Edges".
func TestAddWebhookRouteWrongDirectionEdgeForbidden(t *testing.T) {
	pool, ctx := routeTestPool(t)
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	f := newRouteFixture(t, ctx, pool)
	cs := routeSession(t, ctx, f.st, f.slugA1, f.tokenA1)

	// B → A, approved by A. The grant flows B into A, the opposite of what A now attempts.
	edgeID := friendRequest(t, ctx, f.st, "persona-b", "persona-a", f.humanB, f.humanA)
	approveFriend(t, ctx, f.st, edgeID, f.humanA, f.agentA1, "friend-b-55555555")

	callErr(t, ctx, cs, "add_webhook_route",
		map[string]any{"webhook_id": f.webhookA, "target_endpoint_id": f.epB}, "forbidden")
	assertNoRoute(t, ctx, f, f.webhookA, f.epB)

	// And the direction that IS authorized stays authorized: B may route into A. Proving both halves
	// rules out an implementation that simply refuses everything cross-human.
	ok, err := f.st.FriendEdgeAuthorizesDelivery(ctx, f.humanB, f.humanA)
	if err != nil {
		t.Fatalf("friend edge authorizes delivery: %v", err)
	}
	if !ok {
		t.Fatal("approved B->A edge must authorize B delivering into A")
	}
	if ok, err := f.st.FriendEdgeAuthorizesDelivery(ctx, f.humanA, f.humanB); err != nil || ok {
		t.Fatalf("A->B authorized = %v (err %v), want false — the edge is B->A", ok, err)
	}
}

// TestWebhookRouteOnUnownedWebhookIsIndistinguishableFromUnknown: routing a webhook the caller does
// not own MUST fail, and MUST fail identically to routing a webhook id that does not exist at all —
// otherwise the verb confirms that another human's webhook is real. Governing: ADR-0022,
// SPEC-0006 REQ "Structured Output and Stable Error Shape".
func TestWebhookRouteOnUnownedWebhookIsIndistinguishableFromUnknown(t *testing.T) {
	pool, ctx := routeTestPool(t)
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	f := newRouteFixture(t, ctx, pool)
	cs := routeSession(t, ctx, f.st, f.slugA1, f.tokenA1)

	const absent = "6ba7b810-9dad-11d1-80b4-00c04fd430c8" // well-formed, and no such webhook

	// B's real webhook and a webhook that was never created must answer byte-identically, on every
	// routing verb — add, list, and remove alike.
	for _, verb := range []string{"add_webhook_route", "remove_webhook_route"} {
		unowned := callErr(t, ctx, cs, verb,
			map[string]any{"webhook_id": f.webhookB, "target_endpoint_id": f.epA2}, "not_found")
		unknown := callErr(t, ctx, cs, verb,
			map[string]any{"webhook_id": absent, "target_endpoint_id": f.epA2}, "not_found")
		if unowned != unknown {
			t.Fatalf("%s leaks existence: unowned %q vs unknown %q", verb, unowned, unknown)
		}
	}
	unowned := callErr(t, ctx, cs, "list_webhook_routes", map[string]any{"webhook_id": f.webhookB}, "not_found")
	unknown := callErr(t, ctx, cs, "list_webhook_routes", map[string]any{"webhook_id": absent}, "not_found")
	if unowned != unknown {
		t.Fatalf("list_webhook_routes leaks existence: unowned %q vs unknown %q", unowned, unknown)
	}

	// Nothing was written against B's webhook.
	routes, err := f.st.ListWebhookRoutes(ctx, f.webhookB)
	if err != nil {
		t.Fatalf("list B routes: %v", err)
	}
	if len(routes) != 0 {
		t.Fatalf("B's webhook gained %d routes from A's calls", len(routes))
	}
}

// TestRemoveWebhookRouteOnUnownedWebhookLeavesItIntact: A may not tear down a route on B's webhook.
// The refusal is not_found (same non-leaking shape as above) and B's route survives untouched.
func TestRemoveWebhookRouteOnUnownedWebhookLeavesItIntact(t *testing.T) {
	pool, ctx := routeTestPool(t)
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	f := newRouteFixture(t, ctx, pool)
	cs := routeSession(t, ctx, f.st, f.slugA1, f.tokenA1)

	// B routes its own webhook to a second endpoint of B's — wired directly, since this test is
	// about A's inability to REMOVE it.
	agentB2 := mustAgent(t, ctx, f.st, f.humanB, "agent-b2")
	epB2, _ := mustEndpoint(t, ctx, f.st, agentB2, "agent-b2-66666666", allRouteVerbs)
	if err := f.st.AddWebhookRoute(ctx, f.webhookB, epB2, f.humanB); err != nil {
		t.Fatalf("seed B route: %v", err)
	}

	callErr(t, ctx, cs, "remove_webhook_route",
		map[string]any{"webhook_id": f.webhookB, "target_endpoint_id": epB2}, "not_found")

	if got := mustTargets(t, ctx, f, f.webhookB); !slices.Equal(got, []string{f.epB, epB2}) {
		t.Fatalf("B's targets = %v after A's removal attempt, want [%s %s]", got, f.epB, epB2)
	}
}

// TestAddWebhookRouteIsIdempotent: a fan-out target set is a SET, so "route this webhook there"
// states an end condition. A retry succeeds and leaves exactly one row — an agent replaying a call
// after a timeout must not get a conflict or a duplicate target.
func TestAddWebhookRouteIsIdempotent(t *testing.T) {
	pool, ctx := routeTestPool(t)
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	f := newRouteFixture(t, ctx, pool)
	cs := routeSession(t, ctx, f.st, f.slugA1, f.tokenA1)

	args := map[string]any{"webhook_id": f.webhookA, "target_endpoint_id": f.epA2}
	var first, second webhookRouteOut
	callOK(t, ctx, cs, "add_webhook_route", args, &first)
	callOK(t, ctx, cs, "add_webhook_route", args, &second)
	if first != second {
		t.Fatalf("repeat add = %+v, want the same result as %+v", second, first)
	}

	routes, err := f.st.ListWebhookRoutes(ctx, f.webhookA)
	if err != nil {
		t.Fatalf("list routes: %v", err)
	}
	if len(routes) != 1 {
		t.Fatalf("routes after double add = %d, want 1", len(routes))
	}
	if got := mustTargets(t, ctx, f, f.webhookA); !slices.Equal(got, []string{f.epA1, f.epA2}) {
		t.Fatalf("targets = %v, want the target set de-duplicated", got)
	}

	// Removing twice is equally idempotent — the second call states an already-true end condition.
	var r1, r2 removeWebhookRouteOut
	callOK(t, ctx, cs, "remove_webhook_route", args, &r1)
	callOK(t, ctx, cs, "remove_webhook_route", args, &r2)
	if !r1.Removed || !r2.Removed {
		t.Fatalf("repeat remove = %+v / %+v, want both removed", r1, r2)
	}
}

// TestAddWebhookRouteToUnknownTargetIsIndistinguishableFromUnfriended: the target side collapses
// every failure into one code. An endpoint id that does not exist and a real endpoint belonging to
// an unfriended human MUST answer identically, or the verb becomes a cross-tenant existence oracle.
// Governing: ADR-0022.
func TestAddWebhookRouteToUnknownTargetIsIndistinguishableFromUnfriended(t *testing.T) {
	pool, ctx := routeTestPool(t)
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	f := newRouteFixture(t, ctx, pool)
	cs := routeSession(t, ctx, f.st, f.slugA1, f.tokenA1)

	const absent = "6ba7b811-9dad-11d1-80b4-00c04fd430c8"
	stranger := callErr(t, ctx, cs, "add_webhook_route",
		map[string]any{"webhook_id": f.webhookA, "target_endpoint_id": f.epB}, "forbidden")
	unknown := callErr(t, ctx, cs, "add_webhook_route",
		map[string]any{"webhook_id": f.webhookA, "target_endpoint_id": absent}, "forbidden")
	malformed := callErr(t, ctx, cs, "add_webhook_route",
		map[string]any{"webhook_id": f.webhookA, "target_endpoint_id": "not-a-uuid"}, "forbidden")
	if stranger != unknown || stranger != malformed {
		t.Fatalf("target failures differ: stranger %q, unknown %q, malformed %q",
			stranger, unknown, malformed)
	}
}

// TestWebhookRouteVerbsScopeGated: the routing verbs obey the same allowlist gate as every other
// verb — tools/list advertises only granted ones, and calling an ungranted one is a stable scope
// error, not an unknown-tool protocol error. Governing: SPEC-0006 REQ "Scope Enforcement at the
// Boundary", SPEC-0014 REQ "Agent Tool Surface over MCP".
func TestWebhookRouteVerbsScopeGated(t *testing.T) {
	pool, ctx := routeTestPool(t)
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	f := newRouteFixture(t, ctx, pool)

	// A second endpoint of A's granted ONLY list_webhook_routes.
	narrowSlug := "agent-a1-77777777"
	_, narrowToken := mustEndpoint(t, ctx, f.st, f.agentA1, narrowSlug, []string{"list_webhook_routes"})
	cs := routeSession(t, ctx, f.st, narrowSlug, narrowToken)

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	var names []string
	for _, tool := range tools.Tools {
		if slices.Contains(allRouteVerbs, tool.Name) {
			names = append(names, tool.Name)
		}
	}
	if !slices.Equal(names, []string{"list_webhook_routes"}) {
		t.Fatalf("advertised routing tools = %v, want [list_webhook_routes]", names)
	}

	callErr(t, ctx, cs, "add_webhook_route",
		map[string]any{"webhook_id": f.webhookA, "target_endpoint_id": f.epA2}, "forbidden")
	callErr(t, ctx, cs, "remove_webhook_route",
		map[string]any{"webhook_id": f.webhookA, "target_endpoint_id": f.epA2}, "forbidden")
	assertNoRoute(t, ctx, f, f.webhookA, f.epA2)
}

// --- assertions ----------------------------------------------------------------------------------

func mustTargets(t *testing.T, ctx context.Context, f *routeFixture, webhookID string) []string {
	t.Helper()
	owner, err := f.st.WebhookOwnerEndpointForHuman(ctx, webhookID, f.humanA)
	if err != nil {
		// Not A's webhook — resolve against B, the only other human in the fixture.
		if owner, err = f.st.WebhookOwnerEndpointForHuman(ctx, webhookID, f.humanB); err != nil {
			t.Fatalf("resolve webhook owner: %v", err)
		}
	}
	targets, err := f.st.ResolveWebhookTargets(ctx, webhookID, owner)
	if err != nil {
		t.Fatalf("resolve webhook targets: %v", err)
	}
	return targets
}

func assertNoRoute(t *testing.T, ctx context.Context, f *routeFixture, webhookID, target string) {
	t.Helper()
	routes, err := f.st.ListWebhookRoutes(ctx, webhookID)
	if err != nil {
		t.Fatalf("list routes: %v", err)
	}
	for _, r := range routes {
		if r.TargetEndpointID == target {
			t.Fatalf("route to %s exists but must not", target)
		}
	}
}
