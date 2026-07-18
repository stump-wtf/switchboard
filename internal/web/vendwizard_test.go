package web

// DOM render contract for the vend wizard's step pages (templates/vend.html): each routed step is
// a full page with plain method=post forms (no-JS completion), a value-preserving Back link, a
// step tracker, and the confirm step's irreversibility warning. Assertions key on data-sb-* hooks
// and ids, never on style classes. The state-machine behavior over HTTP (redirects, seeding,
// server-side value preservation) is bound end-to-end in internal/server/vend_wizard_test.go.
// Governing: SPEC-0015 REQ "Endpoints View And Vend Wizard", REQ "Wizard Interaction Pattern".

import (
	"strings"
	"testing"
)

// testVendStepView builds a render-ready step view the way renderVendStep does (tracker tabs,
// back/action URLs), for template tests that bypass the handler.
func testVendStepView(step string) *vendStepView {
	idx := vendWizard.index(step)
	v := &vendStepView{
		Step: step, StepNum: idx + 1, StepTotal: len(vendWizard.steps),
		ActionURL: vendWizard.stepPath(step),
	}
	if prev := vendWizard.prev(step); prev != "" {
		v.BackURL = vendWizard.stepPath(prev)
	}
	for i, s := range vendWizard.steps {
		v.Steps = append(v.Steps, vendStepTab{Slug: s, Num: i + 1, Current: i == idx, Done: i < idx})
	}
	return v
}

func renderVendPage(t *testing.T, h *Handler, v *vendStepView, personasEnabled bool) string {
	t.Helper()
	return renderPage(t, h, "vend", view{
		Title: "Vend endpoint", Human: testHuman(), CSRF: "tok",
		Shell: shell{Active: "endpoints", DBConnected: true, Initials: "JS"},
		PersonasEnabled: personasEnabled, Vend: v,
	})
}

// TestVendWizardStepOrder pins the SPEC-0015 step contract: persona → queues → verbs → lifetime →
// confirm, under /endpoints/vend.
func TestVendWizardStepOrder(t *testing.T) {
	want := []string{"persona", "queues", "verbs", "lifetime", "confirm"}
	if len(vendWizard.steps) != len(want) {
		t.Fatalf("vend wizard has %d steps, want %d", len(vendWizard.steps), len(want))
	}
	for i, s := range want {
		if vendWizard.steps[i] != s {
			t.Errorf("step %d = %q, want %q", i, vendWizard.steps[i], s)
		}
	}
	if vendWizard.base != "/endpoints/vend" {
		t.Errorf("vend wizard base = %q", vendWizard.base)
	}
}

// TestVendStepPersonaRendersNameAndPersonaSelect: step 1 collects the agent name (prefilled from
// the draft — value-preserving back nav) and, with personas enabled, the persona select with the
// draft's choice selected. With personas disabled no persona field renders.
func TestVendStepPersonaRendersNameAndPersonaSelect(t *testing.T) {
	h := newTestHandler(t)
	v := testVendStepView("persona")
	v.Name = "release-bot"
	v.PersonaID = "pr_123"
	v.PersonaOptions = []vendPersonaOption{{ID: "pr_123", Name: "Reviewer"}, {ID: "pr_456", Name: "Deployer"}}
	body := renderVendPage(t, h, v, true)
	for _, want := range []string{
		`data-sb-wizard="vend"`,           // the wizard page hook
		`aria-current="step"`,             // tracker marks the current step
		`data-sb-wiz-step="persona"`,      // tracker entries carry their slugs
		"step 1 of 5",                     // progress line
		`method="post"`,                   // plain form — no-JS completion
		`action="/endpoints/vend/persona"`,
		`name="csrf_token" value="tok"`,
		`name="name"`, `value="release-bot"`, `data-sb-vend-name`, // prefilled name
		`<select id="sb-vend-persona"`, `name="persona"`,
		`<option value="">`,                                 // the agent-level (no persona) choice
		`<option value="pr_123" selected>Reviewer</option>`, // draft choice re-selected
		`href="/endpoints" data-sb-wiz-cancel`,              // cancel escapes to the view
		`data-sb-vend-submit`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("persona step: missing %q", want)
		}
	}
	// First step: no Back link.
	if strings.Contains(body, "data-sb-wiz-back") {
		t.Error("persona step must not render a Back link (it is the first step)")
	}
	// Personas off: no persona slot at all.
	off := renderVendPage(t, h, testVendStepView("persona"), false)
	if strings.Contains(off, `name="persona"`) {
		t.Error("persona step must not render a persona field while personas are disabled")
	}
}

// TestVendStepQueuesRendersChipsAndFreeText: step 2 offers the known queues as toggle chips with
// the draft's choices checked, ALWAYS renders the free-text add-queues field (a first vend on an
// empty store is never blocked), and echoes draft queues the store does not know into that field.
func TestVendStepQueuesRendersChipsAndFreeText(t *testing.T) {
	h := newTestHandler(t)
	v := testVendStepView("queues")
	v.QueueOptions = []vendChipOption{{Name: "reviews", Checked: true}, {Name: "deploys"}}
	v.ExtraQueues = "hotfixes"
	body := renderVendPage(t, h, v, false)
	for _, want := range []string{
		`action="/endpoints/vend/queues"`,
		`data-sb-vend-queue-chips`,
		`name="queues" value="reviews" checked`, // draft-checked chip survives back nav
		`name="queues" value="deploys"`,
		`name="queues_extra"`, `value="hotfixes"`, `data-sb-vend-queues`, // free-text add field, prefilled
		`data-sb-vend-queue-preview`,                 // sb-vend.js mirrors typed queues as chips
		`href="/endpoints/vend/persona" data-sb-wiz-back`, // Back to the previous step page
	} {
		if !strings.Contains(body, want) {
			t.Errorf("queues step: missing %q", want)
		}
	}
	// No known queues: the free-text field still renders (and no chip group does).
	bare := renderVendPage(t, h, testVendStepView("queues"), false)
	if !strings.Contains(bare, `name="queues_extra"`) {
		t.Error("queues step without known queues must render the free-text field")
	}
	if strings.Contains(bare, "data-sb-vend-queue-chips") {
		t.Error("queues step without known queues must not render an empty chip group")
	}
}

// TestVendStepVerbsRendersChips: step 3 renders the agent-tools verb chips; fresh drafts start
// with the drain verbs pre-checked, and a revisit re-renders the operator's own selection instead.
func TestVendStepVerbsRendersChips(t *testing.T) {
	h := newTestHandler(t)
	v := testVendStepView("verbs")
	v.VerbOptions = vendVerbOptions()
	body := renderVendPage(t, h, v, false)
	for _, want := range []string{
		`action="/endpoints/vend/verbs"`,
		`data-sb-vend-verbs`,
		`name="verbs" value="list_todos" checked`, // drain verbs pre-checked by default
		`name="verbs" value="claim" checked`,
		`name="verbs" value="create_webhook"`, // wider surface offered…
		`href="/endpoints/vend/queues" data-sb-wiz-back`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("verbs step: missing %q", want)
		}
	}
	// …but never pre-checked: wider grants are a deliberate toggle.
	for _, verb := range []string{"create_webhook", "list_webhook_events", "replay_webhook_event"} {
		if strings.Contains(body, `value="`+verb+`" checked`) {
			t.Errorf("verbs step: %s must not be pre-checked", verb)
		}
	}
}

// TestVendStepLifetimeRendersPresets: step 4 offers "until revoked" (default), the preset
// lifetimes, and a custom duration field; a custom draft value re-checks custom and prefills the
// field (value-preserving back nav).
func TestVendStepLifetimeRendersPresets(t *testing.T) {
	h := newTestHandler(t)
	v := testVendStepView("lifetime")
	v.LifetimePresets = lifetimePresets
	body := renderVendPage(t, h, v, false)
	for _, want := range []string{
		`action="/endpoints/vend/lifetime"`,
		`data-sb-vend-lifetime`,
		`name="lifetime" value="" checked`, // until revoked is the default
		`name="lifetime" value="1h"`, `name="lifetime" value="24h"`,
		`name="lifetime" value="7d"`, `name="lifetime" value="30d"`,
		`name="lifetime" value="custom"`,
		`name="lifetime_custom"`, `data-sb-vend-lifetime-custom`,
		`href="/endpoints/vend/verbs" data-sb-wiz-back`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("lifetime step: missing %q", want)
		}
	}
	// A saved custom value re-renders checked + prefilled.
	v.LifetimePreset = "custom"
	v.LifetimeCustom = "36h"
	custom := renderVendPage(t, h, v, false)
	for _, want := range []string{`name="lifetime" value="custom" checked`, `value="36h"`} {
		if !strings.Contains(custom, want) {
			t.Errorf("lifetime step (custom draft): missing %q", want)
		}
	}
	// A preset draft value re-checks its radio.
	v.LifetimePreset = "7d"
	v.LifetimeCustom = ""
	preset := renderVendPage(t, h, v, false)
	if !strings.Contains(preset, `name="lifetime" value="7d" checked`) {
		t.Error("lifetime step (7d draft): preset radio not re-checked")
	}
}

// TestVendStepConfirmSummarizesAndWarns: the confirm step — the irreversible one — summarizes the
// full draft from SERVER-SIDE state (the form posts only the CSRF token), states the consequence
// (credential shown once, scope immutable), and submits to the confirm step itself.
func TestVendStepConfirmSummarizesAndWarns(t *testing.T) {
	h := newTestHandler(t)
	v := testVendStepView("confirm")
	v.Name = "release-bot"
	v.PersonaName = "Reviewer"
	v.Queues = []string{"reviews", "deploys"}
	v.Verbs = []string{"claim", "complete"}
	v.LifetimeLabel = "7d"
	body := renderVendPage(t, h, v, true)
	for _, want := range []string{
		`action="/endpoints/vend/confirm"`,
		`data-sb-wiz-summary`,
		"release-bot", "Reviewer", "reviews", "deploys", "claim", "complete",
		`data-sb-wiz-lifetime`, ">7d<",
		"Vending is irreversible", "shown exactly once", "immutable", // the consequence, stated plainly
		"Vend endpoint →",
		`href="/endpoints/vend/lifetime" data-sb-wiz-back`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("confirm step: missing %q", want)
		}
	}
	// The confirm form must NOT carry the draft as hidden fields — state is server-side.
	for _, bad := range []string{`type="hidden" name="name"`, `type="hidden" name="queues"`, `type="hidden" name="verbs"`, `type="hidden" name="lifetime"`} {
		if strings.Contains(body, bad) {
			t.Errorf("confirm step: draft leaked into hidden field %q — state must be server-side", bad)
		}
	}
	// "until revoked" renders when no lifetime was chosen.
	v.LifetimeLabel = "until revoked"
	if !strings.Contains(renderVendPage(t, h, v, true), "until revoked") {
		t.Error("confirm step: missing the until-revoked lifetime label")
	}
}

// TestVendStepErrorRerenders: a step validation failure re-renders the same page with an inline
// alert — never a dead-end error page (SPEC-0015: the flow is resumable in place).
func TestVendStepErrorRerenders(t *testing.T) {
	h := newTestHandler(t)
	v := testVendStepView("queues")
	v.Error = "at least one queue is required"
	body := renderVendPage(t, h, v, false)
	if !strings.Contains(body, `role="alert"`) || !strings.Contains(body, "data-sb-wiz-error") {
		t.Error("step error must render as an alert with the data-sb-wiz-error hook")
	}
	if !strings.Contains(body, "at least one queue is required") {
		t.Error("step error text missing")
	}
}

// TestVendStepReVendBanner: a wizard seeded from an existing endpoint says so, and states the
// re-vend doctrine (this mints a NEW endpoint; scope is immutable).
func TestVendStepReVendBanner(t *testing.T) {
	h := newTestHandler(t)
	v := testVendStepView("persona")
	v.ReVendOf = "old-bot"
	body := renderVendPage(t, h, v, false)
	for _, want := range []string{"re-vend of old-bot", "mints a NEW endpoint", "revoke the old one"} {
		if !strings.Contains(body, want) {
			t.Errorf("re-vend banner: missing %q", want)
		}
	}
}

// TestRevokeConfirmPageStatesTheKill: the revoke confirm page shows exactly which capability dies
// (agent, URL, credential prefix, scope) and executes only via its explicit POST — the card's
// Revoke is a link here, never a direct kill (SPEC-0015: irreversible steps confirm).
func TestRevokeConfirmPageStatesTheKill(t *testing.T) {
	h := newTestHandler(t)
	card := endpointCard{ID: "e1", AgentName: "reviewer-bot", Principal: "Joe Stump",
		Slug: "reviewer-bot-ab12cd", URL: "https://sb.example.com/mcp/reviewer-bot-ab12cd",
		CredPrefix: "sbk_ab12cd", Queues: []string{"reviews"}, Verbs: []string{"claim"}, State: "active"}
	body := renderPage(t, h, "revoke", view{Title: "Revoke endpoint", Human: testHuman(), CSRF: "tok",
		Shell: shell{Active: "endpoints", DBConnected: true, Initials: "JS"},
		RevokeConfirm: &revokeConfirmView{Card: card}})
	for _, want := range []string{
		`data-sb-revoke-confirm`,
		"reviewer-bot", "sbk_ab12cd…", "https://sb.example.com/mcp/reviewer-bot-ab12cd",
		"instant and total", // the consequence, stated plainly
		`method="post" action="/endpoints/e1/revoke"`,
		`name="csrf_token" value="tok"`,
		`data-sb-revoke-submit`,
		`href="/endpoints"`, // cancel path
	} {
		if !strings.Contains(body, want) {
			t.Errorf("revoke confirm: missing %q", want)
		}
	}
}
