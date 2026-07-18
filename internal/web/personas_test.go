package web

// Companion coverage for the SPEC-0015 Personas view (issue #28): render tests for the persona
// cards (initials block, published/draft state, prompt, verb subset, derived skills, agent-card
// URL, vended-as usage) and the whole page (wizard entry point, no modal), plus the capability
// gating (the /personas routes 404 and the rail entry hides while the personas capability is
// disabled). These run in the `go test ./...` gate without a database: render tests execute
// fragments from the embedded FS, and the gating tests return before any store read. Assertions
// key on data-sb-* hooks and ids, never on style classes. The CSRF-guarded happy paths for the
// create/update/delete/publish POSTs are exercised end-to-end by the DB-backed internal/server
// suite (skipped without a test database).
//
// Governing: SPEC-0015 REQ "Personas View And Wizard", REQ "Application Shell And Navigation"
// (rail gating); ADR-0018.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// cardFixture builds a persona card render model in the given publish state.
func cardFixture(discoverable bool) personaCardView {
	return personaCardView{
		ID:           "0a1b2c3d-0000-0000-0000-000000000001",
		Name:         "Reviewer",
		Slug:         "reviewer",
		Initials:     personaInitials("Reviewer"),
		AgentID:      "agent-1",
		AgentName:    "review-bot",
		Discoverable: discoverable,
		SystemPrompt: "You are a careful code reviewer.",
		Verbs:        []string{"list_todos", "claim", "complete"},
		Queues:       []string{"reviews"},
		Skills:       deriveSkills([]string{"list_todos", "claim", "complete"}),
		AgentCardURL: agentCardURL("https://sb.example.com", "0a1b2c3d-0000-0000-0000-000000000001"),
		VendedAs:     []string{"review-bot"},
		CSRF:         "tok",
	}
}

// TestPersonaCardPublishedState covers a discoverable persona: the initials block, the "published"
// pill, the quoted system prompt, the derived skill chips, the advertised verb chips, the
// resolvable agent-card URL, the vended-as usage row, and the Edit (→ wizard) + Unpublish actions.
// Governing: SPEC-0015 REQ "Personas View And Wizard".
func TestPersonaCardPublishedState(t *testing.T) {
	h := newTestHandler(t)
	out, err := h.renderFragment("persona_card", cardFixture(true))
	if err != nil {
		t.Fatalf("render persona_card: %v", err)
	}
	for _, want := range []string{
		`id="sb-persona-0a1b2c3d-0000-0000-0000-000000000001"`,
		`data-sb-persona-card="0a1b2c3d-0000-0000-0000-000000000001"`,
		">RE<",                             // initials block derived from the persona name
		"Reviewer",                         // persona name
		"review-bot",                       // backing agent
		">published<",                      // published pill
		"You are a careful code reviewer.", // quoted system prompt
		"Advertised skills · derived from vended verbs", // skills section label (design canvas)
		"Process work",      // a derived skill chip (list_todos+claim+complete)
		">claim<",           // an advertised verb chip
		"data-sb-vended-as", // vended-as usage row
		">Vended as<",       // its microlabel
		"/a/0a1b2c3d-0000-0000-0000-000000000001/.well-known/agent-card.json", // resolvable card URL
		`href="/personas/0a1b2c3d-0000-0000-0000-000000000001/edit"`,          // Edit → the edit wizard page
		"data-sb-persona-edit",
		"Unpublish",                  // a published persona offers Unpublish
		`name="toggle_discoverable"`, // publish toggle is a discoverable-only flip
	} {
		if !strings.Contains(out, want) {
			t.Errorf("published card missing %q:\n%s", want, out)
		}
	}
	// Unpublishing sets discoverable=0 (the opposite of the current state).
	if !strings.Contains(out, `name="discoverable" value="0"`) {
		t.Errorf("published card's toggle should post discoverable=0:\n%s", out)
	}
	// The modal is retired: no overlay trigger renders on a card.
	if strings.Contains(out, "data-sb-open-modal") {
		t.Errorf("persona card must link to the edit wizard, not open a modal:\n%s", out)
	}
}

// TestPersonaCardDraftState covers a non-discoverable persona: the "draft" pill and the Publish
// action (posting discoverable=1), plus the honest empty vended-as state.
// Governing: SPEC-0015 REQ "Personas View And Wizard".
func TestPersonaCardDraftState(t *testing.T) {
	h := newTestHandler(t)
	card := cardFixture(false)
	card.VendedAs = nil
	out, err := h.renderFragment("persona_card", card)
	if err != nil {
		t.Fatalf("render persona_card: %v", err)
	}
	for _, want := range []string{">draft<", "Publish", "no active endpoint vends this persona"} {
		if !strings.Contains(out, want) {
			t.Errorf("draft card missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, `name="discoverable" value="1"`) {
		t.Errorf("draft card's toggle should post discoverable=1:\n%s", out)
	}
	if strings.Contains(out, "Unpublish") {
		t.Errorf("draft card must offer Publish, not Unpublish:\n%s", out)
	}
}

// TestPersonaInitials pins the initials derivation the card's avatar tile renders: first two
// letters/digits upper-cased, punctuation skipped, "PA" fallback for unusable names.
func TestPersonaInitials(t *testing.T) {
	cases := map[string]string{
		"Reviewer":   "RE",
		"deploy-bot": "DE",
		"-r2":        "R2",
		"":           "PA",
		"⚙️":         "PA",
	}
	for name, want := range cases {
		if got := personaInitials(name); got != want {
			t.Errorf("personaInitials(%q) = %q, want %q", name, got, want)
		}
	}
}

// TestPersonasCapabilityGating proves the hidden-not-broken contract at the handler: while the
// personas capability is disabled, GET /personas, every persona mutation, AND every wizard route
// (incl. the live preview endpoint) 404 — before any store read — so a direct request to a gated
// view is indistinguishable from a missing route. With the capability enabled the gate opens (the
// store-backed body then runs, covered by the server suite). Governing: SPEC-0015 REQ "Personas
// View And Wizard" (capability gating carries over).
func TestPersonasCapabilityGating(t *testing.T) {
	h := newTestHandler(t) // personasEnabled defaults to false

	cases := []struct {
		method, path string
		fn           http.HandlerFunc
	}{
		{http.MethodGet, "/personas", h.Personas},
		{http.MethodGet, "/personas/wizard", h.PersonaWizardStart},
		{http.MethodGet, "/personas/wizard/identity", h.PersonaWizardStep},
		{http.MethodPost, "/personas/wizard/identity", h.PersonaWizardStepSubmit},
		{http.MethodPost, "/personas/wizard/preview", h.PersonaCardPreview},
		{http.MethodGet, "/personas/x/edit", h.PersonaWizardEdit},
		{http.MethodPost, "/personas", h.CreatePersona},
		{http.MethodPost, "/personas/x", h.UpdatePersona},
		{http.MethodPost, "/personas/x/delete", h.DeletePersona},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		c.fn(rec, httptest.NewRequest(c.method, c.path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s while disabled: got %d, want 404", c.method, c.path, rec.Code)
		}
	}
}

// TestPersonasRailGating proves the rail entry is present only when the personas capability is
// enabled (SPEC-0015 REQ "Application Shell And Navigation"): the layout renders the Personas link
// with aria-current when active and enabled, and omits it entirely when disabled.
func TestPersonasRailGating(t *testing.T) {
	h := newTestHandler(t)

	enabled := renderPage(t, h, "board", view{
		Title: "The Board", Human: testHuman(), CSRF: "tok",
		Shell: shell{Active: "board", DBConnected: true, Initials: "JS", PersonasEnabled: true},
	})
	if !strings.Contains(enabled, `href="/personas"`) {
		t.Errorf("enabled rail should link to /personas:\n%s", enabled)
	}

	disabled := renderPage(t, h, "board", view{
		Title: "The Board", Human: testHuman(), CSRF: "tok",
		Shell: shell{Active: "board", DBConnected: true, Initials: "JS", PersonasEnabled: false},
	})
	if strings.Contains(disabled, `href="/personas"`) {
		t.Errorf("disabled rail must not link to /personas:\n%s", disabled)
	}
}

// TestPersonasPageRendersCardsAndWizardEntry covers the whole-page render: the cards grid and the
// "+ New persona" entry point, which is now a plain link into the full-page create wizard (the
// SPEC-0013 modal templates are gone). The active rail entry is marked. Governing: SPEC-0015 REQ
// "Personas View And Wizard", REQ "Wizard Interaction Pattern".
func TestPersonasPageRendersCardsAndWizardEntry(t *testing.T) {
	h := newTestHandler(t)
	card := cardFixture(true)
	pv := &personasView{Cards: []personaCardView{card}, CSRF: "tok"}
	body := renderPage(t, h, "personas", view{
		Title: "Personas", Human: testHuman(), CSRF: "tok",
		Shell: shell{Active: "personas", DBConnected: true, Initials: "JS", PersonasEnabled: true}, Personas: pv,
	})
	for _, want := range []string{
		"Personas · A2A Agent Cards", // header per the design canvas
		"one agent, many least-privilege faces · a human-authored prompt plus a verb subset · advertised as an A2A Agent Card", // tagline per the design canvas
		"+ New persona",           // entry point label per the design canvas
		`href="/personas/wizard"`, // …which is a full-page wizard, not a modal
		"data-sb-persona-new",     // its behavior hook
		"Reviewer",                // the card
		`href="/personas"`,        // rail entry present
		`aria-current="page"`,     // marked active
	} {
		if !strings.Contains(body, want) {
			t.Errorf("personas page missing %q", want)
		}
	}
	// The modal machinery is retired wholesale on this page.
	for _, gone := range []string{"<template", "data-sb-open-modal", "data-sb-agent-select"} {
		if strings.Contains(body, gone) {
			t.Errorf("personas page must not carry modal machinery %q", gone)
		}
	}
}

// TestPersonasPageEmptyState proves the "+ New persona" entry point renders even with no personas
// (the design canvas always shows it) alongside the empty-state line; the wizard's identity step —
// not the page — explains an empty backing-agent list. Governing: SPEC-0015 REQ "Personas View And
// Wizard".
func TestPersonasPageEmptyState(t *testing.T) {
	h := newTestHandler(t)
	body := renderPage(t, h, "personas", view{
		Title: "Personas", Human: testHuman(), CSRF: "tok",
		Shell:    shell{Active: "personas", DBConnected: true, Initials: "JS", PersonasEnabled: true},
		Personas: &personasView{CSRF: "tok"},
	})
	for _, want := range []string{
		"+ New persona", // the entry point is unconditional
		`href="/personas/wizard"`,
		"no personas yet", // empty state
	} {
		if !strings.Contains(body, want) {
			t.Errorf("empty personas page missing %q", want)
		}
	}
}
