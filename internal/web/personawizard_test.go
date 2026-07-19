package web

// DOM render contract for the persona wizard's step pages (templates/personawiz.html) and the live
// A2A card-preview endpoint: each routed step is a full page with plain method=post forms (no-JS
// completion), a value-preserving Back link, a step tracker, and — on the scope and publish steps —
// the live agent-card preview rendered from the UNSAVED draft. Assertions key on data-sb-* hooks
// and ids, never on style classes. The state-machine behavior over HTTP (redirects, seeding,
// server-side value preservation, save-on-publish) is bound end-to-end in the DB-backed
// internal/server/persona_wizard_test.go.
// Governing: SPEC-0015 REQ "Personas View And Wizard" (scenario "Preview before publish"), REQ
// "Wizard Interaction Pattern"; SPEC-0009 REQ "Agent Card Mapping"; ADR-0018.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// chiRequest builds a request carrying a chi route parameter, for calling handlers that read
// chi.URLParam outside a real router.
func chiRequest(method, path, param, value string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, path, body)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add(param, value)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

// testPersonaWizStepView builds a render-ready step view the way renderPersonaWizStep does
// (tracker tabs, back/action URLs), for template tests that bypass the handler.
func testPersonaWizStepView(step string) *personaWizStepView {
	idx := personaWizard.index(step)
	v := &personaWizStepView{
		Step: step, StepNum: idx + 1, StepTotal: len(personaWizard.steps),
		ActionURL: personaWizard.stepPath(step),
	}
	if prev := personaWizard.prev(step); prev != "" {
		v.BackURL = personaWizard.stepPath(prev)
	}
	for i, s := range personaWizard.steps {
		v.Steps = append(v.Steps, vendStepTab{Slug: s, Num: i + 1, Current: i == idx, Done: i < idx})
	}
	return v
}

func renderPersonaWizPage(t *testing.T, h *Handler, v *personaWizStepView) string {
	t.Helper()
	return renderPage(t, h, "personawiz", view{
		Title: "Persona wizard", Human: testHuman(), CSRF: "tok",
		Shell:           shell{Active: "personas", DBConnected: true, Initials: "JS", PersonasEnabled: true},
		PersonasEnabled: true, PersonaWiz: v,
	})
}

// TestPersonaWizardStepOrder pins the SPEC-0015 step contract: identity → scope → publish, under
// /personas/wizard.
func TestPersonaWizardStepOrder(t *testing.T) {
	want := []string{"identity", "scope", "publish"}
	if len(personaWizard.steps) != len(want) {
		t.Fatalf("persona wizard has %d steps, want %d", len(personaWizard.steps), len(want))
	}
	for i, s := range want {
		if personaWizard.steps[i] != s {
			t.Errorf("step %d = %q, want %q", i, personaWizard.steps[i], s)
		}
	}
	if personaWizard.base != "/personas/wizard" {
		t.Errorf("persona wizard base = %q", personaWizard.base)
	}
}

// TestPersonaWizardStartRedirectsToFirstStep: GET /personas/wizard mints server-side state, sets
// the path-scoped state cookie, and 303s to the identity step (SPEC-0015 wizard pattern).
func TestPersonaWizardStartRedirectsToFirstStep(t *testing.T) {
	h := newTestHandler(t)
	h.personasEnabled = true
	rec := httptest.NewRecorder()
	h.PersonaWizardStart(rec, httptest.NewRequest(http.MethodGet, "/personas/wizard", nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("start: got %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/personas/wizard/identity" {
		t.Errorf("start redirected to %q, want /personas/wizard/identity", loc)
	}
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == personaWizard.cookieName() {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatalf("start must set the wizard state cookie %q", personaWizard.cookieName())
	}
	if !cookie.HttpOnly || cookie.Path != personaWizard.base {
		t.Errorf("state cookie must be HttpOnly and path-scoped to %s, got %+v", personaWizard.base, cookie)
	}
}

// TestPersonaWizardStepWithoutStateRestarts: a step request with no live wizard state (expired,
// cleared, or deep-linked cold) 303s back to the wizard start instead of rendering a half-broken
// page.
func TestPersonaWizardStepWithoutStateRestarts(t *testing.T) {
	h := newTestHandler(t)
	h.personasEnabled = true
	req := chiRequest(http.MethodGet, "/personas/wizard/scope", "step", "scope", nil)
	rec := httptest.NewRecorder()
	h.PersonaWizardStep(rec, req)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != personaWizard.base {
		t.Fatalf("cold step: got %d → %q, want 303 → %s", rec.Code, rec.Header().Get("Location"), personaWizard.base)
	}
}

// TestPersonaWizIdentityStepCreate: step 1 collects name/backing agent/prompt/description with the
// draft's values prefilled (value-preserving back nav) and a plain method=post form (no-JS).
func TestPersonaWizIdentityStepCreate(t *testing.T) {
	h := newTestHandler(t)
	v := testPersonaWizStepView("identity")
	v.Name = "Reviewer"
	v.SystemPrompt = "You are a careful code reviewer."
	v.Description = "Reviews pull requests."
	v.AgentID = "agent-2"
	v.Agents = []personaAgentOption{
		{ID: "agent-1", Name: "review-bot", Verbs: []string{"claim"}},
		{ID: "agent-2", Name: "deploy-bot", Verbs: []string{"create_for"}},
	}
	body := renderPersonaWizPage(t, h, v)
	for _, want := range []string{
		`data-sb-wizard="persona"`,    // the wizard page hook
		`aria-current="step"`,         // tracker marks the current step
		`data-sb-wiz-step="identity"`, // tracker entries carry their slugs
		"step 1 of 3",                 // progress line
		`method="post"`,               // plain form — no-JS completion
		`action="/personas/wizard/identity"`,
		`name="csrf_token" value="tok"`,
		`id="sb-pw-name"`, `value="Reviewer"`, // prefilled name
		`id="sb-pw-agent"`, `name="agent_id"`, // backing agent select
		`<option value="agent-2" selected>deploy-bot</option>`, // draft choice re-selected
		`id="sb-pw-prompt"`, "You are a careful code reviewer.", // prefilled prompt
		`id="sb-pw-desc"`, `value="Reviews pull requests."`, // prefilled description
		`href="/personas" data-sb-wiz-cancel`, // cancel escapes to the view
		"data-sb-pw-submit",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("identity step: missing %q", want)
		}
	}
	// First step: no Back link.
	if strings.Contains(body, "data-sb-wiz-back") {
		t.Error("identity step must not render a Back link (it is the first step)")
	}
}

// TestPersonaWizIdentityStepNoAgents: with no vended agent the identity step explains the empty
// backing-agent list and disables Continue — the entry point stays visible, the wizard explains.
func TestPersonaWizIdentityStepNoAgents(t *testing.T) {
	h := newTestHandler(t)
	body := renderPersonaWizPage(t, h, testPersonaWizStepView("identity"))
	if !strings.Contains(body, "data-sb-pw-no-agents") {
		t.Errorf("agentless identity step should explain the empty backing-agent list:\n%s", body)
	}
	if !strings.Contains(body, "disabled data-sb-pw-submit") {
		t.Errorf("agentless identity step must disable Continue:\n%s", body)
	}
}

// TestPersonaWizIdentityStepEditPinsAgent: the edit wizard pins the backing agent (immutable — no
// select) and shows the pinned name.
func TestPersonaWizIdentityStepEditPinsAgent(t *testing.T) {
	h := newTestHandler(t)
	v := testPersonaWizStepView("identity")
	v.Edit = true
	v.EditID = "p-1"
	v.EditName = "Reviewer"
	v.AgentName = "review-bot"
	body := renderPersonaWizPage(t, h, v)
	for _, want := range []string{
		"data-sb-pw-agent-pinned", "review-bot", "the backing agent is immutable",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("edit identity step: missing %q", want)
		}
	}
	if strings.Contains(body, `name="agent_id"`) {
		t.Errorf("edit identity step must pin the backing agent (no agent_id field):\n%s", body)
	}
}

// TestPersonaWizScopeStepChipsAndLivePreview: the scope step offers ONLY the backing agent's vended
// verbs/queues as chips (draft choices checked), and carries the live preview region wired to the
// HTMX preview endpoint — re-rendered from the CURRENT form fields on every change, so the
// advertised skills update before anything is persisted (scenario "Preview before publish").
func TestPersonaWizScopeStepChipsAndLivePreview(t *testing.T) {
	h := newTestHandler(t)
	v := testPersonaWizStepView("scope")
	v.VerbOptions = []vendChipOption{
		{Name: "list_todos", Checked: true}, {Name: "claim", Checked: true},
		{Name: "complete", Checked: true}, {Name: "create_for"},
	}
	v.QueueOptions = []vendChipOption{{Name: "reviews", Checked: true}}
	v.Preview = h.buildPersonaPreview(testHuman(), url.Values{
		"name": {"Reviewer"}, "verbs": {"list_todos", "claim", "complete"},
	})
	body := renderPersonaWizPage(t, h, v)
	for _, want := range []string{
		"step 2 of 3",
		`action="/personas/wizard/scope"`,
		`data-sb-pw-verbs`, // verb chipset
		`<input type="checkbox" name="verbs" value="claim" checked>`,
		`<input type="checkbox" name="verbs" value="create_for">`, // offered but unchecked
		`data-sb-pw-queues`,
		`<input type="checkbox" name="queues" value="reviews" checked>`,
		`href="/personas/wizard/identity" data-sb-wiz-back`, // value-preserving Back
		// The live preview region: HTMX posts the form's current fields to the preview endpoint.
		`id="sb-persona-preview"`,
		`hx-post="/personas/wizard/preview"`,
		`hx-include="#sb-wiz-form"`,
		`hx-trigger="change from:#sb-wiz-form"`,
		"data-sb-card-preview",
		`data-sb-skill="process-work"`, // the drafted trio derives process-work
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scope step: missing %q", want)
		}
	}
	if strings.Contains(body, `data-sb-skill="delegate-work"`) {
		t.Errorf("scope step preview must not advertise delegate-work without create_for:\n%s", body)
	}
}

// TestPersonaWizPublishStepPreviewAndSave: the final step shows the full live card preview from the
// unsaved draft, the Published/Draft toggle, the scope summary, and Create/Save; in edit mode it
// additionally carries the Delete form. Nothing on this page is persisted until the POST.
func TestPersonaWizPublishStepPreviewAndSave(t *testing.T) {
	h := newTestHandler(t)
	v := testPersonaWizStepView("publish")
	v.Name = "Reviewer"
	v.Verbs = []string{"list_todos", "claim", "complete", "create_for"}
	v.Queues = []string{"reviews"}
	v.Discoverable = true
	v.Preview = h.buildPersonaPreview(testHuman(), url.Values{
		"name": {"Reviewer"}, "verbs": {v.Verbs[0], v.Verbs[1], v.Verbs[2], v.Verbs[3]},
	})
	body := renderPersonaWizPage(t, h, v)
	for _, want := range []string{
		"step 3 of 3",
		`action="/personas/wizard/publish"`,
		"data-sb-wiz-summary",
		`name="discoverable"`, `checked`, // the drafted publish state pre-checked
		`id="sb-persona-preview"`,
		"data-sb-card-preview",
		`data-sb-skill="process-work"`,
		`data-sb-skill="delegate-work"`, // create_for in the draft derives delegate-work
		`data-sb-card-json`,             // the exact JSON peers would fetch
		"Create persona",
		`href="/personas/wizard/scope" data-sb-wiz-back`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("publish step: missing %q", want)
		}
	}
	// Create mode: no Delete, and the preview URL carries the honest "{id}" placeholder (never a
	// slug shape the well-known endpoint would not resolve).
	if strings.Contains(body, "data-sb-pw-delete") {
		t.Error("create-mode publish step must not carry a Delete form")
	}
	if !strings.Contains(body, "/a/"+pendingCardURLID+"/") {
		t.Errorf("create-mode preview should show the id-based placeholder URL:\n%s", body)
	}

	// Edit mode: Delete appears and the preview URL uses the real persona id.
	v.Edit = true
	v.EditID = "0a1b2c3d-0000-0000-0000-000000000001"
	v.Preview = h.buildPersonaPreview(testHuman(), url.Values{
		"edit_id": {v.EditID}, "name": {"Reviewer"}, "verbs": {"claim"},
	})
	edit := renderPersonaWizPage(t, h, v)
	for _, want := range []string{
		"data-sb-pw-delete",
		`action="/personas/0a1b2c3d-0000-0000-0000-000000000001/delete"`,
		"Save persona",
		"/a/0a1b2c3d-0000-0000-0000-000000000001/",
	} {
		if !strings.Contains(edit, want) {
			t.Errorf("edit publish step: missing %q", want)
		}
	}
}

// postPreview drives the live-preview endpoint the way the scope step's HTMX region does: the
// form's current fields, urlencoded. No wizard state cookie is attached, so the preview renders
// from exactly the posted (unsaved) fields.
func postPreview(t *testing.T, h *Handler, form url.Values) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/personas/wizard/preview", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.PersonaCardPreview(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview: got %d, body %q", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// TestPersonaCardPreviewSkillsFollowVerbSubset is the SPEC-0015 named scenario ("Preview before
// publish") at the preview endpoint: editing the verb subset updates the previewed card's
// advertised skills to the derived set — process-work appears only with the full drain trio,
// delegate-work only with create_for — all from UNSAVED form state (the handler runs with no store
// behind it, so nothing can possibly persist). Governing: SPEC-0015 REQ "Personas View And
// Wizard"; SPEC-0009 REQ "Skills Derived From Vended Capability".
func TestPersonaCardPreviewSkillsFollowVerbSubset(t *testing.T) {
	h := newTestHandler(t) // nil store: the preview must be a pure projection of the draft
	h.personasEnabled = true

	trio := postPreview(t, h, url.Values{
		"name":  {"Reviewer"},
		"verbs": {"list_todos", "claim", "complete"},
	})
	if !strings.Contains(trio, `data-sb-skill="process-work"`) {
		t.Errorf("drain-trio preview must advertise process-work:\n%s", trio)
	}
	if strings.Contains(trio, `data-sb-skill="delegate-work"`) {
		t.Errorf("preview must not advertise delegate-work without create_for:\n%s", trio)
	}

	// The operator toggles create_for on: the previewed skills recompute to the derived set.
	withDelegate := postPreview(t, h, url.Values{
		"name":  {"Reviewer"},
		"verbs": {"list_todos", "claim", "complete", "create_for"},
	})
	if !strings.Contains(withDelegate, `data-sb-skill="delegate-work"`) {
		t.Errorf("adding create_for must add delegate-work to the previewed skills:\n%s", withDelegate)
	}

	// The operator unchecks everything: an all-unchecked form posts no verbs at all, and the
	// preview must show the empty derived set — never a stale draft.
	none := postPreview(t, h, url.Values{"name": {"Reviewer"}})
	if strings.Contains(none, "data-sb-skill=") {
		t.Errorf("an empty verb subset must derive no skills:\n%s", none)
	}
	if !strings.Contains(none, "no skills derived") {
		t.Errorf("the empty derived set should render its honest empty state:\n%s", none)
	}
}

// TestPersonaCardPreviewShowsCardShape: the preview renders the card's discovery fields (name, the
// id-based URL with the "{id}" placeholder pre-create, the A2A protocol version chip) and the
// exact JSON serialization the well-known endpoint would serve.
func TestPersonaCardPreviewShowsCardShape(t *testing.T) {
	h := newTestHandler(t)
	h.personasEnabled = true
	body := postPreview(t, h, url.Values{
		"name":        {"Reviewer"},
		"description": {"Reviews pull requests."},
		"verbs":       {"list_todos", "claim", "complete"},
	})
	for _, want := range []string{
		"Reviewer",
		"Reviews pull requests.",
		"https://sb.example.com/a/" + pendingCardURLID + "/", // id-shape URL, ids minted on create
		"a2a " + a2aProtocolVersion,
		`id="sb-persona-preview-json"`,
		`&#34;protocolVersion&#34;: &#34;` + a2aProtocolVersion + `&#34;`, // the raw card JSON, HTML-escaped
	} {
		if !strings.Contains(body, want) {
			t.Errorf("preview missing %q:\n%s", want, body)
		}
	}
	// Never a slug-derived URL: the well-known endpoint resolves only /a/{persona_id}/….
	if strings.Contains(body, "/a/reviewer/") {
		t.Errorf("preview must never derive a slug URL:\n%s", body)
	}
}
