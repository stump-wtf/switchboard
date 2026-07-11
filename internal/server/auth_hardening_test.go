// Auth-surface hardening proofs for SPEC-0008: security headers on the public auth pages, a per-IP
// throttle on the auth entry points, and the login page's WCAG landmark / focus / live-region
// contract. These build the REAL router (the same newRouter Run uses) so the assertions bind to the
// wired middleware stack, not a hand-rolled one.
// Governing: SPEC-0008 "Security Requirements → Security Headers / Rate Limiting",
// "Accessibility Requirements".
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

	"github.com/joestump/switchboard/internal/agentapi"
	"github.com/joestump/switchboard/internal/auth"
	"github.com/joestump/switchboard/internal/config"
	"github.com/joestump/switchboard/internal/ingest"
	mcpsrv "github.com/joestump/switchboard/internal/mcp"
	"github.com/joestump/switchboard/internal/store"
	"github.com/joestump/switchboard/internal/web"
)

// buildRouter assembles the production route table for an arbitrary config (no database). Rendering
// the login page and running the auth middleware never touches the store, so a nil pool is safe.
func buildRouter(t *testing.T, cfg config.Config) chi.Router {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st := store.New(nil)
	authr, err := auth.New(context.Background(), cfg, st, log)
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	webh, err := web.New(st, cfg, log)
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	hub := agentapi.NewHub()
	mcph := mcpsrv.New(st, log)
	t.Cleanup(mcph.Close)
	return newRouter(routerDeps{
		st:    st,
		authr: authr,
		webh:  webh,
		api:   agentapi.New(st, hub, log),
		ing:   ingest.New(st, hub, log, ingest.Config{DevLogin: cfg.DevLogin}),
		mcp:   mcph,
		ping:  func(context.Context) error { return nil },
		log:   log,
	})
}

// TestSecurityHeadersOnAuthPages: every auth response — the rendered login page and even the
// dev-login 404 — carries the full defensive header set, because secureHeaders is the outermost app
// middleware. Governing: SPEC-0008 "Security Requirements → Security Headers".
func TestSecurityHeadersOnAuthPages(t *testing.T) {
	r := buildRouter(t, config.Config{BaseURL: "https://sb.example.com"})

	cases := []struct {
		method, path string
	}{
		{http.MethodGet, "/login"},           // 200 — the public login page
		{http.MethodGet, "/auth/login"},      // 503 — OIDC unconfigured, still passes secureHeaders
		{http.MethodPost, "/auth/dev-login"}, // 404 — dev mode off, still passes secureHeaders
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		h := rec.Header()
		if got := h.Get("X-Frame-Options"); got != "DENY" {
			t.Errorf("%s %s: X-Frame-Options = %q, want DENY", c.method, c.path, got)
		}
		if got := h.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s %s: X-Content-Type-Options = %q, want nosniff", c.method, c.path, got)
		}
		if got := h.Get("Referrer-Policy"); got != "strict-origin-when-cross-origin" {
			t.Errorf("%s %s: Referrer-Policy = %q", c.method, c.path, got)
		}
		if csp := h.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") ||
			!strings.Contains(csp, "frame-ancestors 'none'") {
			t.Errorf("%s %s: CSP missing expected directives: %q", c.method, c.path, csp)
		}
	}
}

// TestAuthEndpointsRateLimited: the auth entry points are throttled per IP (SPEC-0008 recommends a
// native limiter to blunt credential-stuffing / callback abuse). After the burst is spent, a further
// request from the same IP is answered 429 with a Retry-After.
func TestAuthEndpointsRateLimited(t *testing.T) {
	r := buildRouter(t, config.Config{BaseURL: "https://sb.example.com"})

	var got429 bool
	var retryAfter string
	// authRL is burst 10; 15 requests from one IP overruns it. httptest.NewRequest pins RemoteAddr.
	for i := 0; i < 15; i++ {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
		if rec.Code == http.StatusTooManyRequests {
			got429 = true
			retryAfter = rec.Header().Get("Retry-After")
			break
		}
	}
	if !got429 {
		t.Fatal("auth endpoint should throttle a single IP after its burst is spent")
	}
	if retryAfter == "" {
		t.Error("429 from the auth throttle should carry a Retry-After header")
	}
}

// TestLoginPageMisconfiguredAccessibility: with no login method configured the login page still
// exposes banner/main/contentinfo landmarks, and the misconfiguration message is an assertive
// live region (role="alert") that receives focus (autofocus), so AT announces it. Exactly one
// element carries autofocus. Governing: SPEC-0008 "Accessibility Requirements".
func TestLoginPageMisconfiguredAccessibility(t *testing.T) {
	r := buildRouter(t, config.Config{BaseURL: "https://sb.example.com"}) // no OIDC, no dev login
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/login", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /login: got %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	for _, want := range []string{
		`role="banner"`,      // banner landmark (top bar)
		"<main",              // main landmark holds the login form
		`role="contentinfo"`, // content-info landmark (footer)
		`role="alert"`,       // the misconfiguration error is an assertive live region
		"autofocus",          // focus lands on the error region
	} {
		if !strings.Contains(body, want) {
			t.Errorf("login page missing accessibility hook %q", want)
		}
	}
	if n := strings.Count(body, "autofocus"); n != 1 {
		t.Errorf("exactly one control may carry autofocus, found %d", n)
	}
}

// TestLoginPageDevLoginFocus: when dev login is the available method, focus lands on the dev-login
// button (autofocus) and its warning is a polite live region (role="status"); no misconfiguration
// alert is shown. Governing: SPEC-0008 "Accessibility Requirements" (focus on the primary control).
func TestLoginPageDevLoginFocus(t *testing.T) {
	r := buildRouter(t, config.Config{BaseURL: "https://sb.example.com", DevLogin: true})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/login", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /login: got %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	if !strings.Contains(body, "sb-btn--secondary") || !strings.Contains(body, "autofocus") {
		t.Error("dev-login button should be present and carry autofocus")
	}
	if !strings.Contains(body, `role="status"`) {
		t.Error("dev-login warning should be a polite live region (role=status)")
	}
	if strings.Contains(body, `role="alert"`) {
		t.Error("no misconfiguration alert should render when a login method is available")
	}
	if n := strings.Count(body, "autofocus"); n != 1 {
		t.Errorf("exactly one control may carry autofocus, found %d", n)
	}
}
