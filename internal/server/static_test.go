package server

// Static-serving conformance for SPEC-0012 REQ "Server-Rendered Pages from Embedded Templates",
// scenario "Static assets served from embed, not a CDN": /static/* resolves from the embedded FS
// (works offline, no runtime CDN) and the secureHeaders CSP that fronts it names no external
// origins, so every asset the layout references loads same-origin.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// newStaticRouter mounts staticHandler exactly as Run does, behind secureHeaders.
func newStaticRouter() http.Handler {
	r := chi.NewRouter()
	r.Use(secureHeaders)
	r.Handle("/static/*", staticHandler())
	return r
}

func TestStaticServedFromEmbed(t *testing.T) {
	r := newStaticRouter()
	// Every asset class the layout template references: stylesheets, vendored htmx (ADR-0001:
	// no CDN), and the split sb.js feature modules + theme boot (SPEC-0015 foundation). All must
	// come out of the embedded FS with content. (The charm-web woff2 files join this list once
	// vendored — fonts_test.go and static/fonts/README.md govern them until then.)
	cases := []struct {
		path       string
		wantPrefix string // magic bytes / leading content, "" to skip
	}{
		{"/static/tokens.css", ""},
		{"/static/switchboard.css", ""},
		{"/static/vendor/htmx.min.js", ""},
		{"/static/vendor/htmx-ext-sse.min.js", ""},
		{"/static/js/theme-boot.js", ""},
		{"/static/js/sb-live.js", ""},
		{"/static/js/sb-overlay.js", ""},
		{"/static/js/sb-vend.js", ""},
		{"/static/js/sb-theme.js", ""},
		{"/static/js/sb-keys.js", ""},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s: status %d", c.path, rec.Code)
			continue
		}
		body := rec.Body.Bytes()
		if len(body) == 0 {
			t.Errorf("GET %s: empty body", c.path)
			continue
		}
		if c.wantPrefix != "" && !bytes.HasPrefix(body, []byte(c.wantPrefix)) {
			t.Errorf("GET %s: body does not start with %q", c.path, c.wantPrefix)
		}
	}
}

func TestStaticUnknownAssetIs404(t *testing.T) {
	r := newStaticRouter()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/static/nope.css", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /static/nope.css: status %d, want 404", rec.Code)
	}
}

func TestStaticTraversalCannotEscapeEmbed(t *testing.T) {
	// http.FS + embed reject path traversal; nothing outside static/ is reachable.
	r := newStaticRouter()
	for _, p := range []string{"/static/../go.mod", "/static/..%2fgo.mod", "/static/%2e%2e/go.mod"} {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "module github.com/stump-wtf/switchboard") {
			t.Errorf("GET %s: escaped the embedded static FS", p)
		}
	}
}

func TestStaticResponsesCarryStrictSameOriginCSP(t *testing.T) {
	r := newStaticRouter()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/static/tokens.css", nil))
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'self'") {
		t.Errorf("CSP missing default-src 'self': %q", csp)
	}
	// A CSP that allowlists any external origin would defeat the offline/no-CDN guarantee.
	for _, banned := range []string{"http://", "https://", "cdn.", "unpkg", "googleapis"} {
		if strings.Contains(csp, banned) {
			t.Errorf("CSP names an external origin (%q): %q", banned, csp)
		}
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("static response missing X-Content-Type-Options: nosniff")
	}
}
