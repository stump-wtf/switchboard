// End-to-end tests for the public A2A Agent Card endpoint through the real router + PostgreSQL:
// a discoverable persona serves a schema-shaped card with derived skills, security headers, and no
// owner PII; a non-discoverable persona, an unknown id, and a malformed id all 404 without leaking
// existence; and a state-changing method is rejected (read-only). Skipped without
// SWITCHBOARD_TEST_DATABASE_URL, like the other DB-backed server tests.
// Governing: SPEC-0009 REQ "Well-Known Card Endpoint", REQ "Discoverability Is Owner-Controlled",
// REQ "Agent Card Mapping"; SPEC-0009 "Security Requirements".
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stump-wtf/switchboard/internal/store"
)

// cardPath builds the well-known card path for a persona id.
func cardPath(id string) string { return "/a/" + id + "/.well-known/agent-card.json" }

// TestAgentCardDiscoverableServesCard: a persona its owner marked discoverable serves a schema-shaped
// A2A Agent Card at its well-known path — 200, application/json, default-src 'none' CSP, derived
// skills, owner display name as provider — and never leaks owner PII (email) or its capability slice.
func TestAgentCardDiscoverableServesCard(t *testing.T) {
	r, st, ctx := newDBRouter(t)

	h, err := st.UpsertHuman(ctx, "test|owner", "Ada Lovelace", "ada-secret@example.com")
	if err != nil {
		t.Fatalf("upsert human: %v", err)
	}
	ag, err := st.CreateAgent(ctx, h.ID, "reviewer-bot", "")
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	slug, err := store.MintSlug("reviewer-bot")
	if err != nil {
		t.Fatalf("mint slug: %v", err)
	}
	if _, err := st.CreateEndpoint(ctx, ag.ID, "card-grant-1", "sbk_test12", slug,
		[]string{"reviews"}, []string{"list_todos", "claim", "complete"}); err != nil {
		t.Fatalf("vend endpoint: %v", err)
	}
	p, err := st.CreatePersona(ctx, store.CreatePersonaParams{
		OwnerHumanID: h.ID, AgentID: ag.ID, Name: "Reviewer",
		Description: "Reviews pull requests.",
		VerbSubset:  []string{"list_todos", "claim", "complete"}, Queues: []string{"reviews"},
	})
	if err != nil {
		t.Fatalf("create persona: %v", err)
	}
	if _, err := st.SetPersonaDiscoverable(ctx, p.ID, h.ID, true); err != nil {
		t.Fatalf("set discoverable: %v", err)
	}

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cardPath(p.ID), nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET card: got %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); csp != "default-src 'none'" {
		t.Errorf("CSP = %q, want default-src 'none' for the inert JSON card", csp)
	}

	var card struct {
		ProtocolVersion string `json:"protocolVersion"`
		Name            string `json:"name"`
		URL             string `json:"url"`
		Provider        struct {
			Organization string `json:"organization"`
		} `json:"provider"`
		Skills []struct {
			ID string `json:"id"`
		} `json:"skills"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &card); err != nil {
		t.Fatalf("card is not valid JSON: %v (%s)", err, rec.Body.String())
	}
	if card.Name != "Reviewer" {
		t.Errorf("card.name = %q, want Reviewer", card.Name)
	}
	if card.Provider.Organization != "Ada Lovelace" {
		t.Errorf("card.provider.organization = %q, want the owner display name", card.Provider.Organization)
	}
	if !strings.HasSuffix(card.URL, "/a/"+p.ID+"/") {
		t.Errorf("card.url = %q, want the persona discovery base path", card.URL)
	}
	var haveProcess bool
	for _, s := range card.Skills {
		if s.ID == "process-work" {
			haveProcess = true
		}
	}
	if !haveProcess {
		t.Errorf("card must advertise the derived process-work skill, got %+v", card.Skills)
	}
	// Owner PII (email) must never appear in the public card.
	if strings.Contains(rec.Body.String(), "ada-secret@example.com") {
		t.Errorf("card leaked owner email: %s", rec.Body.String())
	}
}

// TestAgentCardHidesNonDiscoverableAndUnknown: a persona that exists but is NOT discoverable, an
// unknown (well-formed) id, and a malformed id all return an identical 404, so the endpoint never
// leaks the existence of an unpublished persona.
func TestAgentCardHidesNonDiscoverableAndUnknown(t *testing.T) {
	r, st, ctx := newDBRouter(t)

	h, err := st.UpsertHuman(ctx, "test|owner2", "Owner Two", "owner2@example.com")
	if err != nil {
		t.Fatalf("upsert human: %v", err)
	}
	ag, err := st.CreateAgent(ctx, h.ID, "hidden-bot", "")
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	slug, err := store.MintSlug("hidden-bot")
	if err != nil {
		t.Fatalf("mint slug: %v", err)
	}
	if _, err := st.CreateEndpoint(ctx, ag.ID, "card-grant-2", "sbk_test12", slug,
		[]string{"reviews"}, []string{"list_todos", "claim", "complete"}); err != nil {
		t.Fatalf("vend endpoint: %v", err)
	}
	// Created but left non-discoverable (default false).
	p, err := st.CreatePersona(ctx, store.CreatePersonaParams{
		OwnerHumanID: h.ID, AgentID: ag.ID, Name: "Hidden", SystemPrompt: "secret",
		VerbSubset: []string{"list_todos", "claim", "complete"}, Queues: []string{"reviews"},
	})
	if err != nil {
		t.Fatalf("create persona: %v", err)
	}

	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}

	nonDiscoverable := get(cardPath(p.ID))
	unknown := get(cardPath("00000000-0000-0000-0000-000000000000"))
	malformed := get(cardPath("not-a-uuid"))

	if nonDiscoverable.Code != http.StatusNotFound {
		t.Fatalf("non-discoverable persona: got %d, want 404", nonDiscoverable.Code)
	}
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown id: got %d, want 404", unknown.Code)
	}
	if malformed.Code != http.StatusNotFound {
		t.Fatalf("malformed id: got %d, want 404 (not a 500)", malformed.Code)
	}
	// Existence must not leak: the non-discoverable 404 is byte-identical to the unknown-id 404.
	if nonDiscoverable.Body.String() != unknown.Body.String() {
		t.Errorf("non-discoverable 404 body %q differs from unknown 404 %q — existence leak",
			nonDiscoverable.Body.String(), unknown.Body.String())
	}
	if strings.Contains(nonDiscoverable.Body.String(), "Hidden") {
		t.Errorf("non-discoverable 404 leaked persona name: %q", nonDiscoverable.Body.String())
	}
}

// TestAgentCardIsReadOnly: a state-changing method against the card path is rejected (405), since the
// route is registered GET-only. Governing: SPEC-0009 REQ "Well-Known Card Endpoint" (read-only).
func TestAgentCardIsReadOnly(t *testing.T) {
	r := newTestRouterA2A(t) // no DB needed: the router rejects the method before any handler runs
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(m, cardPath("00000000-0000-0000-0000-000000000000"), nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s card path: got %d, want 405 (read-only endpoint)", m, rec.Code)
		}
	}
}
