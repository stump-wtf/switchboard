package server

// The one-step quick vend (ADR-0023) through the real router + PostgreSQL, driven as a no-JS
// browser: the Endpoints view links it, the page is session-gated and CSRF-guarded, a rejected
// submission re-renders inline with every entered value preserved (the known-queue chips
// included), and a valid submission mints through the shared vend path and renders the one-time
// reveal. Skipped without SWITCHBOARD_TEST_DATABASE_URL. Governing: ADR-0023; SPEC-0015 (value
// preservation, no-JS completion); SPEC-0007 (one-time reveal).

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestQuickVendOneStep(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	human, session := mintSession(t, st, ctx, "test|quick", "Joe Stump", "joe@example.com")
	// A prior vend on "reviews" makes that queue a known chip on the quick page.
	vendFixture(t, st, ctx, human.ID, "seed-bot", "hash-seed", "sbk_seed…")
	c := newWizClient(t, r, session)

	// Session-gated like the rest of the operator UI.
	if anon := anonRequest(t, r, http.MethodGet, "/endpoints/quick"); anon.Code != http.StatusFound || anon.Header().Get("Location") != "/login" {
		t.Fatalf("anonymous quick vend: got %d → %q, want 302 → /login", anon.Code, anon.Header().Get("Location"))
	}

	// Reachable from the Endpoints view, not only by URL.
	if page := c.get("/endpoints"); page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `href="/endpoints/quick"`) {
		t.Fatalf("Endpoints view does not link the quick page (status %d)", page.Code)
	}

	page := c.get("/endpoints/quick")
	if page.Code != http.StatusOK {
		t.Fatalf("GET /endpoints/quick: %d (body %.300s)", page.Code, page.Body.String())
	}
	body := page.Body.String()
	for _, want := range []string{`data-sb-quickvend-form`, `action="/endpoints/quick"`, `name="queues" value="reviews"`, `name="verbs" value="claim" checked`} {
		if !strings.Contains(body, want) {
			t.Errorf("quick page: missing %q", want)
		}
	}
	csrf := scrapeCSRF(t, body)

	// A rejected submission (bad lifetime) re-renders inline with everything preserved — the chip
	// the operator ticked stays ticked, the extra queue stays in the free-text field.
	bad := url.Values{
		"csrf_token": {csrf}, "name": {"release-bot"},
		"queues": {"reviews"}, "queues_extra": {"deploys"},
		"verbs":    {"claim"},
		"lifetime": {"custom"}, "lifetime_custom": {"banana"},
	}
	rec := c.do(http.MethodPost, "/endpoints/quick", bad)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid lifetime: got %d, want 400 (body %.300s)", rec.Code, rec.Body.String())
	}
	body = rec.Body.String()
	for _, want := range []string{
		`data-sb-quickvend-error`, "invalid lifetime",
		`value="release-bot"`,
		`name="queues" value="reviews" checked`,
		`name="queues_extra" value="deploys"`,
		`name="verbs" value="claim" checked`,
		`name="lifetime" value="custom" checked`, `value="banana"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("error re-render: missing %q", want)
		}
	}
	if strings.Contains(body, `name="verbs" value="list_todos" checked`) {
		t.Error("error re-render: an unchosen verb came back checked")
	}
	for _, missing := range []url.Values{
		{"csrf_token": {csrf}, "name": {""}, "queues": {"reviews"}, "verbs": {"claim"}},
		{"csrf_token": {csrf}, "name": {"x"}, "verbs": {"claim"}},
		{"csrf_token": {csrf}, "name": {"x"}, "queues": {"reviews"}},
	} {
		if rec := c.do(http.MethodPost, "/endpoints/quick", missing); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "data-sb-quickvend-error") {
			t.Errorf("submission %v: got %d without an inline error", missing, rec.Code)
		}
	}
	// Without the CSRF token the mint is refused before validation.
	noCSRF := url.Values{"name": {"csrf-bot"}, "queues": {"reviews"}, "verbs": {"claim"}}
	if rec := c.do(http.MethodPost, "/endpoints/quick", noCSRF); rec.Code != http.StatusForbidden {
		t.Fatalf("quick vend without CSRF: got %d, want 403", rec.Code)
	}
	cards, err := st.ListEndpointCards(ctx, human.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 1 {
		t.Fatalf("a rejected submission minted an endpoint: %d cards", len(cards))
	}

	// The valid submission mints through the shared vend path and renders the reveal inline.
	good := url.Values{
		"csrf_token": {csrf}, "name": {"release-bot"},
		"queues": {"reviews"}, "queues_extra": {"ci, deploys"},
		"verbs":    {"list_todos", "claim"},
		"lifetime": {"7d"},
	}
	rec = c.do(http.MethodPost, "/endpoints/quick", good)
	if rec.Code != http.StatusOK {
		t.Fatalf("quick vend: got %d (body %.300s)", rec.Code, rec.Body.String())
	}
	body = rec.Body.String()
	for _, want := range []string{`data-sb-reveal`, "release-bot", `data-sb-reveal-wiring`, "sbk_", `data-sb-quickvend-next`} {
		if !strings.Contains(body, want) {
			t.Errorf("reveal: missing %q", want)
		}
	}
	if strings.Contains(body, `data-sb-quickvend-form`) {
		t.Error("the form rendered alongside the one-time reveal")
	}
	cards, err = st.ListEndpointCards(ctx, human.ID)
	if err != nil {
		t.Fatal(err)
	}
	var minted bool
	for _, card := range cards {
		if card.AgentName != "release-bot" {
			continue
		}
		minted = true
		if strings.Join(card.ScopeQueues, ",") != "reviews,ci,deploys" {
			t.Errorf("scope queues = %v, want chips + extras in order", card.ScopeQueues)
		}
		if strings.Join(card.ScopeVerbs, ",") != "list_todos,claim" {
			t.Errorf("scope verbs = %v", card.ScopeVerbs)
		}
		if card.ExpiresAt == nil || card.ExpiresAt.Before(time.Now().Add(6*24*time.Hour)) || card.ExpiresAt.After(time.Now().Add(8*24*time.Hour)) {
			t.Errorf("expires_at = %v, want ~7d out", card.ExpiresAt)
		}
	}
	if !minted {
		t.Fatalf("no endpoint minted for release-bot; cards = %+v", cards)
	}
	// The reveal is one-time: a fresh GET has no credential to show.
	if again := c.get("/endpoints/quick"); strings.Contains(again.Body.String(), "data-sb-reveal") {
		t.Error("a later GET re-rendered the reveal")
	}
}
