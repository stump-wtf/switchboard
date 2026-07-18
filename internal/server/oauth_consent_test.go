// End-to-end consent-flow tests through the real router + PostgreSQL, driven the way an MCP
// client + a browser drive it: the client opens /oauth/authorize with its discovered parameters,
// the human session gates the screen, the plain-form decision POST issues (or refuses) the
// single-use PKCE-bound code. Binds SPEC-0016 REQ "Authorization Code Flow With Consent" — both
// scenarios ("Consent reflects real enforcement", "Human absent") plus redirect-mismatch
// dead-ends, PKCE failure modes at authorize time, denial, and code replay revocation. Skipped
// without SWITCHBOARD_TEST_DATABASE_URL. Governing: ADR-0019.
package server

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/joestump/switchboard/internal/cred"
	"github.com/joestump/switchboard/internal/store"
)

const (
	consentRedirect  = "https://client.example.com/cb"
	consentChallenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" // a real S256 challenge shape
)

// consentFixture vends an endpoint scoped to queues github+ci and verbs
// list_todos+claim+complete for the given human, and registers an OAuth client.
func consentFixture(t *testing.T, st *store.Store, ctx context.Context, ownerID, agentName, slug, clientID string) store.Endpoint {
	t.Helper()
	ag, err := st.CreateAgent(ctx, ownerID, agentName, "")
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	ep, err := st.CreateEndpoint(ctx, ag.ID, "cred-hash-"+slug, "sbk_test…", slug,
		[]string{"github", "ci"}, []string{"list_todos", "claim", "complete"})
	if err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
	if _, err := st.CreateOAuthClient(ctx, clientID, "Claude Desktop", []string{consentRedirect}); err != nil {
		t.Fatalf("create oauth client: %v", err)
	}
	return ep
}

// authorizeQuery builds the authorize request the MCP client would open, binding via the RFC 8707
// resource indicator (the mount URL it discovered).
func authorizeQuery(clientID, slug string) url.Values {
	return url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {consentRedirect},
		"state":                 {"st-e2e"},
		"code_challenge":        {consentChallenge},
		"code_challenge_method": {"S256"},
		"resource":              {"https://sb.example.com/mcp/" + slug},
	}
}

// decisionForm converts the authorize query into the consent POST the rendered form submits.
func decisionForm(q url.Values, csrf, decision string) url.Values {
	f := url.Values{}
	for k, vs := range q {
		if k != "resource" {
			f.Set(k, vs[0])
		}
	}
	f.Set("endpoint", strings.TrimPrefix(q.Get("resource"), "https://sb.example.com/mcp/"))
	f.Set("csrf_token", csrf)
	f.Set("decision", decision)
	return f
}

// locationQuery asserts a 302 Found to the registered redirect URI and returns its query.
func locationQuery(t *testing.T, code int, location string) url.Values {
	t.Helper()
	if code != http.StatusFound {
		t.Fatalf("got %d, want 302", code)
	}
	u, err := url.Parse(location)
	if err != nil {
		t.Fatalf("Location %q does not parse: %v", location, err)
	}
	if got := u.Scheme + "://" + u.Host + u.Path; got != consentRedirect {
		t.Fatalf("redirected to %q, want %q", got, consentRedirect)
	}
	return u.Query()
}

// TestConsentFlowApprove walks the whole story: anonymous → login (scenario "Human absent");
// authenticated → the consent screen with bullets derived from the STORED scope (scenario
// "Consent reflects real enforcement"); approve → 302 back to the registered redirect URI with a
// single-use code that is stored hashed, PKCE-bound, endpoint-bound — and whose replay is
// rejected.
func TestConsentFlowApprove(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	human, session := mintSession(t, st, ctx, "test|consent", "Joe Stump", "joe@example.com")
	ep := consentFixture(t, st, ctx, human.ID, "ci-responder", "ci-responder-ab12cd34", "cid-consent")
	q := authorizeQuery("cid-consent", "ci-responder-ab12cd34")
	authPath := "/oauth/authorize?" + q.Encode()

	// Scenario "Human absent": no session → through login first, nothing rendered, nothing minted.
	anon := anonRequest(t, r, http.MethodGet, authPath)
	if anon.Code != http.StatusFound || anon.Header().Get("Location") != "/login" {
		t.Fatalf("anonymous authorize: got %d → %q, want 302 → /login", anon.Code, anon.Header().Get("Location"))
	}

	// Authenticated: the consent screen renders client, workspace, principal, and ONLY the stored
	// scope, as bullets derived from scope_queues/scope_verbs.
	c := newWizClient(t, r, session)
	page := c.get(authPath)
	if page.Code != http.StatusOK {
		t.Fatalf("consent GET: got %d (body %.300s)", page.Code, page.Body.String())
	}
	body := page.Body.String()
	for _, want := range []string{
		"Claude Desktop",                    // registered client name
		"sb.example.com",                    // the workspace host
		"Joe Stump · joe@example.com",       // the accountable principal
		"read todos on github · ci",         // bullet from scope_queues + list_todos
		"claim &amp; complete under a lease", // bullet from the stored lease verbs
		`data-sb-oauth-consent`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("consent page: missing %q", want)
		}
	}
	// Nothing advertised beyond the stored scope (the scope guard would refuse it).
	for _, banned := range []string{"webhook", "event history", "fail &"} {
		if strings.Contains(body, banned) {
			t.Errorf("consent page advertises %q, which is outside the stored scope", banned)
		}
	}

	// Approve: 302 back to the registered redirect URI with code + echoed state.
	csrf := scrapeCSRF(t, body)
	rec := c.do(http.MethodPost, "/oauth/authorize", decisionForm(q, csrf, "approve"))
	loc := locationQuery(t, rec.Code, rec.Header().Get("Location"))
	if loc.Get("state") != "st-e2e" {
		t.Fatalf("state = %q, want st-e2e", loc.Get("state"))
	}
	code := loc.Get("code")
	if code == "" || loc.Get("error") != "" {
		t.Fatalf("approve redirect query = %v", loc)
	}

	// The stored row is the code's HASH, bound to the endpoint, the client, the challenge, and the
	// exact redirect URI — redeemable exactly once.
	grant, err := st.RedeemOAuthCode(ctx, cred.Hash(code))
	if err != nil {
		t.Fatalf("redeem by hash: %v (the plaintext must be stored hashed)", err)
	}
	if grant.EndpointID != ep.ID || grant.ClientID != "cid-consent" ||
		grant.PKCEChallenge != consentChallenge || grant.RedirectURI != consentRedirect {
		t.Fatalf("stored grant = %+v", grant)
	}
	if !grant.ExpiresAt.After(time.Now()) {
		t.Fatal("issued code must carry a future expiry")
	}
	if _, err := st.RedeemOAuthCode(ctx, cred.Hash(code)); !errors.Is(err, store.ErrCodeReplayed) {
		t.Fatalf("replay: err = %v, want ErrCodeReplayed", err)
	}
}

// TestConsentFlowDeny: denial returns the standard error to the client — access_denied at the
// registered redirect URI, state echoed, nothing minted.
func TestConsentFlowDeny(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	human, session := mintSession(t, st, ctx, "test|consent-deny", "Joe Stump", "joe@example.com")
	consentFixture(t, st, ctx, human.ID, "deny-bot", "deny-bot-ab12cd34", "cid-deny")
	q := authorizeQuery("cid-deny", "deny-bot-ab12cd34")

	c := newWizClient(t, r, session)
	csrf := scrapeCSRF(t, c.get("/oauth/authorize?"+q.Encode()).Body.String())
	rec := c.do(http.MethodPost, "/oauth/authorize", decisionForm(q, csrf, "deny"))
	loc := locationQuery(t, rec.Code, rec.Header().Get("Location"))
	if loc.Get("error") != "access_denied" || loc.Get("state") != "st-e2e" || loc.Get("code") != "" {
		t.Fatalf("deny redirect query = %v", loc)
	}
}

// TestConsentRequestValidation: redirect mismatch and unknown client dead-end on our page (never
// a bounce — that would BE the open redirect); PKCE-less and foreign-endpoint requests return the
// standard errors to the validated redirect URI.
func TestConsentRequestValidation(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	human, session := mintSession(t, st, ctx, "test|consent-val", "Joe Stump", "joe@example.com")
	consentFixture(t, st, ctx, human.ID, "val-bot", "val-bot-ab12cd34", "cid-val")
	c := newWizClient(t, r, session)

	// Unregistered redirect_uri → 400 dead end, no Location.
	q := authorizeQuery("cid-val", "val-bot-ab12cd34")
	q.Set("redirect_uri", "https://evil.example.com/cb")
	rec := c.get("/oauth/authorize?" + q.Encode())
	if rec.Code != http.StatusBadRequest || rec.Header().Get("Location") != "" {
		t.Fatalf("redirect mismatch: got %d → %q, want 400 dead end", rec.Code, rec.Header().Get("Location"))
	}
	if !strings.Contains(rec.Body.String(), "data-sb-oauth-error") {
		t.Error("redirect mismatch must render the error state")
	}

	// Unknown client_id → 400 dead end (no redirect URI can be trusted).
	q = authorizeQuery("cid-never-registered", "val-bot-ab12cd34")
	if rec := c.get("/oauth/authorize?" + q.Encode()); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown client: got %d, want 400", rec.Code)
	}

	// Missing PKCE → invalid_request at the registered redirect URI (OAuth 2.1: no PKCE, no flow).
	q = authorizeQuery("cid-val", "val-bot-ab12cd34")
	q.Del("code_challenge")
	rec = c.get("/oauth/authorize?" + q.Encode())
	if loc := locationQuery(t, rec.Code, rec.Header().Get("Location")); loc.Get("error") != "invalid_request" {
		t.Fatalf("missing PKCE: error = %q, want invalid_request", loc.Get("error"))
	}

	// A foreign principal's endpoint binds nothing: same error as nonexistent (no leak), and no
	// consent screen for an endpoint the signed-in human does not own.
	mallory, mallorySession := mintSession(t, st, ctx, "test|consent-mallory", "Mallory", "m@example.com")
	_ = mallory
	mc := newWizClient(t, r, mallorySession)
	q = authorizeQuery("cid-val", "val-bot-ab12cd34")
	rec = mc.get("/oauth/authorize?" + q.Encode())
	if loc := locationQuery(t, rec.Code, rec.Header().Get("Location")); loc.Get("error") != "invalid_target" {
		t.Fatalf("foreign endpoint: error = %q, want invalid_target", loc.Get("error"))
	}

	// Revoked endpoint: consent cannot bind a dead line — same standard error.
	ep, err := st.EndpointBySlugOwned(ctx, "val-bot-ab12cd34", human.ID)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if err := st.RevokeEndpoint(ctx, ep.ID, human.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	q = authorizeQuery("cid-val", "val-bot-ab12cd34")
	rec = c.get("/oauth/authorize?" + q.Encode())
	if loc := locationQuery(t, rec.Code, rec.Header().Get("Location")); loc.Get("error") != "invalid_target" {
		t.Fatalf("revoked endpoint: error = %q, want invalid_target", loc.Get("error"))
	}
}

// TestConsentDecisionRequiresCSRF: the decision POST is a session mutation like any other — a
// missing synchronizer token is refused before any code is minted.
func TestConsentDecisionRequiresCSRF(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	human, session := mintSession(t, st, ctx, "test|consent-csrf", "Joe Stump", "joe@example.com")
	consentFixture(t, st, ctx, human.ID, "csrf-bot", "csrf-bot-ab12cd34", "cid-csrf")
	q := authorizeQuery("cid-csrf", "csrf-bot-ab12cd34")

	c := newWizClient(t, r, session)
	rec := c.do(http.MethodPost, "/oauth/authorize", decisionForm(q, "", "approve"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("decision without CSRF: got %d, want 403", rec.Code)
	}
}
