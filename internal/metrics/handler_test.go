package metrics

// Scrape handler tests
//
// The handler's auth matrix in isolation: the dedicated bearer is the only credential that opens
// it, every failure is the same empty 401, an unset token closes it, and the exposition is pinned
// to Prometheus text 0.0.4 whatever the client asks for. The route-level matrix (vended endpoint,
// OAuth and session credentials against the real router) lives in internal/server.
//
// @joestump-agent 09/21/2026 - Added for issue #273 (SPEC-0023 story 1).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stump-wtf/switchboard/internal/ingest"
	"github.com/stump-wtf/switchboard/internal/store"
)

// The concrete surface satisfies both consumer seams; server.go relies on this.
var (
	_ store.Metrics        = (*Metrics)(nil)
	_ store.AttemptMetrics = (*Metrics)(nil)
	_ ingest.Metrics       = (*Metrics)(nil)
)

// testScrapeToken is a fixed, non-secret test credential (>= 32 bytes).
const testScrapeToken = "test-scrape-token-0123456789abcdef"

func scrape(h http.Handler, header map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func assertEmpty401(t *testing.T, name string, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("%s: status %d, want 401", name, rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("%s: 401 carried a %d-byte body, want none (no metric names may leak): %.80q", name, rec.Body.Len(), rec.Body.String())
	}
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Errorf("%s: 401 without a WWW-Authenticate challenge", name)
	}
}

func TestHandlerRejectsEverythingButTheScrapeToken(t *testing.T) {
	m := New(Options{})
	m.TodoCreated("secret-project-queue", "github") // a queue name a prober must not learn
	h := m.Handler(testScrapeToken)

	for name, auth := range map[string]string{
		"no credential":             "",
		"wrong bearer":              "Bearer not-the-token-not-the-token-000",
		"token prefix":              "Bearer " + testScrapeToken[:len(testScrapeToken)-1],
		"token with extra suffix":   "Bearer " + testScrapeToken + "x",
		"vended endpoint shape":     "Bearer sbk_0123456789abcdef0123456789abcdef",
		"basic scheme":              "Basic " + testScrapeToken,
		"token without scheme":      testScrapeToken,
		"bearer with no credential": "Bearer ",
		"bearer only":               "Bearer",
	} {
		hdr := map[string]string{}
		if auth != "" {
			hdr["Authorization"] = auth
		}
		assertEmpty401(t, name, scrape(h, hdr))
	}
}

func TestHandlerServesTextFormatToTheScrapeToken(t *testing.T) {
	m := New(Options{})
	m.TodoCreated("forge", "github")
	h := m.Handler(testScrapeToken)

	for name, accept := range map[string]string{
		"no accept":   "",
		"openmetrics": "application/openmetrics-text;version=1.0.0,text/plain;version=0.0.4;q=0.5",
		"protobuf":    "application/vnd.google.protobuf;proto=io.prometheus.client.MetricFamily;encoding=delimited",
	} {
		hdr := map[string]string{"Authorization": "Bearer " + testScrapeToken}
		if accept != "" {
			hdr["Accept"] = accept
		}
		rec := scrape(h, hdr)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d, want 200", name, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
			t.Errorf("%s: Content-Type %q, want text/plain; version=0.0.4 (exposition is pinned)", name, ct)
		}
		body := rec.Body.String()
		for _, want := range []string{"go_goroutines", "process_", `switchboard_todos_created_total{queue="forge",source="github"} 1`} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: body missing %q", name, want)
			}
		}
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s: Cache-Control %q, want no-store", name, rec.Header().Get("Cache-Control"))
		}
	}
}

// TestHandlerSchemeIsCaseInsensitive: RFC 7235 auth schemes are case-insensitive.
func TestHandlerSchemeIsCaseInsensitive(t *testing.T) {
	h := New(Options{}).Handler(testScrapeToken)
	if rec := scrape(h, map[string]string{"Authorization": "bearer " + testScrapeToken}); rec.Code != http.StatusOK {
		t.Fatalf("lowercase scheme: status %d, want 200", rec.Code)
	}
}

// TestHandlerClosedWhenTokenUnset: no configured token means the endpoint is closed, never open —
// including to a request that presents an empty bearer that would "match" an empty token.
func TestHandlerClosedWhenTokenUnset(t *testing.T) {
	for name, h := range map[string]http.Handler{
		"unset token":  New(Options{}).Handler(""),
		"nil receiver": (*Metrics)(nil).Handler(testScrapeToken),
	} {
		for _, auth := range []string{"", "Bearer ", "Bearer " + testScrapeToken} {
			hdr := map[string]string{}
			if auth != "" {
				hdr["Authorization"] = auth
			}
			assertEmpty401(t, name+" / "+auth, scrape(h, hdr))
		}
	}
}
