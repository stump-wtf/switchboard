// End-to-end vend-flow tests through the real router + PostgreSQL: the vend form rejects a missing
// scope with 400 and mints nothing, and a successful vend reveals the plaintext credential exactly
// once with HTTP-only .mcp.json wiring while persisting only its hash and prefix. Skipped without
// SWITCHBOARD_TEST_DATABASE_URL (Gitea CI runs DB-less; the GitHub mirror provides Postgres).
// Governing: SPEC-0012 REQ "Vend Flow and One-Time Credential Reveal"; SPEC-0014 REQ "HTTP Wiring
// Is the Only Wiring".
package server

import (
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// postForm submits an application/x-www-form-urlencoded body as the given session, the way the
// rendered vend form would.
func postForm(t *testing.T, r chi.Router, token, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// credRe pulls the revealed plaintext credential out of the vended page's <pre> block.
var credRe = regexp.MustCompile(`sbk_[A-Za-z0-9_-]+`)

// TestVendRejectsMissingScopeAndMintsNothing: an empty queues or verbs field is a 400 and leaves the
// agent with zero endpoints — no credential is minted on the rejection path.
// SPEC-0012 scenario "Missing scope is rejected".
func TestVendRejectsMissingScopeAndMintsNothing(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	human, token := mintSession(t, st, ctx, "test|alice", "Alice Ames", "alice@example.com")
	agent, err := st.CreateAgent(ctx, human.ID, "alice-reviewer", "")
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	csrf := scrapeCSRF(t, getAs(t, r, token, "/agents/"+agent.ID).Body.String())

	for name, form := range map[string]url.Values{
		"missing verbs":  {"csrf_token": {csrf}, "queues": {"reviews"}, "verbs": {""}},
		"missing queues": {"csrf_token": {csrf}, "queues": {""}, "verbs": {"claim"}},
		"both empty":     {"csrf_token": {csrf}, "queues": {"  "}, "verbs": {","}},
	} {
		rec := postForm(t, r, token, "/agents/"+agent.ID+"/vend", form)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", name, rec.Code)
		}
	}

	eps, err := st.ListEndpoints(ctx, agent.ID)
	if err != nil {
		t.Fatalf("list endpoints: %v", err)
	}
	if len(eps) != 0 {
		t.Fatalf("rejected vends minted %d endpoint(s); want 0", len(eps))
	}
}

// TestVendRevealsCredentialOnceWithHTTPWiring: a valid vend returns the plaintext credential once,
// with HTTP-only wiring, and persists only the hash + prefix. The plaintext is not recoverable on a
// later GET, and the persisted prefix is a prefix of the revealed credential (never the whole thing).
// SPEC-0012 scenario "Credential is shown once and stored only as a hash"; SPEC-0014 "Vend reveal
// shows HTTP wiring".
func TestVendRevealsCredentialOnceWithHTTPWiring(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	human, token := mintSession(t, st, ctx, "test|alice", "Alice Ames", "alice@example.com")
	agent, err := st.CreateAgent(ctx, human.ID, "alice-reviewer", "")
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	csrf := scrapeCSRF(t, getAs(t, r, token, "/agents/"+agent.ID).Body.String())

	rec := postForm(t, r, token, "/agents/"+agent.ID+"/vend",
		url.Values{"csrf_token": {csrf}, "queues": {"reviews, deploys"}, "verbs": {"claim, complete"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("vend: got %d, want 200", rec.Code)
	}
	// html/template escapes the JSON block's quotes; unescape so the wiring assertions read the
	// literal .mcp.json a human would copy off the page.
	body := html.UnescapeString(rec.Body.String())

	cred := credRe.FindString(body)
	if cred == "" {
		t.Fatalf("no plaintext credential revealed on vended page")
	}
	for _, want := range []string{`"type": "http"`, "/mcp/", "Bearer " + cred} {
		if !strings.Contains(body, want) {
			t.Errorf("vended page missing HTTP wiring element %q", want)
		}
	}
	for _, bad := range []string{`"command"`, "SWITCHBOARD_TOKEN", "on your PATH", "switchboard channel"} {
		if strings.Contains(body, bad) {
			t.Errorf("vended page leaks retired stdio marker %q", bad)
		}
	}

	// Exactly one endpoint persisted, storing only a prefix — never the full plaintext.
	eps, err := st.ListEndpoints(ctx, agent.ID)
	if err != nil {
		t.Fatalf("list endpoints: %v", err)
	}
	if len(eps) != 1 {
		t.Fatalf("got %d endpoints, want 1", len(eps))
	}
	ep := eps[0]
	if !strings.HasPrefix(cred, ep.CredentialPrefix) {
		t.Errorf("stored prefix %q is not a prefix of revealed credential %q", ep.CredentialPrefix, cred)
	}
	if ep.CredentialPrefix == cred {
		t.Errorf("stored prefix equals the full credential — plaintext leaked at rest")
	}
	if !strings.Contains(body, "/mcp/"+ep.Slug) {
		t.Errorf("wiring URL does not carry the minted slug %q", ep.Slug)
	}

	// One-time reveal: the plaintext is gone on any later render of the agent screen.
	if agentBody := getAs(t, r, token, "/agents/"+agent.ID).Body.String(); strings.Contains(agentBody, cred) {
		t.Errorf("agent screen re-rendered the one-time plaintext credential")
	}
}
