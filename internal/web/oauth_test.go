package web

// Consent-screen contract tests (SPEC-0016 REQ "Authorization Code Flow With Consent"): the
// scope-bullet derivation — bullets come from the endpoint's STORED scope_queues/scope_verbs, the
// same columns the MCP scope guard enforces, and never advertise beyond them — and the DOM render
// contract for templates/authorize.html (assertions key on data-sb-* hooks and ids, never style
// classes). The flow over HTTP (login gate, code issuance, denial, replay) is bound end-to-end in
// internal/server/oauth_consent_test.go. Governing: ADR-0019.

import (
	"strings"
	"testing"
)

// TestScopeBulletsDeriveFromStoredScope pins the spec scenario "Consent reflects real
// enforcement": for queues github+ci and verbs list_todos+claim+complete, the bullets are exactly
// the read line over those queues and the lease line over those verbs — nothing more.
func TestScopeBulletsDeriveFromStoredScope(t *testing.T) {
	got := scopeBullets([]string{"github", "ci"}, []string{"list_todos", "claim", "complete"})
	want := []string{
		"read todos on github · ci",
		"claim & complete under a lease",
	}
	if len(got) != len(want) {
		t.Fatalf("bullets = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("bullet[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestScopeBulletsNeverAdvertiseBeyondScope: families whose verbs are absent must contribute no
// bullet — a consent screen naming webhooks or event history for a drain-only endpoint would
// advertise what the scope guard refuses.
func TestScopeBulletsNeverAdvertiseBeyondScope(t *testing.T) {
	got := scopeBullets([]string{"github"}, []string{"list_todos"})
	if len(got) != 1 || got[0] != "read todos on github" {
		t.Fatalf("read-only scope bullets = %q, want exactly the read line", got)
	}
	for _, b := range got {
		for _, banned := range []string{"webhook", "event", "claim", "complete", "lease"} {
			if strings.Contains(b, banned) {
				t.Errorf("bullet %q advertises %q, which is out of scope", b, banned)
			}
		}
	}

	// Wider grants surface their families — with only the in-scope verbs named.
	wide := scopeBullets([]string{"ci"}, []string{"claim", "create_webhook", "list_webhook_events"})
	joined := strings.Join(wide, "\n")
	for _, want := range []string{
		"claim under a lease",
		"self-manage webhooks (create_webhook)",
		"inspect event history & providers (list_webhook_events)",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("wide-scope bullets %q missing %q", wide, want)
		}
	}
	if strings.Contains(joined, "read todos") {
		t.Errorf("bullets %q advertise reading without list_todos in scope", wide)
	}
	if strings.Contains(joined, "rotate_webhook") || strings.Contains(joined, "get_webhook_event") {
		t.Errorf("bullets %q name verbs outside the stored scope", wide)
	}

	// get_todo alone earns the read line, and names no verb beyond it (SPEC-0034 REQ-8).
	if got := scopeBullets([]string{"ci"}, []string{"get_todo"}); len(got) != 1 || got[0] != "read todos on ci" {
		t.Fatalf("get_todo-only bullets = %q, want exactly the read line", got)
	}
	// ...and beside list_todos it adds nothing: one read line, not two.
	if got := scopeBullets([]string{"ci"}, []string{"list_todos", "get_todo"}); len(got) != 1 {
		t.Fatalf("list_todos+get_todo bullets = %q, want one read line", got)
	}

	// A verb outside every known family renders verbatim, never embellished.
	odd := scopeBullets([]string{"q"}, []string{"future_verb"})
	if len(odd) != 1 || odd[0] != "future_verb" {
		t.Fatalf("unknown-verb bullets = %q, want the verb verbatim", odd)
	}
}

// TestAuthorizeConsentDOM: the consent page renders the client's ask, the workspace, the
// accountable principal, the endpoint binding, the derived bullets, and a plain method=post
// decision form with both decisions + the round-tripped request fields — completing with JS
// disabled per the SPEC-0015 page pattern.
func TestAuthorizeConsentDOM(t *testing.T) {
	h := newTestHandler(t)
	body := renderPage(t, h, "authorize", view{
		Title: "Authorize access", Human: testHuman(), CSRF: "tok",
		Shell: shell{Active: "endpoints", DBConnected: true, Initials: "JS"},
		Authorize: &authorizeView{
			ClientName: "Claude Desktop",
			Workspace:  "sb.example.com",
			Principal:  "Joe Stump · joe@example.com",
			Initials:   "JS",
			AgentName:  "ci-responder",
			Slug:       "ci-responder-ab12cd34",
			Bullets:    []string{"read todos on github · ci", "claim & complete under a lease"},

			ClientID:      "cid-abc",
			RedirectURI:   "https://client.example.com/cb",
			State:         "st-123",
			CodeChallenge: strings.Repeat("c", 43),
		},
	})
	for _, want := range []string{
		`data-sb-oauth-consent`, // the consent surface hook
		"authorize access",      // the surface title
		`<strong data-sb-oauth-client>Claude Desktop</strong>`, // client name, escaped render
		"wants to connect to",                                    // the ask copy
		`data-sb-oauth-principal`, "Joe Stump · joe@example.com", // accountable principal
		`data-sb-oauth-workspace`, "sb.example.com", // workspace
		`data-sb-oauth-endpoint`, "ci-responder", "/mcp/ci-responder-ab12cd34", // endpoint binding
		`data-sb-oauth-scope`, "this will allow the agent to", // scope panel
		`data-sb-oauth-bullet`, "read todos on github · ci", "claim &amp; complete under a lease",
		`method="post"`, `action="/oauth/authorize"`, // plain decision form (no-JS completion)
		`name="csrf_token" value="tok"`,
		`name="client_id" value="cid-abc"`,
		`name="redirect_uri" value="https://client.example.com/cb"`,
		`name="state" value="st-123"`,
		`name="code_challenge" value="` + strings.Repeat("c", 43) + `"`,
		`name="endpoint" value="ci-responder-ab12cd34"`,
		`name="decision" value="approve"`, `data-sb-oauth-approve`,
		`name="decision" value="deny"`, `data-sb-oauth-deny`,
		"revoke anytime in", // the endpoints pointer
	} {
		if !strings.Contains(body, want) {
			t.Errorf("consent page: missing %q", want)
		}
	}
	if strings.Contains(body, `data-sb-oauth-error`) {
		t.Error("consent page must not render the error state")
	}
}

// TestAuthorizeClientNameEscaped: the client name is attacker-registered data (RFC 7591 DCR is
// anonymous); a script-shaped name must render inert.
func TestAuthorizeClientNameEscaped(t *testing.T) {
	h := newTestHandler(t)
	body := renderPage(t, h, "authorize", view{
		Title: "Authorize access", Human: testHuman(), CSRF: "tok",
		Shell: shell{Active: "endpoints", Initials: "JS"},
		Authorize: &authorizeView{
			ClientName: `<script>alert(1)</script>`,
			Bullets:    []string{"read todos on q"},
		},
	})
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Fatal("client name rendered unescaped")
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Error("escaped client name not present")
	}
}

// TestAuthorizeErrorState: a request too broken to bounce back to the client (unknown client_id,
// unregistered redirect_uri) dead-ends on the page's error state — no consent form, no decision.
func TestAuthorizeErrorState(t *testing.T) {
	h := newTestHandler(t)
	body := renderPage(t, h, "authorize", view{
		Title: "Authorize access", Human: testHuman(), CSRF: "tok",
		Shell:     shell{Active: "endpoints", Initials: "JS"},
		Authorize: &authorizeView{Error: "unknown client — the client_id was never registered with this switchboard"},
	})
	for _, want := range []string{
		`data-sb-oauth-error`,
		"unknown client",
		`href="/endpoints"`, // the escape hatch
	} {
		if !strings.Contains(body, want) {
			t.Errorf("error state: missing %q", want)
		}
	}
	for _, banned := range []string{`data-sb-oauth-consent`, `name="decision"`, `data-sb-oauth-approve`} {
		if strings.Contains(body, banned) {
			t.Errorf("error state must not render %q", banned)
		}
	}
}

// TestPrincipalLabel covers the principal-line fallbacks.
func TestPrincipalLabel(t *testing.T) {
	if got := principalLabel(testHuman()); got != "Joe Stump · joe@example.com" {
		t.Errorf("principalLabel(full) = %q", got)
	}
	if got := principalLabel(nil); got != "" {
		t.Errorf("principalLabel(nil) = %q", got)
	}
}
