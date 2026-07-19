// Route-table security baseline tests for the switchboard HTTP surface. These build the REAL
// router (the same newRouter Run uses) and walk its route table, so every registered route must be
// explicitly classified — session-gated, bearer-gated, or deliberately public — and a new route
// that is not classified fails the suite. Auth-by-default, enforced by test.
// Governing: SPEC-0012 REQ "Screen Set and Routes" (public GET /login; everything else behind
// RequireHuman); SPEC-0013 REQ "Information Architecture and Navigation" (GET / Board is gated).
package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/joestump/switchboard/internal/auth"
	"github.com/joestump/switchboard/internal/config"
	"github.com/joestump/switchboard/internal/ingest"
	mcpsrv "github.com/joestump/switchboard/internal/mcp"
	"github.com/joestump/switchboard/internal/oauthsrv"
	"github.com/joestump/switchboard/internal/store"
	"github.com/joestump/switchboard/internal/web"
)

// newTestRouter builds the production route table without a database. That is safe for anonymous
// requests: RequireHuman rejects on the missing session cookie and the bearer surfaces reject on
// the missing Authorization header, both before any store call.
func newTestRouter(t *testing.T) chi.Router {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Config{BaseURL: "https://sb.example.com"}
	st := store.New(nil)
	authr, err := auth.New(context.Background(), cfg, st, log)
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	webh, err := web.New(st, cfg, log)
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	hub := ingest.NewHub()
	mcph := mcpsrv.New(st, log)
	t.Cleanup(mcph.Close)
	return newRouter(routerDeps{
		st:    st,
		authr: authr,
		webh:  webh,
		ing:   ingest.New(st, hub, log, ingest.Config{}),
		mcp:   mcph,
		oauth: oauthsrv.New(st, cfg.BaseURL, log),
		ping:  func(context.Context) error { return nil },
		log:   log,
	})
}

// sessionRoutes is the exact set of session-gated (RequireHuman) web routes — the SPEC-0012 screen
// set plus the SPEC-0013 Board landing and the SSE stream. Anonymous requests MUST be redirected
// to /login, and the set itself is asserted, so dropping a route from the RequireHuman group (or
// adding one without updating this contract) fails.
var sessionRoutes = map[string]bool{
	"GET /":                       true, // Board landing (SPEC-0013) — gated, per PR #120 wiring
	"GET /todos":                  true, // Todos view (SPEC-0013 durable-queue table)
	"GET /todos/{id}":             true, // Todo detail drawer (fragment / standalone)
	"GET /endpoints":              true, // Endpoints view (SPEC-0015 vended-endpoint cards)
	"GET /endpoints/vend":         true, // vend wizard start (mints server-side step state)
	"GET /endpoints/vend/{step}":  true, // vend wizard step pages (SPEC-0015 wizard pattern)
	"POST /endpoints/vend/{step}": true, // step submit / confirm-step mint
	// Providers view + lifecycle (SPEC-0017): the view over the runtime registry, the shared
	// confirmation modal, and the disable/enable/rotate/remove POSTs — all session-gated.
	"GET /providers":                         true, // Providers view (SPEC-0015 six-view IA; SPEC-0017 registry-backed)
	"GET /providers/{name}/confirm/{action}": true, // lifecycle confirmation modal (SPEC-0017)
	"POST /providers/{name}/disable":         true, // stop the line, keep history (SPEC-0017)
	"POST /providers/{name}/enable":          true, // restore a disabled line (SPEC-0017)
	"POST /providers/{name}/rotate":          true, // rotate secret + one-time reveal (SPEC-0017)
	"POST /providers/{name}/remove":          true, // remove line, never its events/todos (SPEC-0017)
	"POST /endpoints/vend":                   true, // direct single-form mint + one-time reveal
	"GET /agents":                            true, // retired SPEC-0012 screen — 303-redirects to /endpoints
	"GET /agents/{id}":                       true, // retired SPEC-0012 screen — 303-redirects to /endpoints
	"GET /events":                            true,
	"POST /todos/{id}/claim":                 true, // operator claim (Board feed + Todos view)
	"POST /todos/{id}/complete":              true, // operator complete · ack (SPEC-0013)
	"POST /todos/{id}/fail":                  true, // operator fail (SPEC-0013)
	"POST /todos/{id}/retry":                 true, // operator retry a dead-lettered todo (SPEC-0013)
	"POST /todos/{id}/extend":                true, // operator extend lease / heartbeat (SPEC-0013)
	"POST /todos/{id}/release":               true, // operator release lease back to pending (SPEC-0013)
	// OAuth consent (SPEC-0016 "Authorization Code Flow With Consent"): the authorize endpoint IS
	// the flow's human gate, so both the screen and the decision POST are session-gated — an
	// anonymous authorize request must land on /login (scenario "Human absent").
	"GET /oauth/authorize":        true,
	"POST /oauth/authorize":       true,
	"GET /endpoints/{id}/revoke":  true, // revoke confirm page (irreversible steps confirm, SPEC-0015)
	"POST /endpoints/{id}/revoke": true,
	"POST /endpoints/{id}/delete": true, // permanently delete a revoked endpoint (SPEC-0007)
	// Friends view + approval flow (SPEC-0013). All session-gated; the handlers 404 when the friending
	// capability is disabled, but auth (RequireHuman) still runs first, so anonymous → /login here too.
	"GET /friends":                  true,
	"GET /friends/new":              true,
	"GET /friends/resolve":          true,
	"POST /friends":                 true,
	"POST /friends/{id}/approve":    true,
	"POST /friends/{id}/decline":    true,
	"POST /friends/{id}/withdraw":   true,
	"POST /friends/{id}/revoke":     true,
	"POST /friends/{id}/unblock":    true,
	"GET /personas":                 true, // Personas view (SPEC-0015; capability-gated in the handler)
	"GET /personas/wizard":          true, // persona create wizard start (mints server-side step state)
	"GET /personas/wizard/{step}":   true, // persona wizard step pages (identity → scope → publish)
	"POST /personas/wizard/{step}":  true, // step submit / publish-step save
	"POST /personas/wizard/preview": true, // live A2A card preview from the unsaved draft (SPEC-0015)
	"GET /personas/{id}/edit":       true, // persona edit wizard start (seeded from the persona)
	"POST /personas":                true, // direct single-form create persona
	"POST /personas/{id}":           true, // update persona (incl. publish toggle)
	"POST /personas/{id}/delete":    true, // delete persona
	"POST /logout":                  true,
}

// publicRoutes are the routes deliberately reachable without a session or bearer credential, each
// with its justification (SPEC-0012 security checklist: "public routes explicitly justified").
var publicRoutes = map[string]bool{
	"GET /login":           true, // the login screen itself (SPEC-0012: public GET /login)
	"GET /auth/login":      true, // OIDC initiation — must be reachable to authenticate
	"GET /auth/callback":   true, // OIDC redirect target — verified by state/nonce, not session
	"POST /auth/dev-login": true, // 404s unless SWITCHBOARD_DEV_LOGIN=1 (tested below)
	"GET /healthz":         true, // liveness probe
	"POST /dev/todos":      true, // 404s unless SWITCHBOARD_DEV_LOGIN=1 (dev loop helper)
	// Public A2A Agent Card (SPEC-0009): discovery requires peers to read the card before any
	// friendship exists; it grants nothing, exposes only owner-approved metadata, and 404s for any
	// persona the owner has not marked discoverable.
	"GET /a/{persona_id}/.well-known/agent-card.json": true,
	// Webhook receivers authenticate per-provider (HMAC/token; SPEC-0001), not via session.
	"POST /webhooks/github":         true,
	"POST /webhooks/stripe":         true,
	"POST /webhooks/slack":          true,
	"POST /webhooks/generic/{name}": true,
	// Self-managed webhook receiver: the unguessable path token both routes and authenticates
	// (SPEC-0006); an unknown token 404s, so it is deliberately session-free.
	"POST /webhooks/w/{token}": true,
	// OAuth AS surface (ADR-0019; SPEC-0016): discovery documents are how an unauthenticated MCP
	// client learns to authorize at all, and RFC 7591 dynamic registration is anonymous by design
	// (public clients + PKCE) — the flow's human gate is the session-guarded consent screen, not
	// registration. Every field of a registration is validated (exact redirect-URI rules) and the
	// body is bounded; the metadata GETs read nothing from the store.
	"GET /.well-known/oauth-authorization-server":              true,
	"GET /.well-known/oauth-protected-resource/mcp/{endpoint}": true,
	"POST /oauth/register":                                     true,
	// The token endpoint is public like the rest of the AS surface: clients are public (no client
	// secret), so the proof is PKCE possession on the code grant and the rotating refresh token on
	// the refresh grant (SPEC-0016 REQ "Token Issuance And Refresh") — never a session.
	"POST /oauth/token": true,
	// A2A friend-request intake authenticates by the requesting human's OIDC-signed provenance
	// carried IN-BAND (ADR-0010/0011; SPEC-0010), not via a session cookie or bearer header —
	// missing/invalid provenance → 401 with no pending edge. Not "ungoverned public": it is
	// authenticated, just by signed provenance instead of a session.
	"POST /a2a/friend-requests": true,
}

// routePath turns a chi route pattern into a concrete request path.
func routePath(route string) string {
	return strings.NewReplacer(
		"{id}", "00000000-0000-0000-0000-000000000000",
		"{persona_id}", "00000000-0000-0000-0000-000000000000",
		"{name}", "x", "{endpoint}", "x", "{step}", "persona", "*", "x",
	).Replace(route)
}

func anonRequest(t *testing.T, r chi.Router, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

// TestEveryRouteClassifiedAndAnonymousRejected walks the full route table and enforces the
// authentication boundary route by route:
//   - session routes (RequireHuman group) redirect anonymous requests to /login;
//   - the vended MCP surface (/mcp) rejects anonymous requests with 401;
//   - static assets stay public;
//   - anything else must appear in publicRoutes, or the test fails (auth-by-default).
func TestEveryRouteClassifiedAndAnonymousRejected(t *testing.T) {
	r := newTestRouter(t)
	walked := map[string]bool{}
	err := chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		key := method + " " + route
		walked[key] = true
		switch {
		case sessionRoutes[key]:
			rec := anonRequest(t, r, method, routePath(route))
			if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/login" {
				t.Errorf("%s: anonymous got %d → %q, want 302 → /login", key, rec.Code, rec.Header().Get("Location"))
			}
		case strings.HasPrefix(route, "/mcp/"):
			// Vended MCP surface (ADR-0017; SPEC-0014): bearer-credential auth, no Authorization header → 401.
			rec := anonRequest(t, r, method, routePath(route))
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s: anonymous got %d, want 401", key, rec.Code)
			}
		case strings.HasPrefix(route, "/static/"):
			// Embedded assets are public by design (SPEC-0012 "Static assets served from embed").
		case publicRoutes[key]:
			// Deliberately public; justified in the map above.
		default:
			t.Errorf("%s: unclassified route — new routes must be session-gated, bearer-gated, or "+
				"explicitly justified as public in this test (auth-by-default)", key)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	// The screen set itself: every SPEC-0012/0013 route must actually be registered (and gated —
	// asserted above). Catches the Board landing being dropped or moved out of RequireHuman.
	for key := range sessionRoutes {
		if !walked[key] {
			t.Errorf("%s: required session-gated route missing from the router table", key)
		}
	}
	for key := range publicRoutes {
		if !walked[key] {
			t.Errorf("%s: expected public route missing from the router table", key)
		}
	}
}

// TestLoginPagePublic: the login screen renders for anonymous users — the one public web page
// (SPEC-0012 scenario "Login is public").
func TestLoginPagePublic(t *testing.T) {
	r := newTestRouter(t)
	rec := anonRequest(t, r, http.MethodGet, "/login")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /login anonymous: got %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "Log in") {
		t.Fatalf("login page should render the login screen, got: %.200s", body)
	}
}

// TestDevLoginDisabledIs404: without SWITCHBOARD_DEV_LOGIN the dev-login route exists but denies
// (404), so the public classification above never opens an unauthenticated login path in prod.
func TestDevLoginDisabledIs404(t *testing.T) {
	r := newTestRouter(t)
	if rec := anonRequest(t, r, http.MethodPost, "/auth/dev-login"); rec.Code != http.StatusNotFound {
		t.Fatalf("POST /auth/dev-login with dev mode off: got %d, want 404", rec.Code)
	}
	if rec := anonRequest(t, r, http.MethodPost, "/dev/todos"); rec.Code != http.StatusNotFound {
		t.Fatalf("POST /dev/todos with dev mode off: got %d, want 404", rec.Code)
	}
}
