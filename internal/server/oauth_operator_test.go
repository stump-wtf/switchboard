package server

// The operator grant end to end (ADR-0023): the CLI's authorize request (resource = <base>/api)
// renders a consent screen that says the client will act AS the human, approval issues a
// human-bound code, the real token endpoint exchanges it under PKCE, the access token opens
// /api/v1, refresh rotates the pair, and an endpoint-bound token is refused by the operator API.
// Through the real router + PostgreSQL; skipped without SWITCHBOARD_TEST_DATABASE_URL.
// Governing: ADR-0023, ADR-0019, SPEC-0016 REQ "Token Issuance And Refresh".

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/joestump/switchboard/internal/cred"
)

// apiCall performs a bearer-authenticated /api/v1 request the way the CLI does.
func apiCall(t *testing.T, r chi.Router, method, path, bearer, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// tokenCall posts to the public token endpoint and decodes the RFC 6749 document.
func tokenCall(t *testing.T, r chi.Router, form url.Values) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("token endpoint answered non-JSON (%d): %s", rec.Code, rec.Body.String())
	}
	return rec.Code, doc
}

func TestOperatorGrantConsentToAPI(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	human, session := mintSession(t, st, ctx, "test|operator", "Joe Stump", "joe@example.com")
	if _, err := st.CreateOAuthClient(ctx, "cid-cli", "switchboard CLI", []string{consentRedirect}); err != nil {
		t.Fatalf("create oauth client: %v", err)
	}
	verifier := "operator-grant-verifier-" + strings.Repeat("v", 40)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	// The CLI's authorize request: resource = <base>/api, no endpoint anywhere.
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {"cid-cli"},
		"redirect_uri":          {consentRedirect},
		"state":                 {"st-cli"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"resource":              {"https://sb.example.com/api"},
	}
	c := newWizClient(t, r, session)
	page := c.get("/oauth/authorize?" + q.Encode())
	if page.Code != http.StatusOK {
		t.Fatalf("consent GET: got %d (body %.300s)", page.Code, page.Body.String())
	}
	body := page.Body.String()
	for _, want := range []string{
		"switchboard CLI", "wants to act as you", // the ask names the operator shape
		"operator · Joe Stump · joe@example.com", // the grant binding line
		"register agents on this switchboard", "vend endpoints and see each credential exactly once", "list the endpoints you own",
		`name="resource" value="https://sb.example.com/api"`, // round-trips for the decision POST
		"this will allow the client to",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("operator consent page: missing %q", want)
		}
	}
	for _, banned := range []string{"/mcp/", "wants to connect to", "this will allow the agent to"} {
		if strings.Contains(body, banned) {
			t.Errorf("operator consent page shows endpoint-grant copy %q", banned)
		}
	}

	// Approve: the decision form carries the resource, and the code comes back to the client.
	form := url.Values{}
	for k, vs := range q {
		form.Set(k, vs[0])
	}
	form.Set("csrf_token", scrapeCSRF(t, body))
	form.Set("decision", "approve")
	rec := c.do(http.MethodPost, "/oauth/authorize", form)
	loc := locationQuery(t, rec.Code, rec.Header().Get("Location"))
	code := loc.Get("code")
	if code == "" || loc.Get("state") != "st-cli" || loc.Get("error") != "" {
		t.Fatalf("approve redirect query = %v", loc)
	}

	// Exchange at the real token endpoint under PKCE → a human-bound access/refresh pair.
	status, tok := tokenCall(t, r, url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {verifier},
		"client_id": {"cid-cli"}, "redirect_uri": {consentRedirect},
	})
	if status != http.StatusOK {
		t.Fatalf("exchange: %d %v", status, tok)
	}
	access, _ := tok["access_token"].(string)
	refresh, _ := tok["refresh_token"].(string)
	if access == "" || refresh == "" || tok["token_type"] != "Bearer" {
		t.Fatalf("token document = %v", tok)
	}
	if exp, _ := tok["expires_in"].(float64); exp <= 0 || exp > 3600 {
		t.Fatalf("expires_in = %v, want (0, 3600]", tok["expires_in"])
	}
	stored, err := st.HumanByOAuthToken(ctx, cred.Hash(access))
	if err != nil || stored.ID != human.ID {
		t.Fatalf("access token resolves to %+v, %v; want the consenting human", stored, err)
	}

	// The bearer opens the operator API: list (empty), vend, list again.
	if rec := apiCall(t, r, http.MethodGet, "/api/v1/agents", access, ""); rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("GET /api/v1/agents: %d %q", rec.Code, rec.Body.String())
	}
	vend := apiCall(t, r, http.MethodPost, "/api/v1/endpoints", access, `{"name":"cli-bot","queue":"inbox"}`)
	if vend.Code != http.StatusCreated {
		t.Fatalf("POST /api/v1/endpoints: %d %s", vend.Code, vend.Body.String())
	}
	var vended struct {
		Slug    string          `json:"slug"`
		Token   string          `json:"token"`
		MCPJSON json.RawMessage `json:"mcp_json"`
	}
	if err := json.Unmarshal(vend.Body.Bytes(), &vended); err != nil || vended.Slug == "" || vended.Token == "" || len(vended.MCPJSON) == 0 {
		t.Fatalf("vend document = %s (%v)", vend.Body.String(), err)
	}
	if rec := apiCall(t, r, http.MethodGet, "/api/v1/endpoints", access, ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), vended.Slug) || strings.Contains(rec.Body.String(), vended.Token) {
		t.Fatalf("GET /api/v1/endpoints: %d %s (must list the slug, never the token)", rec.Code, rec.Body.String())
	}
	if rec := apiCall(t, r, http.MethodGet, "/api/v1/agents", access, ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"name":"cli-bot"`) {
		t.Fatalf("GET /api/v1/agents after vend: %d %s", rec.Code, rec.Body.String())
	}
	// The vended endpoint's own bearer is an agent credential, not an operator one.
	if rec := apiCall(t, r, http.MethodGet, "/api/v1/agents", vended.Token, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("vended sbk_ credential on the operator API: %d, want 401", rec.Code)
	}

	// An endpoint-bound OAuth token (the MCP shape) never resolves on the operator API.
	ep := consentFixture(t, st, ctx, human.ID, "mcp-bot", "mcp-bot-ab12cd34", "cid-mcp")
	if _, err := st.CreateOAuthToken(ctx, cred.Hash("ep-access"), cred.Hash("ep-refresh"), "cid-mcp", ep.ID, "", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create endpoint token: %v", err)
	}
	if rec := apiCall(t, r, http.MethodGet, "/api/v1/agents", "ep-access", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("endpoint-bound OAuth token on the operator API: %d, want 401", rec.Code)
	}

	// Refresh rotates the pair within the same grant: the new access works, the old one is dead.
	status, rotated := tokenCall(t, r, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {"cid-cli"}})
	if status != http.StatusOK {
		t.Fatalf("refresh: %d %v", status, rotated)
	}
	newAccess, _ := rotated["access_token"].(string)
	if rec := apiCall(t, r, http.MethodGet, "/api/v1/agents", newAccess, ""); rec.Code != http.StatusOK {
		t.Fatalf("rotated access token: %d", rec.Code)
	}
	if rec := apiCall(t, r, http.MethodGet, "/api/v1/agents", access, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("pre-rotation access token still accepted: %d", rec.Code)
	}
	// A replayed refresh token is refused (rotation is single-use).
	if status, doc := tokenCall(t, r, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {"cid-cli"}}); status != http.StatusBadRequest || doc["error"] != "invalid_grant" {
		t.Fatalf("replayed refresh: %d %v, want 400 invalid_grant", status, doc)
	}
}

// TestOperatorResourceMustMatchTheDeployment: a resource that merely looks like the operator
// resource on another origin is not an operator grant — it falls through to endpoint binding and,
// naming no endpoint, is refused at the client's redirect with invalid_target.
func TestOperatorResourceMustMatchTheDeployment(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	_, session := mintSession(t, st, ctx, "test|operator-foreign", "Joe Stump", "joe@example.com")
	if _, err := st.CreateOAuthClient(ctx, "cid-foreign", "switchboard CLI", []string{consentRedirect}); err != nil {
		t.Fatalf("create oauth client: %v", err)
	}
	q := authorizeQuery("cid-foreign", "")
	q.Set("resource", "https://evil.example.com/api")
	c := newWizClient(t, r, session)
	rec := c.get("/oauth/authorize?" + q.Encode())
	loc := locationQuery(t, rec.Code, rec.Header().Get("Location"))
	if loc.Get("error") != "invalid_target" {
		t.Fatalf("foreign operator resource: redirect query = %v, want invalid_target", loc)
	}
}
