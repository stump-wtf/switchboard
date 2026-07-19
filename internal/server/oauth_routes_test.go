package server

// Wiring tests for the OAuth AS surface on the production route table (ADR-0019; SPEC-0016):
// discovery documents are served at their well-known paths, every URL they advertise stays on the
// deployed base, the registration endpoint the AS metadata advertises actually resolves on this
// router (the round-trip half of REQ "Authorization Server Metadata"), and the group carries the
// per-IP throttle. Handler-level conformance lives in internal/oauthsrv; these tests only prove
// the wiring.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/joestump/switchboard/internal/oauthsrv"
)

func getDoc(t *testing.T, r chi.Router, path string) map[string]any {
	t.Helper()
	rec := anonRequest(t, r, http.MethodGet, path)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: got %d, want 200 (body: %.300s)", path, rec.Code, rec.Body.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("GET %s: invalid JSON: %v", path, err)
	}
	return doc
}

// TestASMetadataServedAndConsistent: the RFC 8414 document is served on the production router and
// every advertised URL sits on the configured base URL; the advertised registration endpoint
// resolves to a live route (a malformed POST draws the RFC 7591 400, not a 404 — proving the
// route exists without touching the store). Governing: SPEC-0016 REQ "Authorization Server
// Metadata" scenario "Metadata round-trip".
func TestASMetadataServedAndConsistent(t *testing.T) {
	r := newTestRouter(t)
	doc := getDoc(t, r, oauthsrv.ASMetadataPath)

	const base = "https://sb.example.com" // newTestRouter's cfg.BaseURL
	if got, _ := doc["issuer"].(string); got != base {
		t.Fatalf("issuer = %v, want %q", doc["issuer"], base)
	}
	for _, k := range []string{"authorization_endpoint", "token_endpoint", "registration_endpoint"} {
		u, _ := doc[k].(string)
		if !strings.HasPrefix(u, base+"/") {
			t.Errorf("%s = %q, want a URL under the deployed base %q", k, u, base)
		}
	}

	// Round-trip the advertised registration endpoint against this very router.
	regURL, err := url.Parse(doc["registration_endpoint"].(string))
	if err != nil {
		t.Fatalf("registration_endpoint does not parse: %v", err)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, regURL.Path, strings.NewReader("not json")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST %s (malformed): got %d, want 400 — the advertised registration endpoint must resolve here", regURL.Path, rec.Code)
	}
	var errDoc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &errDoc); err != nil || errDoc["error"] != "invalid_client_metadata" {
		t.Fatalf("registration error document = %.200s (parse err %v)", rec.Body.String(), err)
	}
}

// TestTokenEndpointServed: the advertised token endpoint resolves on the production router — a
// grantless POST draws the RFC 6749 §5.2 error document (unsupported_grant_type) with
// Cache-Control: no-store, not a 404 — without touching the store. Handler-level grant conformance
// lives in internal/oauthsrv/token_test.go. Governing: SPEC-0016 REQ "Token Issuance And Refresh",
// REQ "Authorization Server Metadata" (every advertised URL resolves here).
func TestTokenEndpointServed(t *testing.T) {
	r := newTestRouter(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, oauthsrv.TokenPath, strings.NewReader("grant_type=password"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST %s: got %d, want 400 — the advertised token endpoint must resolve here", oauthsrv.TokenPath, rec.Code)
	}
	var errDoc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &errDoc); err != nil || errDoc["error"] != "unsupported_grant_type" {
		t.Fatalf("token error document = %.200s (parse err %v)", rec.Body.String(), err)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
}

// TestProtectedResourceMetadataServed: the RFC 9728 document for an MCP mount is served at the
// path-insertion well-known URL and names the mount + AS on the deployed base. Governing:
// SPEC-0016 REQ "Protected Resource Metadata".
func TestProtectedResourceMetadataServed(t *testing.T) {
	r := newTestRouter(t)
	doc := getDoc(t, r, oauthsrv.ProtectedResourcePrefix+"/mcp/agent-a-11111111")
	if got, _ := doc["resource"].(string); got != "https://sb.example.com/mcp/agent-a-11111111" {
		t.Fatalf("resource = %v", doc["resource"])
	}
	as, ok := doc["authorization_servers"].([]any)
	if !ok || len(as) != 1 || as[0] != "https://sb.example.com" {
		t.Fatalf("authorization_servers = %v, want the deployed base", doc["authorization_servers"])
	}
}

// TestOAuthSurfaceRateLimited: the /oauth + well-known group shares one per-IP token bucket, so a
// hammering client draws 429 + Retry-After (SPEC-0016 "rate limiting on /oauth/* consistent with
// the existing limiter").
func TestOAuthSurfaceRateLimited(t *testing.T) {
	r := newTestRouter(t)
	limited := false
	for i := 0; i < 40; i++ {
		rec := anonRequest(t, r, http.MethodGet, oauthsrv.ASMetadataPath)
		if rec.Code == http.StatusTooManyRequests {
			if rec.Header().Get("Retry-After") == "" {
				t.Fatal("429 without Retry-After")
			}
			limited = true
			break
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: got %d, want 200 or 429", i, rec.Code)
		}
	}
	if !limited {
		t.Fatal("40 rapid requests never drew a 429; the oauth group is unthrottled")
	}
}
