// End-to-end vend-flow tests through the real router + PostgreSQL: the vend form rejects a missing
// scope with 400 and mints nothing, and a successful vend reveals the plaintext credential exactly
// once with HTTP-only .mcp.json wiring while persisting only its hash and prefix. Skipped without
// SWITCHBOARD_TEST_DATABASE_URL (both CI hosts provide a Postgres service; local runs need
// `make ci`).
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

// TestVendRejectsMissingScopeAndMintsNothing: an empty name, queues, or verbs field is a 400 and
// mints nothing — the validation runs before any store write, so no agent and no endpoint are left
// behind on the rejection path. SPEC-0013 scenario "Vend modal validates scope".
func TestVendRejectsMissingScopeAndMintsNothing(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	human, token := mintSession(t, st, ctx, "test|alice", "Alice Ames", "alice@example.com")
	csrf := scrapeCSRF(t, getAs(t, r, token, "/endpoints").Body.String())

	for name, form := range map[string]url.Values{
		"missing name":   {"csrf_token": {csrf}, "name": {"  "}, "queues": {"reviews"}, "verbs": {"claim"}},
		"missing verbs":  {"csrf_token": {csrf}, "name": {"bot"}, "queues": {"reviews"}, "verbs": {""}},
		"missing queues": {"csrf_token": {csrf}, "name": {"bot"}, "queues": {""}, "verbs": {"claim"}},
		"both empty":     {"csrf_token": {csrf}, "name": {"bot"}, "queues": {"  "}, "verbs": {","}},
	} {
		rec := postForm(t, r, token, "/endpoints/vend", form)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", name, rec.Code)
		}
	}

	// No agent and no endpoint were created by any rejected vend.
	cards, err := st.ListEndpointCards(ctx, human.ID)
	if err != nil {
		t.Fatalf("list endpoint cards: %v", err)
	}
	if len(cards) != 0 {
		t.Fatalf("rejected vends minted %d endpoint(s); want 0", len(cards))
	}
	agents, err := st.ListAgents(ctx, human.ID)
	if err != nil {
		t.Fatalf("list agents: %v", err)
	}
	if len(agents) != 0 {
		t.Fatalf("rejected vends created %d agent(s); want 0", len(agents))
	}
}

// TestVendRevealsCredentialOnceWithHTTPWiring: a valid vend creates the named agent + endpoint,
// returns the plaintext credential once with HTTP-only wiring, and persists only the hash + prefix.
// The plaintext is not recoverable on a later GET /endpoints, and the persisted prefix is a prefix of
// the revealed credential (never the whole thing). SPEC-0013 scenario "Credential reveal is one-time";
// SPEC-0014 "Vend reveal shows HTTP wiring".
func TestVendRevealsCredentialOnceWithHTTPWiring(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	human, token := mintSession(t, st, ctx, "test|alice", "Alice Ames", "alice@example.com")
	csrf := scrapeCSRF(t, getAs(t, r, token, "/endpoints").Body.String())

	rec := postForm(t, r, token, "/endpoints/vend",
		url.Values{"csrf_token": {csrf}, "name": {"alice-reviewer"}, "queues": {"reviews, deploys"}, "verbs": {"claim", "complete"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("vend: got %d, want 200", rec.Code)
	}
	// html/template escapes the JSON block's quotes; unescape so the wiring assertions read the
	// literal .mcp.json a human would copy off the reveal.
	body := html.UnescapeString(rec.Body.String())

	cred := credRe.FindString(body)
	if cred == "" {
		t.Fatalf("no plaintext credential revealed on the vend reveal")
	}
	for _, want := range []string{`"type": "http"`, "/mcp/", "Bearer " + cred} {
		if !strings.Contains(body, want) {
			t.Errorf("vend reveal missing HTTP wiring element %q", want)
		}
	}
	for _, bad := range []string{`"command"`, "SWITCHBOARD_TOKEN", "on your PATH", "switchboard channel"} {
		if strings.Contains(body, bad) {
			t.Errorf("vend reveal leaks retired stdio marker %q", bad)
		}
	}

	// Exactly one endpoint persisted, storing only a prefix — never the full plaintext.
	cards, err := st.ListEndpointCards(ctx, human.ID)
	if err != nil {
		t.Fatalf("list endpoint cards: %v", err)
	}
	if len(cards) != 1 {
		t.Fatalf("got %d endpoints, want 1", len(cards))
	}
	ep := cards[0]
	if ep.AgentName != "alice-reviewer" {
		t.Errorf("endpoint bound to agent %q, want alice-reviewer", ep.AgentName)
	}
	if !strings.HasPrefix(cred, ep.CredentialPrefix) {
		t.Errorf("stored prefix %q is not a prefix of revealed credential %q", ep.CredentialPrefix, cred)
	}
	if ep.CredentialPrefix == cred {
		t.Errorf("stored prefix equals the full credential — plaintext leaked at rest")
	}
	if !strings.Contains(body, "/mcp/"+ep.Slug) {
		t.Errorf("wiring URL does not carry the minted slug %q", ep.Slug)
	}

	// One-time reveal: the plaintext is gone on any later render of the Endpoints view (only the
	// prefix persists), so the credential is unrecoverable from the UI once the modal closes.
	epsBody := getAs(t, r, token, "/endpoints").Body.String()
	if strings.Contains(epsBody, cred) {
		t.Errorf("endpoints view re-rendered the one-time plaintext credential")
	}
	if !strings.Contains(epsBody, ep.CredentialPrefix) {
		t.Errorf("endpoints view should still show the credential display prefix")
	}
}
