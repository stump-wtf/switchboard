// End-to-end ownership-scoping tests through the real router + PostgreSQL: the dashboard lists
// only the session human's agents, an unowned agent detail is an existence-hiding 404, and logout
// revokes the server-side session. Skipped without SWITCHBOARD_TEST_DATABASE_URL (Gitea CI runs
// DB-less; the GitHub mirror provides Postgres), matching the store test pattern.
// Governing: SPEC-0012 REQ "Screen Set and Routes" (ownership scoping, unowned → 404),
// SPEC-0008 REQ "Session-Gated Human Surface".
package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/joestump/switchboard/internal/auth"
	"github.com/joestump/switchboard/internal/config"
	"github.com/joestump/switchboard/internal/db"
	"github.com/joestump/switchboard/internal/ingest"
	"github.com/joestump/switchboard/internal/store"
	"github.com/joestump/switchboard/internal/web"
)

// sessionCookieName is the session cookie's wire name — a stable browser-facing contract
// (SPEC-0008), asserted here from outside the auth package on purpose.
const sessionCookieName = "sb_session"

// newDBRouter builds the production router against a package-dedicated test database (derived from
// SWITCHBOARD_TEST_DATABASE_URL), migrated and truncated. `go test ./...` runs packages in
// parallel, and the store/ingest/db packages each truncate the shared test database mid-run;
// carving out a separate database keeps these end-to-end tests (and theirs) deterministic.
func newDBRouter(t *testing.T) (chi.Router, *store.Store, context.Context) {
	t.Helper()
	r, st, ctx, _ := newDBRouterOpts(t, false)
	return r, st, ctx
}

// newReceiverDBRouter is newDBRouter for tests that drive an OPERATOR-CONFIGURED receiver
// (/webhooks/github, /webhooks/generic/{name}, …). Those receivers are not vended to an agent, so
// nothing in their configuration names a tenant — yet todos.endpoint_id is NOT NULL with no
// sentinel, so their owner has to be stated once by the operator. Unset, every such delivery
// answers 503 ("receiver not configured") instead of minting a todo, which would silently hollow
// out any test that posts one. This harness vends that owning endpoint exactly as an operator would
// set SWITCHBOARD_LEGACY_RECEIVER_ENDPOINT_ID, and returns it so the test can assert ownership.
//
// It is a SEPARATE harness on purpose: seeding an endpoint is not free. Vending one puts an agent,
// an endpoint and a queue in the store, and surfaces that read the store's queue vocabulary (the
// vend wizard's queues step) render differently against a non-empty store. Only the tests that need
// a configured receiver pay that cost. INTERIM: dies with those receivers in PR 2.
// Governing: ADR-0022, SPEC-0001 REQ "Enqueue Accepted Delivery as Endpoint-Owned Todo".
func newReceiverDBRouter(t *testing.T) (chi.Router, *store.Store, context.Context, store.Endpoint) {
	t.Helper()
	return newDBRouterOpts(t, true)
}

func newDBRouterOpts(t *testing.T, withReceiverEndpoint bool) (chi.Router, *store.Store, context.Context, store.Endpoint) {
	t.Helper()
	dsn := os.Getenv("SWITCHBOARD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set SWITCHBOARD_TEST_DATABASE_URL to run ownership-scoping tests")
	}
	ctx := context.Background()
	const serverTestDB = "switchboard_test_server"
	admin, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect (admin): %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+serverTestDB); err != nil &&
		!strings.Contains(err.Error(), "42P04") { // duplicate_database: already provisioned
		admin.Close()
		t.Fatalf("create test database: %v", err)
	}
	admin.Close()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse test dsn: %v", err)
	}
	u.Path = "/" + serverTestDB
	pool, err := db.Connect(ctx, u.String())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`TRUNCATE humans, agents, endpoints, todos, events, sessions, adapters, oauth_clients RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	st := store.New(pool)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Config{BaseURL: "https://sb.example.com"}
	authr, err := auth.New(ctx, cfg, st, log)
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	webh, err := web.New(st, cfg, log)
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	// The personas capability has landed (store + well-known route wired), so the DB-backed router
	// enables it exactly as Run does — the Personas view and its routes are live for these tests.
	webh.SetPersonasEnabled(true)
	// See newReceiverDBRouter: only receiver-driving tests vend the operator's owning endpoint.
	var legacyEP store.Endpoint
	if withReceiverEndpoint {
		receiverOwner, err := st.UpsertHuman(ctx, "legacy-receiver-owner", "Receiver Owner", "receiver@example.com")
		if err != nil {
			t.Fatalf("upsert receiver-owner human: %v", err)
		}
		legacyEP = seedEndpoint(t, st, ctx, receiverOwner.ID, "legacy-receiver", "hash-legacy", "sbk_legacy")
	}

	hub := ingest.NewHub()
	r := newRouter(routerDeps{
		st:    st,
		authr: authr,
		webh:  webh,
		ing:   ingest.New(st, hub, log, ingest.Config{LegacyEndpointID: legacyEP.ID}),
		ping:  pool.Ping,
		log:   log,
	})
	return r, st, ctx, legacyEP
}

// mintSession creates a human plus a live server-side session and returns the plaintext cookie
// token. Sessions store only the SHA-256 hex of the token (SPEC-0008 "Session cookie carries only
// an opaque token"); the test mints against that stored-hash contract.
func mintSession(t *testing.T, st *store.Store, ctx context.Context, subject, name, email string) (store.Human, string) {
	t.Helper()
	h, err := st.UpsertHuman(ctx, subject, name, email)
	if err != nil {
		t.Fatalf("upsert human: %v", err)
	}
	token := "test-session-" + subject
	sum := sha256.Sum256([]byte(token))
	if err := st.CreateSession(ctx, hex.EncodeToString(sum[:]), h.ID, time.Hour); err != nil {
		t.Fatalf("create session: %v", err)
	}
	return h, token
}

func getAs(t *testing.T, r chi.Router, token, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

var csrfRe = regexp.MustCompile(`name="csrf_token" value="([0-9a-f]+)"`)

// scrapeCSRF pulls the per-session CSRF synchronizer token out of a rendered form, the same way a
// browser would submit it.
func scrapeCSRF(t *testing.T, body string) string {
	t.Helper()
	m := csrfRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no csrf_token field in page: %.300s", body)
	}
	return m[1]
}

// vendFixture creates an agent + a vended endpoint for a human directly through the store, for the
// Endpoints-view scoping tests. The credential material is a fixed non-secret stand-in.
func vendFixture(t *testing.T, st *store.Store, ctx context.Context, ownerHumanID, agentName, hash, prefix string) {
	t.Helper()
	seedEndpoint(t, st, ctx, ownerHumanID, agentName, hash, prefix, "reviews")
}

// seedEndpoint vends an agent + endpoint and returns the endpoint, which is the TENANT every todo
// the test seeds must name: todos.endpoint_id is NOT NULL with no sentinel, so a todo cannot exist
// without a real endpoint row to own it, and two endpoints sharing a queue name are two distinct
// tenants rather than one shared pool. Server-side tests therefore start by minting an endpoint the
// way an operator vends one, then hand its id to CreateTodo/CreateEventTodo (or to
// ingest.Config.LegacyEndpointID for the operator-configured receivers).
// Governing: ADR-0022, SPEC-0003 REQ "Endpoint Ownership (Tenant Isolation)".
func seedEndpoint(t *testing.T, st *store.Store, ctx context.Context, ownerHumanID, agentName, hash, prefix string, queues ...string) store.Endpoint {
	t.Helper()
	if len(queues) == 0 {
		queues = []string{"reviews"}
	}
	ag, err := st.CreateAgent(ctx, ownerHumanID, agentName, "")
	if err != nil {
		t.Fatalf("create agent %s: %v", agentName, err)
	}
	slug, err := store.MintSlug(agentName)
	if err != nil {
		t.Fatalf("mint slug: %v", err)
	}
	ep, err := st.CreateEndpoint(ctx, ag.ID, hash, prefix, slug, queues, []string{"claim"})
	if err != nil {
		t.Fatalf("vend endpoint for %s: %v", agentName, err)
	}
	return ep
}

// TestEndpointsListsOnlySessionHumansEndpoints: two humans, one vended endpoint each; each session
// sees exactly its own endpoint card on GET /endpoints — the handler scopes by the human id from
// request context, never by anything client-supplied. SPEC-0013: the Endpoints view lists only the
// authenticated human's endpoints.
func TestEndpointsListsOnlySessionHumansEndpoints(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	humanA, tokenA := mintSession(t, st, ctx, "test|alice", "Alice Ames", "alice@example.com")
	humanB, tokenB := mintSession(t, st, ctx, "test|bob", "Bob Byrne", "bob@example.com")
	vendFixture(t, st, ctx, humanA.ID, "alice-reviewer", "hash-a", "sbk_alice0")
	vendFixture(t, st, ctx, humanB.ID, "bob-deployer", "hash-b", "sbk_bob000")

	recA := getAs(t, r, tokenA, "/endpoints")
	if recA.Code != http.StatusOK {
		t.Fatalf("GET /endpoints as A: got %d, want 200", recA.Code)
	}
	bodyA := recA.Body.String()
	if !strings.Contains(bodyA, "alice-reviewer") {
		t.Error("A's endpoints view must list A's endpoint")
	}
	if strings.Contains(bodyA, "bob-deployer") {
		t.Error("A's endpoints view must NOT list B's endpoint")
	}

	recB := getAs(t, r, tokenB, "/endpoints")
	if recB.Code != http.StatusOK {
		t.Fatalf("GET /endpoints as B: got %d, want 200", recB.Code)
	}
	bodyB := recB.Body.String()
	if !strings.Contains(bodyB, "bob-deployer") {
		t.Error("B's endpoints view must list B's endpoint")
	}
	if strings.Contains(bodyB, "alice-reviewer") {
		t.Error("B's endpoints view must NOT list A's endpoint")
	}
}

// TestRetiredAgentRoutesRedirectToEndpoints: the SPEC-0012 dashboard/agent routes were folded into
// the Endpoints view, so GET /agents and GET /agents/{id} 303-redirect to /endpoints. Because the
// redirect performs no store lookup, it can never leak whether an agent id exists — every id lands
// on the same view. SPEC-0013 REQ "Endpoints View and Vend Modal" (routes fold into /endpoints).
func TestRetiredAgentRoutesRedirectToEndpoints(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	_, token := mintSession(t, st, ctx, "test|alice", "Alice Ames", "alice@example.com")

	for _, path := range []string{
		"/agents",
		"/agents/00000000-0000-0000-0000-000000000000",
		"/agents/not-a-uuid",
	} {
		rec := getAs(t, r, token, path)
		if rec.Code != http.StatusSeeOther {
			t.Errorf("GET %s: got %d, want 303", path, rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != "/endpoints" {
			t.Errorf("GET %s: Location %q, want /endpoints", path, loc)
		}
	}
}

// TestLogoutRevokesSessionAndClearsCookie: POST /logout (with the session's CSRF token, as the
// rendered form submits it) clears the cookie AND revokes the server-side session, so replaying
// the old cookie is redirected to /login. SPEC-0008 scenario "Logout revokes the session".
func TestLogoutRevokesSessionAndClearsCookie(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	_, token := mintSession(t, st, ctx, "test|alice", "Alice Ames", "alice@example.com")

	// Authenticated before logout; scrape the CSRF token from the rendered Endpoints view.
	rec := getAs(t, r, token, "/endpoints")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /endpoints pre-logout: got %d, want 200", rec.Code)
	}
	csrf := scrapeCSRF(t, rec.Body.String())

	form := url.Values{"csrf_token": {csrf}}
	req := httptest.NewRequest(http.MethodPost, "/logout", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	out := httptest.NewRecorder()
	r.ServeHTTP(out, req)
	if out.Code != http.StatusFound {
		t.Fatalf("POST /logout: got %d, want 302", out.Code)
	}
	var cleared bool
	for _, c := range out.Result().Cookies() {
		if c.Name == sessionCookieName && c.Value == "" && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("logout must clear the session cookie (empty value, negative Max-Age)")
	}
	// The server-side session is gone: the old cookie no longer authenticates.
	if rec := getAs(t, r, token, "/endpoints"); rec.Code != http.StatusFound || rec.Header().Get("Location") != "/login" {
		t.Fatalf("replayed cookie after logout: got %d → %q, want 302 → /login", rec.Code, rec.Header().Get("Location"))
	}
}
