package server

// End-to-end static-asset serving through the REAL production route table. static_test.go proves
// the embedded-FS mount in isolation (secureHeaders + staticHandler); this fetches the exact
// assets layout.html references (SPEC-0013 layout shell) through newRouter — the same table Run
// builds — so a regression that dropped or misordered the /static mount inside the full router
// (behind the middleware stack, alongside the auth groups) would fail here even if the isolated
// handler still worked.
//
// Carry-over from the wave-2b verification pass (issue #102): asset-embed tests read the StaticFS
// directly / via a mini-router but never exercised GET /static/* through the assembled router.
// Governing: SPEC-0012 REQ "Server-Rendered Pages from Embedded Templates" (scenario "Static assets
// served from embed, not a CDN"), ADR-0001 (single binary, embedded assets, no CDN).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestStaticAssetsServedThroughProductionRouter fetches every asset the operator-board layout
// links, through the full newRouter table (no database needed — the static mount sits ahead of the
// auth groups), asserting a 200 with a non-empty body and a plausible content type for each.
func TestStaticAssetsServedThroughProductionRouter(t *testing.T) {
	r := newTestRouter(t)
	// The full set layout.html references: stylesheets, the presentation-JS helper, vendored htmx
	// (ADR-0001: no CDN), and a self-hosted font. wantCT is a substring the resolved Content-Type
	// must contain ("" skips the check, e.g. when the platform mime table is not guaranteed).
	cases := []struct {
		path   string
		wantCT string
	}{
		{"/static/tokens.css", "text/css"},
		{"/static/switchboard.css", "text/css"},
		{"/static/sb.js", "javascript"},
		{"/static/vendor/htmx.min.js", "javascript"},
		{"/static/vendor/htmx-ext-sse.min.js", "javascript"},
		{"/static/fonts/zilla-slab-600.woff2", ""},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s through router: status %d, want 200", c.path, rec.Code)
			continue
		}
		if rec.Body.Len() == 0 {
			t.Errorf("GET %s through router: empty body", c.path)
			continue
		}
		if c.wantCT != "" {
			if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, c.wantCT) {
				t.Errorf("GET %s: Content-Type %q, want to contain %q", c.path, ct, c.wantCT)
			}
		}
	}
}

// TestStaticThroughRouterCarriesSecureHeaders confirms the assembled router still fronts /static
// with the app-wide secure headers (secureHeaders is the outermost middleware, so static responses
// inherit the strict same-origin CSP and nosniff). Governing: SPEC-0012 REQ "Security headers on
// all responses".
func TestStaticThroughRouterCarriesSecureHeaders(t *testing.T) {
	r := newTestRouter(t)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/static/sb.js", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /static/sb.js: status %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("static response missing nosniff through router: %q", got)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
		t.Errorf("static response CSP missing default-src 'self' through router: %q", csp)
	}
}

// TestStaticUnknownAssetThroughRouterIs404 proves the router does not fall an unknown /static path
// through to another handler (it 404s from the embedded FileServer, not a redirect to /login).
func TestStaticUnknownAssetThroughRouterIs404(t *testing.T) {
	r := newTestRouter(t)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/static/does-not-exist.js", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /static/does-not-exist.js through router: status %d, want 404", rec.Code)
	}
}
