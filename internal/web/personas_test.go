package web

// Companion coverage for the SPEC-0013 Personas view (issue #108 → #107): template render tests for
// the persona cards (published/draft) and the create/edit modal (live agent-card URL preview + the
// verb-chip constraint), plus the capability gating (the /personas routes 404 and the rail entry
// hides while the personas capability is disabled). These run in the `go test ./...` gate without a
// database: render tests execute fragments from the embedded FS, and the gating tests return before
// any store read. The CSRF-guarded happy paths for the create/update/delete/publish POSTs are
// exercised end-to-end by the DB-backed internal/server suite (skipped without a test database).
//
// Governing: SPEC-0013 REQ "Personas View", REQ "Information Architecture and Navigation" (rail
// gating), REQ "Error Handling Standards".

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
		AgentID:      "agent-1",
		AgentName:    "review-bot",
		Discoverable: discoverable,
		SystemPrompt: "You are a careful code reviewer.",
		Verbs:        []string{"list_todos", "claim", "complete"},
		Queues:       []string{"reviews"},
		Skills:       deriveSkills([]string{"list_todos", "claim", "complete"}),
		AgentCardURL: agentCardURL("https://sb.example.com", "0a1b2c3d-0000-0000-0000-000000000001"),
		CSRF:         "tok",
	}
}

// TestPersonaCardPublishedState covers a discoverable persona: the "published" pill, the quoted
// system prompt, the derived skill chips, the advertised verb chips, the resolvable agent-card URL,
// and the Edit + Unpublish actions. Governing: SPEC-0013 REQ "Personas View".
func TestPersonaCardPublishedState(t *testing.T) {
	h := newTestHandler(t)
	out, err := h.renderFragment("persona_card", cardFixture(true))
	if err != nil {
		t.Fatalf("render persona_card: %v", err)
	}
	for _, want := range []string{
		"Reviewer",                         // persona name
		"review-bot",                       // backing agent
		">published<",                      // published pill
		"sb-badge--active",                 // published pill styling
		"You are a careful code reviewer.", // quoted system prompt
		"sb-persona__prompt",               // quoted/italic prompt styling hook
		"Advertised skills · derived from vended verbs", // skills section label (design canvas)
		"Process work", // a derived skill chip (list_todos+claim+complete)
		">claim<",      // an advertised verb chip
		"/a/0a1b2c3d-0000-0000-0000-000000000001/.well-known/agent-card.json",               // resolvable card URL
		"data-sb-open-modal=\"sb-persona-modal-edit-0a1b2c3d-0000-0000-0000-000000000001\"", // Edit
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
}

// TestPersonaCardDraftState covers a non-discoverable persona: the "draft" pill and the Publish
// action (posting discoverable=1). Governing: SPEC-0013 REQ "Personas View".
func TestPersonaCardDraftState(t *testing.T) {
	h := newTestHandler(t)
	out, err := h.renderFragment("persona_card", cardFixture(false))
	if err != nil {
		t.Fatalf("render persona_card: %v", err)
	}
	for _, want := range []string{">draft<", "sb-badge--revoked", "Publish"} {
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

// TestPersonaModalCreateURLPreviewAndConstraint covers the create modal: the agent-card URL preview
// is the id-based shape the well-known endpoint actually resolves (with the "{id}" placeholder — the
// id is assigned on create — and never a slug-based URL, which would never resolve), the backing-agent
// select carrying each agent's vended verbs, the stateful Published/Draft toggle, and the verb-chip
// CONSTRAINT — the selected agent's verbs are offered as selectable checkboxes and a verb the agent
// does not vend is not. Governing: SPEC-0013 REQ "Personas View" (scenario "Verb subset is
// constrained"), SPEC-0009 REQ "Well-Known Card Endpoint".
func TestPersonaModalCreateURLPreviewAndConstraint(t *testing.T) {
	h := newTestHandler(t)
	agents := []personaAgentOption{
		{ID: "agent-1", Name: "review-bot", Verbs: []string{"claim", "complete"}, Queues: []string{"reviews"}},
		{ID: "agent-2", Name: "deploy-bot", Verbs: []string{"create_for"}, Queues: []string{"deploys"}},
	}
	m := h.buildPersonaModal("csrf-tok", agents, nil)
	out, err := h.renderFragment("persona_modal", m)
	if err != nil {
		t.Fatalf("render persona_modal: %v", err)
	}
	for _, want := range []string{
		`action="/personas"`, // create posts to /personas
		"https://sb.example.com/a/{id}/.well-known/agent-card.json", // id-based URL shape the endpoint resolves
		"assigned on create",          // the placeholder is explained
		"data-sb-agent-select",        // agent select is JS-wired
		`data-verbs="claim,complete"`, // agent-1 vended verbs carried on its option
		`data-verbs="create_for"`,     // agent-2 vended verbs carried on its option
		`name="discoverable"`,         // the publish state posts as discoverable
		"sb-pubtoggle",                // stateful Published/Draft toggle, not a bare checkbox
		">Published<",                 // dynamic toggle label: on state
		">Draft<",                     // dynamic toggle label: off state
		"csrf-tok",                    // CSRF token embedded
	} {
		if !strings.Contains(out, want) {
			t.Errorf("create modal missing %q:\n%s", want, out)
		}
	}
	// The preview must never be the slug-based URL the audit flagged: the well-known endpoint
	// resolves only /a/{persona_id}/…, so a name/slug-derived preview would never resolve. (The old
	// create modal previewed the empty-name slug fallback "persona".)
	if strings.Contains(out, "/a/persona/") || strings.Contains(out, "data-sb-slug-base") {
		t.Errorf("create modal must not preview a slug-based agent-card URL:\n%s", out)
	}
	// The selected (first) agent constrains the rendered verb chips: claim/complete are selectable,
	// and create_for — which agent-1 does NOT vend — is not offered as a checkbox.
	if !strings.Contains(out, `<input type="checkbox" name="verbs" value="claim"`) {
		t.Errorf("create modal should offer the backing agent's vended verb 'claim':\n%s", out)
	}
	if strings.Contains(out, `name="verbs" value="create_for"`) {
		t.Errorf("create modal must NOT offer a verb the backing agent lacks (create_for):\n%s", out)
	}
	// A create modal has no Delete action (delete is edit-only).
	if strings.Contains(out, "/personas/") && strings.Contains(out, "/delete") {
		t.Errorf("create modal must not carry a delete action:\n%s", out)
	}
}

// TestPersonaModalCreateNoAgents covers the create modal when no agent has a vended grant: the "+ New
// persona" entry point stays visible on the page (see TestPersonasPageAlwaysShowsNewPersona), so the
// modal itself must explain the empty backing-agent list and refuse submission. Governing: SPEC-0013
// REQ "Personas View".
func TestPersonaModalCreateNoAgents(t *testing.T) {
	h := newTestHandler(t)
	m := h.buildPersonaModal("csrf-tok", nil, nil)
	out, err := h.renderFragment("persona_modal", m)
	if err != nil {
		t.Fatalf("render persona_modal: %v", err)
	}
	if !strings.Contains(out, "no agent has a vended grant yet") {
		t.Errorf("agentless create modal should explain the empty backing-agent list:\n%s", out)
	}
	if strings.Contains(out, "data-sb-agent-select") {
		t.Errorf("agentless create modal must not render an empty agent select:\n%s", out)
	}
	if !strings.Contains(out, "disabled>Create persona<") {
		t.Errorf("agentless create modal must disable submission:\n%s", out)
	}
}

// TestPersonaModalEditPinsAgentAndPrefills covers the edit modal: the backing agent is pinned
// (immutable, no select), the name/prompt/verbs prefill, the discoverable toggle reflects state, the
// preview is the real id-based agent-card URL (not a slug base), and Delete is available.
// Governing: SPEC-0013 REQ "Personas View".
func TestPersonaModalEditPinsAgentAndPrefills(t *testing.T) {
	h := newTestHandler(t)
	card := cardFixture(true)
	agents := []personaAgentOption{
		{ID: "agent-1", Name: "review-bot", Verbs: []string{"list_todos", "claim", "complete"}, Queues: []string{"reviews"}},
	}
	m := h.buildPersonaModal("csrf-tok", agents, &card)
	out, err := h.renderFragment("persona_modal", m)
	if err != nil {
		t.Fatalf("render persona_modal: %v", err)
	}
	for _, want := range []string{
		`action="/personas/0a1b2c3d-0000-0000-0000-000000000001"`, // edit posts to /personas/{id}
		`value="Reviewer"`,                                      // name prefilled
		"You are a careful code reviewer.",                      // prompt prefilled
		"the backing agent is immutable",                        // pinned agent note
		"/personas/0a1b2c3d-0000-0000-0000-000000000001/delete", // delete action
		"https://sb.example.com/a/0a1b2c3d-0000-0000-0000-000000000001/.well-known/agent-card.json", // the real resolvable card URL
		"sb-pubtoggle", // stateful Published/Draft toggle
	} {
		if !strings.Contains(out, want) {
			t.Errorf("edit modal missing %q:\n%s", want, out)
		}
	}
	// A pinned backing agent means no agent <select> in edit mode.
	if strings.Contains(out, "data-sb-agent-select") {
		t.Errorf("edit modal must pin the backing agent (no select):\n%s", out)
	}
	// The persona's current verbs are pre-checked.
	if !strings.Contains(out, `value="claim" checked`) {
		t.Errorf("edit modal should pre-check the persona's current verb 'claim':\n%s", out)
	}
	// A discoverable persona's publish toggle renders checked (the dynamic label shows Published).
	if !strings.Contains(out, `name="discoverable" value="1" checked`) {
		t.Errorf("edit modal for a published persona should pre-check the publish toggle:\n%s", out)
	}
	// Edit mode shows the real id-based URL, never the create-mode "{id}" placeholder.
	if strings.Contains(out, "{id}") {
		t.Errorf("edit modal must show the real card URL, not the create placeholder:\n%s", out)
	}
}

// TestPersonasCapabilityGating proves the SPEC-0013 hidden-not-broken contract at the handler: while
// the personas capability is disabled, GET /personas and every persona mutation 404 (before any store
// read), so a direct request to a gated view is indistinguishable from a missing route. With the
// capability enabled the gate opens (the store-backed body then runs, covered by the server suite).
func TestPersonasCapabilityGating(t *testing.T) {
	h := newTestHandler(t) // personasEnabled defaults to false

	cases := []struct {
		method, path string
		fn           http.HandlerFunc
	}{
		{http.MethodGet, "/personas", h.Personas},
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
// enabled (SPEC-0013 REQ "Information Architecture and Navigation"): the layout renders the Personas
// link with aria-current when active and enabled, and omits it entirely when disabled.
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

// TestPersonasPageRendersCardsAndModals covers the whole-page render: the cards grid, the "New
// persona" trigger, and the hidden create/edit modal templates the overlay opens. The active rail
// entry is marked. Governing: SPEC-0013 REQ "Personas View".
func TestPersonasPageRendersCardsAndModals(t *testing.T) {
	h := newTestHandler(t)
	agents := []personaAgentOption{
		{ID: "agent-1", Name: "review-bot", Verbs: []string{"claim", "complete"}, Queues: []string{"reviews"}},
	}
	card := cardFixture(true)
	pv := &personasView{
		Cards:     []personaCardView{card},
		Agents:    agents,
		CSRF:      "tok",
		NewModal:  h.buildPersonaModal("tok", agents, nil),
		EditModal: map[string]personaModalView{card.ID: h.buildPersonaModal("tok", agents, &card)},
	}
	body := renderPage(t, h, "personas", view{
		Title: "Personas", Human: testHuman(), CSRF: "tok",
		Shell: shell{Active: "personas", DBConnected: true, Initials: "JS", PersonasEnabled: true}, Personas: pv,
	})
	for _, want := range []string{
		"Personas · A2A Agent Cards", // header per the design canvas
		"one agent, many least-privilege faces · a human-authored prompt plus a verb subset · advertised as an A2A Agent Card", // tagline per the design canvas
		"+ New persona", // trigger label per the design canvas
		`data-sb-open-modal="sb-persona-modal-new"`,                                  // New persona trigger
		`<template id="sb-persona-modal-new">`,                                       // create modal template
		`<template id="sb-persona-modal-edit-0a1b2c3d-0000-0000-0000-000000000001">`, // edit modal template
		"Reviewer",            // the card
		`href="/personas"`,    // rail entry present
		`aria-current="page"`, // marked active
	} {
		if !strings.Contains(body, want) {
			t.Errorf("personas page missing %q", want)
		}
	}
}

// TestPersonasPageAlwaysShowsNewPersona proves the "+ New persona" entry point renders even when no
// agent has a vended grant (the design canvas always shows it): the trigger and the create-modal
// template are present, and the modal — not the page — explains the empty backing-agent list.
// Governing: SPEC-0013 REQ "Personas View".
func TestPersonasPageAlwaysShowsNewPersona(t *testing.T) {
	h := newTestHandler(t)
	pv := &personasView{
		CSRF:      "tok",
		NewModal:  h.buildPersonaModal("tok", nil, nil),
		EditModal: map[string]personaModalView{},
	}
	body := renderPage(t, h, "personas", view{
		Title: "Personas", Human: testHuman(), CSRF: "tok",
		Shell: shell{Active: "personas", DBConnected: true, Initials: "JS", PersonasEnabled: true}, Personas: pv,
	})
	for _, want := range []string{
		"+ New persona", // the trigger is unconditional
		`data-sb-open-modal="sb-persona-modal-new"`,
		`<template id="sb-persona-modal-new">`,
		"no agent has a vended grant yet", // the modal explains the empty agent list
		"no personas yet",                 // empty state
	} {
		if !strings.Contains(body, want) {
			t.Errorf("agentless personas page missing %q", want)
		}
	}
}
