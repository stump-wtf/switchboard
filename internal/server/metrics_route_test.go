package server

// /metrics route tests
//
// The scrape endpoint against the REAL route table. The DB-free test pins the auth matrix, the
// closed-by-default posture, GET-only mounting and the per-IP throttle. The DB-backed test is the
// isolation half of SPEC-0023 REQ-1: it proves each foreign credential is genuinely live on its own
// surface (a vended endpoint token on /mcp, an operator OAuth token on /api/v1, a session cookie on
// the web UI) and then shows /metrics refusing it exactly like no credential at all.
//
// Governing: SPEC-0023 REQ-1 "The endpoint"; design.md "Auth" and "Testing"; ADR-0028.
//
// @joestump-agent 09/21/2026 - Added for issue #273 (SPEC-0023 story 1).

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/stump-wtf/switchboard/internal/a2a"
	"github.com/stump-wtf/switchboard/internal/auth"
	"github.com/stump-wtf/switchboard/internal/config"
	"github.com/stump-wtf/switchboard/internal/ingest"
	mcpsrv "github.com/stump-wtf/switchboard/internal/mcp"
	"github.com/stump-wtf/switchboard/internal/metrics"
	"github.com/stump-wtf/switchboard/internal/oauthsrv"
	"github.com/stump-wtf/switchboard/internal/store"
	"github.com/stump-wtf/switchboard/internal/web"
)

// testScrapeToken is a fixed, non-secret test credential (>= 32 bytes).
const testScrapeToken = "test-scrape-token-0123456789abcdef"

// newMetricsRouter builds the production route table over st with the metric surface wired and the
// scrape token set to token ("" leaves the endpoint closed). mcph may be nil, in which case one is
// built over st.
func newMetricsRouter(t *testing.T, st *store.Store, mcph *mcpsrv.Handler, token string) chi.Router {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Config{BaseURL: "https://sb.example.com", MetricsToken: token}
	authr, err := auth.New(context.Background(), cfg, st, log)
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	webh, err := web.New(st, cfg, log)
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	if mcph == nil {
		mcph = mcpsrv.New(st, log)
		t.Cleanup(mcph.Close)
	}
	mtr := metrics.New(metrics.Options{Log: log})
	ing := ingest.New(st, ingest.NewHub(), log, ingest.Config{})
	st.SetMetrics(mtr)
	ing.SetMetrics(mtr)
	return newRouter(routerDeps{
		cfg:     cfg,
		st:      st,
		authr:   authr,
		webh:    webh,
		ing:     ing,
		mcp:     mcph,
		a2a:     a2a.New(st, log),
		oauth:   oauthsrv.New(st, cfg.BaseURL, log),
		metrics: mtr,
		ping:    func(context.Context) error { return nil },
		log:     log,
	})
}

// getMetrics issues GET /metrics with an optional Authorization header and session cookie, from a
// distinct client address per call site so the per-IP throttle never interferes with the matrix.
func getMetrics(t *testing.T, r http.Handler, remote, authz, session string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.RemoteAddr = remote + ":40000"
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	if session != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: session})
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func assertMetrics401(t *testing.T, name string, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("%s: GET /metrics got %d, want 401", name, rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("%s: 401 carried %d body bytes, want an empty body", name, rec.Body.Len())
	}
}

func assertMetrics200(t *testing.T, name string, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: GET /metrics got %d, want 200", name, rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Errorf("%s: Content-Type %q, want prefix text/plain; version=0.0.4", name, ct)
	}
	body := rec.Body.String()
	for _, want := range []string{"go_goroutines", "process_"} {
		if !strings.Contains(body, want) {
			t.Errorf("%s: scrape body missing %q", name, want)
		}
	}
}

// TestMetricsRouteAuthMatrix pins the REQ-1 acceptance criteria that need no database: no
// credential, a wrong or foreign-shaped bearer, and a session cookie alone are all 401 with an
// empty body; the scrape token is 200 with the text exposition.
func TestMetricsRouteAuthMatrix(t *testing.T) {
	r := newMetricsRouter(t, store.New(nil), nil, testScrapeToken)

	assertMetrics401(t, "no credential", getMetrics(t, r, "192.0.2.1", "", ""))
	assertMetrics401(t, "wrong bearer", getMetrics(t, r, "192.0.2.2", "Bearer wrong-wrong-wrong-wrong-wrong-wrong", ""))
	assertMetrics401(t, "endpoint-shaped bearer", getMetrics(t, r, "192.0.2.3", "Bearer sbk_notarealcredential0000000000", ""))
	assertMetrics401(t, "basic scheme", getMetrics(t, r, "192.0.2.4", "Basic "+testScrapeToken, ""))
	assertMetrics401(t, "session cookie only", getMetrics(t, r, "192.0.2.5", "", "some-session-token"))
	assertMetrics200(t, "scrape token", getMetrics(t, r, "192.0.2.6", "Bearer "+testScrapeToken, ""))
	// The scrape token still opens it alongside an unrelated cookie: the cookie is simply not read.
	assertMetrics200(t, "scrape token + cookie", getMetrics(t, r, "192.0.2.7", "Bearer "+testScrapeToken, "some-session-token"))
}

// TestMetricsRouteClosedWhenTokenUnset: with SWITCHBOARD_METRICS_TOKEN unset — or no metric surface
// wired at all — every request is 401, including one presenting what would be the token.
func TestMetricsRouteClosedWhenTokenUnset(t *testing.T) {
	for name, r := range map[string]chi.Router{
		"token unset":     newMetricsRouter(t, store.New(nil), nil, ""),
		"metrics not set": newTestRouter(t),
	} {
		for _, authz := range []string{"", "Bearer ", "Bearer " + testScrapeToken} {
			assertMetrics401(t, name+" / "+authz, getMetrics(t, r, "192.0.2.10", authz, ""))
		}
	}
}

// TestMetricsRouteIsGetOnly: the scrape surface reads nothing from a request body, so no other
// method is routed to it.
func TestMetricsRouteIsGetOnly(t *testing.T) {
	r := newMetricsRouter(t, store.New(nil), nil, testScrapeToken)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "/metrics", strings.NewReader("x"))
		req.Header.Set("Authorization", "Bearer "+testScrapeToken)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /metrics: got %d, want 405", method, rec.Code)
		}
	}
}

// TestMetricsRouteRateLimited: the route sits behind a per-IP throttle, so a single client probing
// for the token runs dry while another address is unaffected.
func TestMetricsRouteRateLimited(t *testing.T) {
	r := newMetricsRouter(t, store.New(nil), nil, testScrapeToken)
	throttled := false
	for range 40 {
		if rec := getMetrics(t, r, "198.51.100.7", "Bearer guess", ""); rec.Code == http.StatusTooManyRequests {
			throttled = true
			if rec.Header().Get("Retry-After") == "" {
				t.Error("429 without Retry-After")
			}
			break
		}
	}
	if !throttled {
		t.Fatal("40 rapid requests from one address were never throttled; /metrics must sit behind a per-IP limiter")
	}
	assertMetrics200(t, "other address", getMetrics(t, r, "198.51.100.8", "Bearer "+testScrapeToken, ""))
}

// TestMetricsRefusesLiveForeignCredentials is the REQ-1 isolation criterion with real credentials:
// each one is first shown to authenticate on its OWN surface, then refused by /metrics with the same
// empty 401 as no credential at all.
func TestMetricsRefusesLiveForeignCredentials(t *testing.T) {
	_, st, ctx := newDBRouter(t) // provisions + truncates the package test database; skips without one
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mcph := mcpsrv.New(st, log)
	t.Cleanup(mcph.Close)
	r := newMetricsRouter(t, st, mcph, testScrapeToken)

	human, session := mintSession(t, st, ctx, "test|metrics-op", "Metrics Operator", "metrics-op@example.com")
	endpointTok, ep := vendWithExpiry(t, st, ctx, human.ID, "metrics-probe-bot", nil)
	operatorTok := mintOperatorBearer(t, st, ctx, human.ID, "metrics-test-cli")

	// Each credential is live where it belongs, through the same router that serves /metrics. The
	// empty MCP body fails protocol-wise, never auth-wise: anything but 401 means the bearer
	// authenticated.
	mcpReq := httptest.NewRequest(http.MethodPost, "/mcp/"+ep.Slug+"/", strings.NewReader(""))
	mcpReq.Header.Set("Authorization", "Bearer "+endpointTok)
	mcpRec := httptest.NewRecorder()
	r.ServeHTTP(mcpRec, mcpReq)
	if mcpRec.Code == http.StatusUnauthorized {
		t.Fatalf("vended endpoint credential rejected on /mcp (%d); fixture is not a live credential", mcpRec.Code)
	}
	apiReq := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	apiReq.Header.Set("Authorization", "Bearer "+operatorTok)
	apiRec := httptest.NewRecorder()
	r.ServeHTTP(apiRec, apiReq)
	if apiRec.Code != http.StatusOK {
		t.Fatalf("operator OAuth token on /api/v1/agents: got %d, want 200; fixture is not a live credential", apiRec.Code)
	}
	if rec := getAs(t, r, session, "/todos"); rec.Code != http.StatusOK {
		t.Fatalf("session cookie on /todos: got %d, want 200; fixture is not a live session", rec.Code)
	}

	// And none of them opens /metrics.
	assertMetrics401(t, "vended endpoint bearer", getMetrics(t, r, "203.0.113.1", "Bearer "+endpointTok, ""))
	assertMetrics401(t, "operator OAuth bearer", getMetrics(t, r, "203.0.113.2", "Bearer "+operatorTok, ""))
	assertMetrics401(t, "human session", getMetrics(t, r, "203.0.113.3", "", session))
	assertMetrics200(t, "scrape token", getMetrics(t, r, "203.0.113.4", "Bearer "+testScrapeToken, ""))
}
