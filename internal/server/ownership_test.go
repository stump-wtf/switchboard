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

	"github.com/joestump/switchboard/internal/agentapi"
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
		`TRUNCATE humans, agents, endpoints, todos, events, sessions, adapters RESTART IDENTITY CASCADE`); err != nil {
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
	hub := agentapi.NewHub()
	r := newRouter(routerDeps{
		st:    st,
		authr: authr,
		webh:  webh,
		api:   agentapi.New(st, hub, log),
		ing:   ingest.New(st, hub, log, ingest.Config{}),
		ping:  pool.Ping,
		log:   log,
	})
	return r, st, ctx
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

// TestDashboardListsOnlySessionHumansAgents: two humans, one agent each; each session sees exactly
// its own agent on GET /agents — the handler scopes by the human id from request context, never by
// anything client-supplied. SPEC-0012: "The dashboard MUST list only the authenticated human's agents."
func TestDashboardListsOnlySessionHumansAgents(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	humanA, tokenA := mintSession(t, st, ctx, "test|alice", "Alice Ames", "alice@example.com")
	humanB, tokenB := mintSession(t, st, ctx, "test|bob", "Bob Byrne", "bob@example.com")
	if _, err := st.CreateAgent(ctx, humanA.ID, "alice-reviewer", ""); err != nil {
		t.Fatalf("create agent A: %v", err)
	}
	if _, err := st.CreateAgent(ctx, humanB.ID, "bob-deployer", ""); err != nil {
		t.Fatalf("create agent B: %v", err)
	}

	recA := getAs(t, r, tokenA, "/agents")
	if recA.Code != http.StatusOK {
		t.Fatalf("GET /agents as A: got %d, want 200", recA.Code)
	}
	bodyA := recA.Body.String()
	if !strings.Contains(bodyA, "alice-reviewer") {
		t.Error("A's dashboard must list A's agent")
	}
	if strings.Contains(bodyA, "bob-deployer") {
		t.Error("A's dashboard must NOT list B's agent")
	}

	recB := getAs(t, r, tokenB, "/agents")
	if recB.Code != http.StatusOK {
		t.Fatalf("GET /agents as B: got %d, want 200", recB.Code)
	}
	bodyB := recB.Body.String()
	if !strings.Contains(bodyB, "bob-deployer") {
		t.Error("B's dashboard must list B's agent")
	}
	if strings.Contains(bodyB, "alice-reviewer") {
		t.Error("B's dashboard must NOT list A's agent")
	}
}

// TestUnownedAgentIs404WithoutExistenceLeak: fetching another human's agent returns the same 404 —
// status and body — as fetching an agent that does not exist at all, and never echoes the unowned
// agent's name or description. SPEC-0012: "MUST return 404 for an agent the human does not own."
func TestUnownedAgentIs404WithoutExistenceLeak(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	_, tokenA := mintSession(t, st, ctx, "test|alice", "Alice Ames", "alice@example.com")
	humanB, tokenB := mintSession(t, st, ctx, "test|bob", "Bob Byrne", "bob@example.com")
	agentB, err := st.CreateAgent(ctx, humanB.ID, "bob-secret-agent", "bob's confidential notes")
	if err != nil {
		t.Fatalf("create agent B: %v", err)
	}

	unowned := getAs(t, r, tokenA, "/agents/"+agentB.ID)
	nonexistent := getAs(t, r, tokenA, "/agents/00000000-0000-0000-0000-000000000000")

	if unowned.Code != http.StatusNotFound {
		t.Fatalf("GET unowned agent as A: got %d, want 404", unowned.Code)
	}
	if nonexistent.Code != http.StatusNotFound {
		t.Fatalf("GET nonexistent agent as A: got %d, want 404", nonexistent.Code)
	}
	// Existence must not leak: the unowned response is byte-identical to the nonexistent one.
	if unowned.Body.String() != nonexistent.Body.String() {
		t.Errorf("unowned 404 body %q differs from nonexistent 404 body %q — existence leak",
			unowned.Body.String(), nonexistent.Body.String())
	}
	if b := unowned.Body.String(); strings.Contains(b, "bob-secret-agent") || strings.Contains(b, "confidential") {
		t.Errorf("unowned 404 leaked agent contents: %q", b)
	}
	// And the owner still sees it: 404 is authorization-scoped, not a missing row.
	if rec := getAs(t, r, tokenB, "/agents/"+agentB.ID); rec.Code != http.StatusOK {
		t.Fatalf("GET own agent as B: got %d, want 200", rec.Code)
	}
}

// TestLogoutRevokesSessionAndClearsCookie: POST /logout (with the session's CSRF token, as the
// rendered form submits it) clears the cookie AND revokes the server-side session, so replaying
// the old cookie is redirected to /login. SPEC-0008 scenario "Logout revokes the session".
func TestLogoutRevokesSessionAndClearsCookie(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	_, token := mintSession(t, st, ctx, "test|alice", "Alice Ames", "alice@example.com")

	// Authenticated before logout; scrape the CSRF token from the rendered dashboard form.
	rec := getAs(t, r, token, "/agents")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /agents pre-logout: got %d, want 200", rec.Code)
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
	if rec := getAs(t, r, token, "/agents"); rec.Code != http.StatusFound || rec.Header().Get("Location") != "/login" {
		t.Fatalf("replayed cookie after logout: got %d → %q, want 302 → /login", rec.Code, rec.Header().Get("Location"))
	}
}
