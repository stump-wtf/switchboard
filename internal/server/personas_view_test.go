// Server-level coverage for the SPEC-0013 Personas view routes: the CSRF guard on every persona
// mutation and the end-to-end HTMX create/update/publish/delete flow against the real router (the
// same newRouter Run uses) with a live session. These are DB-backed, so they run against Postgres on
// the GitHub mirror and skip without SWITCHBOARD_TEST_DATABASE_URL — the CI-gate coverage of the
// cards/modal render and the capability gating lives in internal/web (no database).
//
// Governing: SPEC-0013 REQ "Personas View", REQ "Error Handling Standards"; SPEC-0008 CSRF;
// SPEC-0009 REQ "Persona Record", REQ "Discoverability Is Owner-Controlled".
package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// TestPersonaMutationsRequireCSRF proves each persona POST rejects a missing or forged CSRF token
// with 403 — before any store write — so a cross-site form can never create, edit, publish, or delete
// a persona. A valid session is present; only the CSRF token is bad. Governing: SPEC-0008 CSRF.
func TestPersonaMutationsRequireCSRF(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	_, token := mintSession(t, st, ctx, "persona-csrf-op", "Op", "persona-csrf@example.com")

	paths := []string{"/personas", "/personas/p_any", "/personas/p_any/delete"}
	for _, path := range paths {
		if rec := postNoCSRF(t, r, token, path); rec.Code != http.StatusForbidden {
			t.Errorf("POST %s without CSRF: got %d, want 403", path, rec.Code)
		}
		if rec := postForgedCSRF(t, r, token, path); rec.Code != http.StatusForbidden {
			t.Errorf("POST %s with forged CSRF: got %d, want 403", path, rec.Code)
		}
	}
}

// postFormAs issues a session-authenticated HTMX POST with a urlencoded form body and a valid CSRF
// token in both the form field and the header (a browser sends the field; HTMX the header).
func postFormAs(t *testing.T, r chi.Router, token, csrf, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	form.Set("csrf_token", csrf)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrf)
	req.Header.Set("HX-Request", "true")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// TestPersonasCreatePublishDeleteFlow drives the HTMX create → publish → unpublish → delete lifecycle
// end-to-end through the router with a live session, proving the operator surface wires to the store
// transitions: a created persona appears on the view as a draft, publishing makes its well-known card
// resolve immediately, unpublishing 404s it again, and delete removes it. Governing: SPEC-0013 REQ
// "Personas View", SPEC-0009 REQ "Discoverability Is Owner-Controlled".
func TestPersonasCreatePublishDeleteFlow(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	human, token := mintSession(t, st, ctx, "persona-flow-op", "Flow Op", "persona-flow@example.com")

	// A backing agent with a vended endpoint — a persona is a scoped face of a real grant.
	ag, err := st.CreateAgent(ctx, human.ID, "review-bot", "")
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if _, err := st.CreateEndpoint(ctx, ag.ID, "hash-1", "sbk_ab12cd", "review-bot-ab12cd",
		[]string{"reviews"}, []string{"list_todos", "claim", "complete"}); err != nil {
		t.Fatalf("create endpoint: %v", err)
	}

	// Scrape the per-session CSRF token from the rendered Personas page.
	page := getAs(t, r, token, "/personas")
	if page.Code != http.StatusOK {
		t.Fatalf("GET /personas: got %d, want 200", page.Code)
	}
	if !strings.Contains(page.Body.String(), "review-bot") {
		t.Fatalf("personas page should offer the backing agent as an option:\n%.400s", page.Body.String())
	}
	csrf := scrapeCSRF(t, page.Body.String())

	// Create a persona (draft: no discoverable flag).
	create := postFormAs(t, r, token, csrf, "/personas", url.Values{
		"agent_id":      {ag.ID},
		"name":          {"Reviewer"},
		"system_prompt": {"You are a careful code reviewer."},
		"verbs":         {"list_todos", "claim", "complete"},
		"queues":        {"reviews"},
	})
	if create.Code != http.StatusNoContent || create.Header().Get("HX-Redirect") != "/personas" {
		t.Fatalf("create persona: got %d (HX-Redirect %q), want 204 → /personas", create.Code, create.Header().Get("HX-Redirect"))
	}

	personas, err := st.ListPersonas(ctx, human.ID)
	if err != nil || len(personas) != 1 {
		t.Fatalf("expected exactly one persona after create, got %d (err %v)", len(personas), err)
	}
	p := personas[0]
	if p.Discoverable {
		t.Fatalf("a freshly created persona must be a draft (not discoverable)")
	}

	// Its well-known card must 404 while it is a draft.
	if rec := anonRequest(t, r, http.MethodGet, "/a/"+p.ID+"/.well-known/agent-card.json"); rec.Code != http.StatusNotFound {
		t.Fatalf("draft persona card: got %d, want 404", rec.Code)
	}

	// Publish it (the card-level toggle: discoverable-only flip).
	pub := postFormAs(t, r, token, csrf, "/personas/"+p.ID, url.Values{
		"toggle_discoverable": {"1"},
		"discoverable":        {"1"},
	})
	if pub.Code != http.StatusNoContent {
		t.Fatalf("publish persona: got %d, want 204", pub.Code)
	}
	// The well-known card now resolves immediately (publish took effect on the endpoint).
	if rec := anonRequest(t, r, http.MethodGet, "/a/"+p.ID+"/.well-known/agent-card.json"); rec.Code != http.StatusOK {
		t.Fatalf("published persona card: got %d, want 200", rec.Code)
	}

	// Unpublish it (draft again) → the card 404s once more.
	unpub := postFormAs(t, r, token, csrf, "/personas/"+p.ID, url.Values{
		"toggle_discoverable": {"1"},
		"discoverable":        {"0"},
	})
	if unpub.Code != http.StatusNoContent {
		t.Fatalf("unpublish persona: got %d, want 204", unpub.Code)
	}
	if rec := anonRequest(t, r, http.MethodGet, "/a/"+p.ID+"/.well-known/agent-card.json"); rec.Code != http.StatusNotFound {
		t.Fatalf("unpublished persona card: got %d, want 404", rec.Code)
	}

	// Delete it.
	del := postFormAs(t, r, token, csrf, "/personas/"+p.ID+"/delete", url.Values{})
	if del.Code != http.StatusNoContent {
		t.Fatalf("delete persona: got %d, want 204", del.Code)
	}
	if remaining, _ := st.ListPersonas(ctx, human.ID); len(remaining) != 0 {
		t.Fatalf("persona should be deleted, %d remain", len(remaining))
	}
}

// TestPersonaVerbSubsetConstraintEnforced proves the server rejects a persona whose verb_subset
// exceeds the backing agent's vended grant with a 400 (the store's ErrScopeExceeded, mapped generic):
// the modal constrains the chips, so an out-of-grant verb is a forged request and never persists.
// Governing: SPEC-0013 REQ "Personas View" (scenario "Verb subset is constrained"), SPEC-0009.
func TestPersonaVerbSubsetConstraintEnforced(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	human, token := mintSession(t, st, ctx, "persona-scope-op", "Scope Op", "persona-scope@example.com")

	ag, err := st.CreateAgent(ctx, human.ID, "scoped-bot", "")
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	// Vended grant is only {claim}; a persona asking for create_for exceeds it.
	if _, err := st.CreateEndpoint(ctx, ag.ID, "hash-2", "sbk_scoped", "scoped-bot-2",
		[]string{"reviews"}, []string{"claim"}); err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
	csrf := scrapeCSRF(t, getAs(t, r, token, "/personas").Body.String())

	rec := postFormAs(t, r, token, csrf, "/personas", url.Values{
		"agent_id":      {ag.ID},
		"name":          {"Overreach"},
		"system_prompt": {"x"},
		"verbs":         {"claim", "create_for"}, // create_for is NOT vended → ErrScopeExceeded
		"queues":        {"reviews"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("out-of-grant persona: got %d, want 400", rec.Code)
	}
	if personas, _ := st.ListPersonas(ctx, human.ID); len(personas) != 0 {
		t.Fatalf("an out-of-grant persona must not persist, %d found", len(personas))
	}
}
