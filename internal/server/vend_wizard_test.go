// End-to-end vend-wizard tests through the real router + PostgreSQL, driven exactly the way a
// browser WITH JAVASCRIPT DISABLED drives it: plain GETs, plain form POSTs, redirects, cookies.
// They bind the SPEC-0015 wizard contract — routed step pages, server-side step state,
// value-preserving back navigation, confirm-before-mint, the one-time reveal with both .mcp.json
// variants, the recorded expiry for a chosen lifetime, and the revoke confirm flow. Skipped
// without SWITCHBOARD_TEST_DATABASE_URL (both CI hosts provide a Postgres service; local runs
// need `make ci`). Governing: SPEC-0015 REQ "Endpoints View And Vend Wizard" (scenario "Lifetime chosen
// at vend"), REQ "Wizard Interaction Pattern" (scenario "JavaScript disabled"); SPEC-0016 REQ
// "Credential Lifetime"; SPEC-0007 (one-time reveal + revoke semantics).
package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

// wizClient is a minimal cookie-jar client over the test router: it carries the session cookie
// plus whatever cookies the server sets (the wizard's server-side-state token), like a browser.
type wizClient struct {
	t       *testing.T
	r       chi.Router
	session string
	cookies map[string]string
}

func newWizClient(t *testing.T, r chi.Router, session string) *wizClient {
	return &wizClient{t: t, r: r, session: session, cookies: map[string]string{}}
}

func (c *wizClient) do(method, path string, form url.Values) *httptest.ResponseRecorder {
	c.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: c.session})
	for k, v := range c.cookies {
		req.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	rec := httptest.NewRecorder()
	c.r.ServeHTTP(rec, req)
	for _, ck := range rec.Result().Cookies() {
		if ck.MaxAge < 0 || ck.Value == "" {
			delete(c.cookies, ck.Name)
		} else {
			c.cookies[ck.Name] = ck.Value
		}
	}
	return rec
}

func (c *wizClient) get(path string) *httptest.ResponseRecorder {
	return c.do(http.MethodGet, path, nil)
}

// followTo asserts a 303 See Other to want and returns the redirect target.
func followTo(t *testing.T, rec *httptest.ResponseRecorder, want string) string {
	t.Helper()
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("got %d (body %.200s), want 303", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if loc != want {
		t.Fatalf("redirected to %q, want %q", loc, want)
	}
	return loc
}

// TestVendWizardCompletesWithoutJS walks the whole wizard as a no-JS browser: start → persona →
// queues → verbs → webhooks (left empty: self-managed webhooks disabled) → lifetime (7d) →
// confirm → one-time reveal, checking value-preserving back navigation along the way, the
// recorded expiry, the countdown data on the card, and that the plaintext credential is
// unrecoverable afterwards.
func TestVendWizardCompletesWithoutJS(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	human, token := mintSession(t, st, ctx, "test|alice", "Alice Ames", "alice@example.com")
	c := newWizClient(t, r, token)
	csrf := scrapeCSRF(t, c.get("/endpoints").Body.String())

	// Start: mints server-side state (cookie) and lands on the first step.
	followTo(t, c.get("/endpoints/vend"), "/endpoints/vend/persona")
	if _, ok := c.cookies["sb_wiz_vend"]; !ok {
		t.Fatal("wizard start did not set the server-side state token cookie")
	}

	// Step 1 — persona: a full page with a plain form.
	step1 := c.get("/endpoints/vend/persona")
	if step1.Code != http.StatusOK {
		t.Fatalf("persona step: got %d", step1.Code)
	}
	for _, want := range []string{`data-sb-wizard="vend"`, `method="post"`, `action="/endpoints/vend/persona"`} {
		if !strings.Contains(step1.Body.String(), want) {
			t.Errorf("persona step: missing %q", want)
		}
	}
	followTo(t, c.do(http.MethodPost, "/endpoints/vend/persona",
		url.Values{"csrf_token": {csrf}, "name": {"wizard-bot"}}), "/endpoints/vend/queues")

	// Step 2 — queues (empty store → free-text field).
	followTo(t, c.do(http.MethodPost, "/endpoints/vend/queues",
		url.Values{"csrf_token": {csrf}, "queues_extra": {"reviews, deploys"}}), "/endpoints/vend/verbs")

	// Back navigation preserves entered values (SPEC-0015): both earlier steps re-render the draft.
	back1 := c.get("/endpoints/vend/persona").Body.String()
	if !strings.Contains(back1, `value="wizard-bot"`) {
		t.Error("back nav: persona step lost the entered agent name")
	}
	back2 := c.get("/endpoints/vend/queues").Body.String()
	if !strings.Contains(back2, `value="reviews, deploys"`) {
		t.Error("back nav: queues step lost the entered queues")
	}

	// Step 3 — verbs.
	followTo(t, c.do(http.MethodPost, "/endpoints/vend/verbs",
		url.Values{"csrf_token": {csrf}, "verbs": {"claim", "complete"}}), "/endpoints/vend/webhooks")
	// Revisit renders the operator's own selection, not the drain-verb defaults.
	backVerbs := c.get("/endpoints/vend/verbs").Body.String()
	if !strings.Contains(backVerbs, `value="claim" checked`) || !strings.Contains(backVerbs, `value="complete" checked`) {
		t.Error("back nav: verbs step lost the chosen verbs")
	}
	if strings.Contains(backVerbs, `value="list_todos" checked`) {
		t.Error("back nav: verbs step re-checked a default the operator deselected")
	}

	// Step 4 — webhooks: the optional self-managed webhook ceiling (ADR-0012 / SPEC-0006). Posted
	// empty, which vends with webhook_max=0 — self-managed webhooks disabled.
	followTo(t, c.do(http.MethodPost, "/endpoints/vend/webhooks",
		url.Values{"csrf_token": {csrf}}), "/endpoints/vend/lifetime")

	// Step 5 — lifetime: the scenario's 7-day choice.
	followTo(t, c.do(http.MethodPost, "/endpoints/vend/lifetime",
		url.Values{"csrf_token": {csrf}, "lifetime": {"7d"}}), "/endpoints/vend/confirm")

	// Step 6 — confirm: summarizes the draft and states the irreversibility.
	confirm := c.get("/endpoints/vend/confirm").Body.String()
	for _, want := range []string{"wizard-bot", "reviews", "deploys", "claim", "complete", ">7d<", "Vending is irreversible"} {
		if !strings.Contains(confirm, want) {
			t.Errorf("confirm step: missing %q", want)
		}
	}

	// The mint: the confirm POST carries ONLY the CSRF token — scope comes from server-side state.
	reveal := c.do(http.MethodPost, "/endpoints/vend/confirm", url.Values{"csrf_token": {csrf}})
	if reveal.Code != http.StatusOK {
		t.Fatalf("confirm POST: got %d (body %.300s)", reveal.Code, reveal.Body.String())
	}
	body := reveal.Body.String()
	cred := credRe.FindString(body)
	if cred == "" {
		t.Fatal("no plaintext credential on the wizard's one-time reveal")
	}
	for _, want := range []string{"data-sb-reveal-wiring", "data-sb-reveal-wiring-url-only", "URL-only wiring"} {
		if !strings.Contains(body, want) {
			t.Errorf("wizard reveal: missing %q", want)
		}
	}

	// The endpoint recorded its scope and the ~7d expiry (SPEC-0015 scenario "Lifetime chosen at
	// vend"; the reaper enforces expires_at — bound in reaper_test/endpoint_expiry_test).
	cards, err := st.ListEndpointCards(ctx, human.ID)
	if err != nil {
		t.Fatalf("list endpoint cards: %v", err)
	}
	if len(cards) != 1 {
		t.Fatalf("got %d endpoints, want 1", len(cards))
	}
	ep := cards[0]
	if ep.AgentName != "wizard-bot" {
		t.Errorf("agent = %q, want wizard-bot", ep.AgentName)
	}
	if len(ep.ScopeQueues) != 2 || len(ep.ScopeVerbs) != 2 {
		t.Errorf("scope = %v / %v, want 2 queues / 2 verbs", ep.ScopeQueues, ep.ScopeVerbs)
	}
	if ep.ExpiresAt == nil {
		t.Fatal("wizard vend with lifetime=7d recorded no expires_at")
	}
	want := time.Now().Add(7 * 24 * time.Hour)
	if d := ep.ExpiresAt.Sub(want); d > time.Minute || d < -time.Minute {
		t.Errorf("expires_at %v is not ~7d out (drift %v)", ep.ExpiresAt, d)
	}

	// The card shows the countdown (chip + machine-readable data)…
	epsBody := c.get("/endpoints").Body.String()
	if !strings.Contains(epsBody, "sb-ep-expiry-"+ep.ID) || !strings.Contains(epsBody, "data-sb-expires-at=") {
		t.Error("endpoints view missing the countdown chip/data for the expiring card")
	}
	// …and the plaintext credential is gone forever (one-time reveal, SPEC-0007).
	if strings.Contains(epsBody, cred) {
		t.Error("endpoints view re-rendered the one-time plaintext credential")
	}

	// The wizard state died with the mint: revisiting the confirm step restarts the flow.
	followTo(t, c.get("/endpoints/vend/confirm"), "/endpoints/vend")
}

// TestVendWizardValidatesEachStep: step validation failures re-render the SAME step page (400)
// with an inline error, and a confirm POST on an incomplete draft bounces to the first unfinished
// step instead of minting.
func TestVendWizardValidatesEachStep(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	human, token := mintSession(t, st, ctx, "test|alice", "Alice Ames", "alice@example.com")
	c := newWizClient(t, r, token)
	csrf := scrapeCSRF(t, c.get("/endpoints").Body.String())
	followTo(t, c.get("/endpoints/vend"), "/endpoints/vend/persona")

	// Confirm on a fresh (empty) draft: no mint, bounce to the first incomplete step.
	followTo(t, c.do(http.MethodPost, "/endpoints/vend/confirm",
		url.Values{"csrf_token": {csrf}}), "/endpoints/vend/persona")

	// Blank name → the persona step re-renders with the error.
	rec := c.do(http.MethodPost, "/endpoints/vend/persona", url.Values{"csrf_token": {csrf}, "name": {"  "}})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "data-sb-wiz-error") {
		t.Errorf("blank name: got %d, want 400 with an inline step error", rec.Code)
	}
	// No queues → same pattern.
	rec = c.do(http.MethodPost, "/endpoints/vend/queues", url.Values{"csrf_token": {csrf}, "queues_extra": {" , "}})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "data-sb-wiz-error") {
		t.Errorf("no queues: got %d, want 400 with an inline step error", rec.Code)
	}
	// Malformed custom lifetime → same pattern.
	rec = c.do(http.MethodPost, "/endpoints/vend/lifetime",
		url.Values{"csrf_token": {csrf}, "lifetime": {"custom"}, "lifetime_custom": {"soon"}})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "data-sb-wiz-error") {
		t.Errorf("bad lifetime: got %d, want 400 with an inline step error", rec.Code)
	}
	// An unknown step slug is a 404, not a render of arbitrary state.
	if rec := c.get("/endpoints/vend/nope"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown step: got %d, want 404", rec.Code)
	}
	// A step page with NO live wizard state (cold deep link) restarts the flow.
	cold := newWizClient(t, r, token)
	followTo(t, cold.get("/endpoints/vend/queues"), "/endpoints/vend")

	// Nothing was minted by any of it.
	cards, err := st.ListEndpointCards(ctx, human.ID)
	if err != nil {
		t.Fatalf("list endpoint cards: %v", err)
	}
	if len(cards) != 0 {
		t.Fatalf("validation failures minted %d endpoint(s); want 0", len(cards))
	}
}

// TestVendWizardReVendSeedsFromEndpoint: the card's Rotate action starts the wizard seeded from an
// existing endpoint's scope — the SPEC-0007 re-vend doctrine as a flow. Lifetime is NOT copied
// (expiry is re-chosen each vend), and the source endpoint is untouched.
func TestVendWizardReVendSeedsFromEndpoint(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	human, token := mintSession(t, st, ctx, "test|alice", "Alice Ames", "alice@example.com")
	c := newWizClient(t, r, token)
	csrf := scrapeCSRF(t, c.get("/endpoints").Body.String())

	// Vend the source endpoint through the direct form path (with a lifetime, to prove it is not
	// copied into the seeded draft).
	rec := c.do(http.MethodPost, "/endpoints/vend", url.Values{
		"csrf_token": {csrf}, "name": {"old-bot"}, "queues": {"reviews"}, "verbs": {"claim"}, "lifetime": {"24h"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("seed vend: got %d", rec.Code)
	}
	cards, err := st.ListEndpointCards(ctx, human.ID)
	if err != nil || len(cards) != 1 {
		t.Fatalf("seed vend cards: %v / %d", err, len(cards))
	}
	src := cards[0]

	// The card renders Rotate → the seeded wizard start.
	if !strings.Contains(c.get("/endpoints").Body.String(), "/endpoints/vend?from="+src.ID) {
		t.Fatal("active card missing its Rotate (re-vend) link")
	}
	followTo(t, c.get("/endpoints/vend?from="+src.ID), "/endpoints/vend/persona")

	// Seeded values render through the steps: name + re-vend banner, queues, verbs.
	persona := c.get("/endpoints/vend/persona").Body.String()
	if !strings.Contains(persona, `value="old-bot"`) || !strings.Contains(persona, "re-vend of old-bot") {
		t.Error("re-vend: persona step not seeded from the source endpoint")
	}
	queues := c.get("/endpoints/vend/queues").Body.String()
	if !strings.Contains(queues, "reviews") {
		t.Error("re-vend: queues step not seeded from the source scope")
	}
	verbs := c.get("/endpoints/vend/verbs").Body.String()
	if !strings.Contains(verbs, `value="claim" checked`) {
		t.Error("re-vend: verbs step not seeded from the source scope")
	}
	if strings.Contains(verbs, `value="list_todos" checked`) {
		t.Error("re-vend: verbs step must render the SOURCE scope, not the drain defaults")
	}
	lifetime := c.get("/endpoints/vend/lifetime").Body.String()
	if !strings.Contains(lifetime, `name="lifetime" value="" checked`) {
		t.Error("re-vend: lifetime must reset to until-revoked, never copy the source expiry")
	}

	// A foreign or unknown id seeds nothing (and never errors the start).
	other := newWizClient(t, r, token)
	followTo(t, other.get("/endpoints/vend?from=00000000-0000-0000-0000-000000000000"), "/endpoints/vend/persona")
	if strings.Contains(other.get("/endpoints/vend/persona").Body.String(), "re-vend of") {
		t.Error("unknown ?from id must start an unseeded wizard")
	}
}

// TestRevokeConfirmFlow: revoking goes through the full-page confirm (GET shows exactly which
// capability dies; only its POST executes), per SPEC-0015 "irreversible steps confirm". Unknown /
// foreign / already-revoked endpoints 404 on the confirm page.
func TestRevokeConfirmFlow(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	human, token := mintSession(t, st, ctx, "test|alice", "Alice Ames", "alice@example.com")
	c := newWizClient(t, r, token)
	csrf := scrapeCSRF(t, c.get("/endpoints").Body.String())

	rec := c.do(http.MethodPost, "/endpoints/vend", url.Values{
		"csrf_token": {csrf}, "name": {"doomed-bot"}, "queues": {"reviews"}, "verbs": {"claim"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("vend: got %d", rec.Code)
	}
	cards, err := st.ListEndpointCards(ctx, human.ID)
	if err != nil || len(cards) != 1 {
		t.Fatalf("cards: %v / %d", err, len(cards))
	}
	ep := cards[0]

	// The card links to the confirm page and carries no direct kill form.
	epsBody := c.get("/endpoints").Body.String()
	if !strings.Contains(epsBody, `href="/endpoints/`+ep.ID+`/revoke"`) {
		t.Fatal("active card missing its Revoke confirm link")
	}
	if strings.Contains(epsBody, `action="/endpoints/`+ep.ID+`/revoke"`) {
		t.Fatal("card must not POST the revoke directly — irreversible steps confirm first")
	}

	// The confirm page names the capability and hosts the only kill form.
	confirm := c.get("/endpoints/" + ep.ID + "/revoke")
	if confirm.Code != http.StatusOK {
		t.Fatalf("revoke confirm: got %d", confirm.Code)
	}
	for _, want := range []string{"doomed-bot", "data-sb-revoke-confirm", `action="/endpoints/` + ep.ID + `/revoke"`, "instant and total"} {
		if !strings.Contains(confirm.Body.String(), want) {
			t.Errorf("revoke confirm: missing %q", want)
		}
	}

	// Confirming kills it (303 back to the view; card flips to revoked).
	kill := c.do(http.MethodPost, "/endpoints/"+ep.ID+"/revoke", url.Values{"csrf_token": {csrf}})
	if kill.Code != http.StatusSeeOther {
		t.Fatalf("revoke POST: got %d, want 303", kill.Code)
	}
	cards, err = st.ListEndpointCards(ctx, human.ID)
	if err != nil || len(cards) != 1 {
		t.Fatalf("cards after revoke: %v / %d", err, len(cards))
	}
	if cards[0].State != "revoked" {
		t.Errorf("state = %q, want revoked", cards[0].State)
	}

	// Already revoked → the confirm page 404s (nothing left to confirm), matching the
	// existence-hiding answer a foreign or unknown id gets.
	if rec := c.get("/endpoints/" + ep.ID + "/revoke"); rec.Code != http.StatusNotFound {
		t.Errorf("confirm for a revoked endpoint: got %d, want 404", rec.Code)
	}
	_, mallory := mintSession(t, st, ctx, "test|mallory", "Mallory M", "mallory@example.com")
	m := newWizClient(t, r, mallory)
	if rec := m.get("/endpoints/" + ep.ID + "/revoke"); rec.Code != http.StatusNotFound {
		t.Errorf("foreign confirm: got %d, want 404 (existence-hiding)", rec.Code)
	}
}
