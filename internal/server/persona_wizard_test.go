// End-to-end persona-wizard tests through the real router + PostgreSQL, driven exactly the way a
// browser WITH JAVASCRIPT DISABLED drives it: plain GETs, plain form POSTs, redirects, cookies.
// They bind the SPEC-0015 "Personas View And Wizard" contract — routed step pages with server-side
// state, the scope chips constrained to the backing agent's vended grant, the LIVE A2A card
// preview rendered from the unsaved draft (scenario "Preview before publish": editing the verb
// subset updates the previewed skills before anything is persisted), the publish-step save, the
// edit flow's pinned backing agent, and the unchanged well-known card output (SPEC-0009). Skipped
// without SWITCHBOARD_TEST_DATABASE_URL (Gitea CI runs DB-less; the GitHub mirror provides
// Postgres). Governing: SPEC-0015 REQ "Personas View And Wizard", REQ "Wizard Interaction Pattern"
// (scenario "JavaScript disabled"); SPEC-0009 REQ "Agent Card Mapping"; ADR-0018.
package server

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/joestump/switchboard/internal/store"
)

// TestPersonaWizardCreateWithLivePreview walks the whole create wizard as a no-JS browser:
// start → identity → scope → publish → saved, exercising the live preview endpoint mid-flow and
// proving nothing persists before the final POST.
func TestPersonaWizardCreateWithLivePreview(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	human, token := mintSession(t, st, ctx, "persona-wiz-op", "Wiz Op", "persona-wiz@example.com")
	c := newWizClient(t, r, token)
	csrf := scrapeCSRF(t, c.get("/personas").Body.String())

	// A backing agent with a vended grant — the scope the persona is bounded to.
	ag, err := st.CreateAgent(ctx, human.ID, "review-bot", "")
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if _, err := st.CreateEndpoint(ctx, ag.ID, "hash-wiz", "sbk_wiz1", "review-bot-wiz1",
		[]string{"reviews"}, []string{"list_todos", "claim", "complete", "create_for"}); err != nil {
		t.Fatalf("create endpoint: %v", err)
	}

	// Start: mints server-side state (cookie) and lands on the identity step.
	followTo(t, c.get("/personas/wizard"), "/personas/wizard/identity")
	if _, ok := c.cookies["sb_wiz_persona"]; !ok {
		t.Fatal("wizard start did not set the server-side state token cookie")
	}

	// Step 1 — identity: a full page with a plain form offering the backing agent.
	step1 := c.get("/personas/wizard/identity")
	if step1.Code != http.StatusOK {
		t.Fatalf("identity step: got %d", step1.Code)
	}
	for _, want := range []string{`data-sb-wizard="persona"`, `method="post"`,
		`action="/personas/wizard/identity"`, "review-bot", `name="agent_id"`} {
		if !strings.Contains(step1.Body.String(), want) {
			t.Errorf("identity step: missing %q", want)
		}
	}
	followTo(t, c.do(http.MethodPost, "/personas/wizard/identity", url.Values{
		"csrf_token":    {csrf},
		"name":          {"Reviewer"},
		"agent_id":      {ag.ID},
		"system_prompt": {"You are a careful code reviewer."},
		"description":   {"Reviews pull requests."},
	}), "/personas/wizard/scope")

	// Step 2 — scope: chips constrained to the agent's vended grant, plus the live preview region.
	scope := c.get("/personas/wizard/scope").Body.String()
	for _, want := range []string{
		`name="verbs" value="claim"`,
		`name="verbs" value="create_for"`, // vended → offered
		`id="sb-persona-preview"`,
		`hx-post="/personas/wizard/preview"`,
	} {
		if !strings.Contains(scope, want) {
			t.Errorf("scope step: missing %q", want)
		}
	}
	if strings.Contains(scope, `name="verbs" value="fail"`) {
		t.Error("scope step must not offer a verb the backing agent does not vend")
	}

	// SPEC-0015 scenario "Preview before publish": the preview endpoint renders the card from the
	// UNSAVED draft. The drain trio derives process-work; toggling create_for on updates the
	// previewed skills to the derived set — and still nothing is persisted.
	trio := c.do(http.MethodPost, "/personas/wizard/preview", url.Values{
		"csrf_token": {csrf}, "verbs": {"list_todos", "claim", "complete"},
	})
	if trio.Code != http.StatusOK {
		t.Fatalf("preview: got %d (body %.300s)", trio.Code, trio.Body.String())
	}
	if !strings.Contains(trio.Body.String(), `data-sb-skill="process-work"`) ||
		strings.Contains(trio.Body.String(), `data-sb-skill="delegate-work"`) {
		t.Errorf("drain-trio preview must derive exactly process-work:\n%.500s", trio.Body.String())
	}
	wider := c.do(http.MethodPost, "/personas/wizard/preview", url.Values{
		"csrf_token": {csrf}, "verbs": {"list_todos", "claim", "complete", "create_for"},
	})
	if !strings.Contains(wider.Body.String(), `data-sb-skill="delegate-work"`) {
		t.Errorf("adding create_for must add delegate-work to the previewed skills:\n%.500s", wider.Body.String())
	}
	if personas, _ := st.ListPersonas(ctx, human.ID); len(personas) != 0 {
		t.Fatalf("the live preview must persist nothing, found %d personas", len(personas))
	}

	// Commit the scope draft (trio only) and land on publish.
	followTo(t, c.do(http.MethodPost, "/personas/wizard/scope", url.Values{
		"csrf_token": {csrf}, "verbs": {"list_todos", "claim", "complete"}, "queues": {"reviews"},
	}), "/personas/wizard/publish")

	// Back navigation preserves entered values (SPEC-0015).
	back := c.get("/personas/wizard/scope").Body.String()
	if !strings.Contains(back, `value="claim" checked`) {
		t.Error("back nav: scope step lost the chosen verbs")
	}
	backID := c.get("/personas/wizard/identity").Body.String()
	if !strings.Contains(backID, `value="Reviewer"`) {
		t.Error("back nav: identity step lost the entered name")
	}

	// Step 3 — publish: the final preview from the unsaved draft (the "{id}" placeholder URL — the
	// id is minted on save) and the save form. Still nothing persisted.
	publish := c.get("/personas/wizard/publish").Body.String()
	for _, want := range []string{`data-sb-skill="process-work"`, "/a/{id}/", `name="discoverable"`, "Create persona"} {
		if !strings.Contains(publish, want) {
			t.Errorf("publish step: missing %q", want)
		}
	}
	if personas, _ := st.ListPersonas(ctx, human.ID); len(personas) != 0 {
		t.Fatalf("nothing may persist before the publish POST, found %d personas", len(personas))
	}

	// The save: creates the persona from SERVER-SIDE state and applies discoverability.
	followTo(t, c.do(http.MethodPost, "/personas/wizard/publish",
		url.Values{"csrf_token": {csrf}, "discoverable": {"1"}}), "/personas")
	personas, err := st.ListPersonas(ctx, human.ID)
	if err != nil || len(personas) != 1 {
		t.Fatalf("expected exactly one persona after save, got %d (err %v)", len(personas), err)
	}
	p := personas[0]
	if p.Name != "Reviewer" || !p.Discoverable || len(p.VerbSubset) != 3 || len(p.Queues) != 1 {
		t.Fatalf("saved persona = %+v, want Reviewer, discoverable, 3 verbs, 1 queue", p)
	}

	// The well-known card resolves and advertises exactly the previewed derived set — the preview
	// and the published card share one projection (SPEC-0009 output unchanged).
	card := anonRequest(t, r, http.MethodGet, "/a/"+p.ID+"/.well-known/agent-card.json")
	if card.Code != http.StatusOK {
		t.Fatalf("published persona card: got %d, want 200", card.Code)
	}
	if !strings.Contains(card.Body.String(), `"id":"process-work"`) ||
		strings.Contains(card.Body.String(), "delegate-work") {
		t.Errorf("published card must advertise exactly the derived set:\n%s", card.Body.String())
	}

	// The wizard state died with the save: revisiting a step restarts the flow.
	followTo(t, c.get("/personas/wizard/publish"), "/personas/wizard")
}

// TestPersonaWizardEditSeedsAndUpdates drives the edit flow: GET /personas/{id}/edit seeds the
// wizard from the saved persona (pinned backing agent, prefilled fields, checked chips); widening
// the verb subset updates the live preview BEFORE the save persists anything; the publish POST then
// applies the new subset and the card recomputes.
func TestPersonaWizardEditSeedsAndUpdates(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	human, token := mintSession(t, st, ctx, "persona-wiz-edit", "Edit Op", "persona-wiz-edit@example.com")
	c := newWizClient(t, r, token)
	csrf := scrapeCSRF(t, c.get("/personas").Body.String())

	ag, err := st.CreateAgent(ctx, human.ID, "deploy-bot", "")
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if _, err := st.CreateEndpoint(ctx, ag.ID, "hash-wiz2", "sbk_wiz2", "deploy-bot-wiz2",
		[]string{"deploys"}, []string{"list_todos", "claim", "complete", "create_for"}); err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
	p, err := st.CreatePersona(ctx, store.CreatePersonaParams{
		OwnerHumanID: human.ID, AgentID: ag.ID, Name: "Deployer",
		SystemPrompt: "You ship approved builds.",
		VerbSubset:   []string{"list_todos", "claim", "complete"}, Queues: []string{"deploys"},
	})
	if err != nil {
		t.Fatalf("create persona: %v", err)
	}

	// Edit start seeds the draft and lands on identity with the agent pinned.
	followTo(t, c.get("/personas/"+p.ID+"/edit"), "/personas/wizard/identity")
	identity := c.get("/personas/wizard/identity").Body.String()
	for _, want := range []string{`value="Deployer"`, "data-sb-pw-agent-pinned", "deploy-bot", "You ship approved builds."} {
		if !strings.Contains(identity, want) {
			t.Errorf("edit identity step: missing %q", want)
		}
	}
	if strings.Contains(identity, `name="agent_id"`) {
		t.Error("edit identity step must pin the backing agent (no agent_id field)")
	}

	// Keep identity, widen the scope: the seeded chips are checked; add create_for.
	followTo(t, c.do(http.MethodPost, "/personas/wizard/identity", url.Values{
		"csrf_token": {csrf}, "name": {"Deployer"}, "system_prompt": {"You ship approved builds."},
	}), "/personas/wizard/scope")
	scope := c.get("/personas/wizard/scope").Body.String()
	if !strings.Contains(scope, `value="claim" checked`) {
		t.Error("edit scope step must pre-check the persona's saved verbs")
	}

	// Scenario "Preview before publish", edit flavor: the widened draft previews delegate-work
	// while the SAVED persona still lacks it.
	preview := c.do(http.MethodPost, "/personas/wizard/preview", url.Values{
		"csrf_token": {csrf}, "verbs": {"list_todos", "claim", "complete", "create_for"},
	})
	if !strings.Contains(preview.Body.String(), `data-sb-skill="delegate-work"`) {
		t.Errorf("widened edit draft must preview delegate-work:\n%.500s", preview.Body.String())
	}
	// The edit draft previews the persona's REAL card URL (not the create placeholder).
	if !strings.Contains(preview.Body.String(), "/a/"+p.ID+"/") {
		t.Errorf("edit preview must carry the persona's real id URL:\n%.500s", preview.Body.String())
	}
	if saved, _ := st.GetPersona(ctx, p.ID, human.ID); len(saved.VerbSubset) != 3 {
		t.Fatalf("preview must not persist: saved subset = %v", saved.VerbSubset)
	}

	// Commit the widened scope, then save from the publish step (kept as a draft).
	followTo(t, c.do(http.MethodPost, "/personas/wizard/scope", url.Values{
		"csrf_token": {csrf}, "verbs": {"list_todos", "claim", "complete", "create_for"}, "queues": {"deploys"},
	}), "/personas/wizard/publish")
	followTo(t, c.do(http.MethodPost, "/personas/wizard/publish",
		url.Values{"csrf_token": {csrf}}), "/personas")

	saved, err := st.GetPersona(ctx, p.ID, human.ID)
	if err != nil {
		t.Fatalf("get persona: %v", err)
	}
	if len(saved.VerbSubset) != 4 || saved.Discoverable {
		t.Fatalf("saved persona = %+v, want 4 verbs and draft (discoverable unchecked)", saved)
	}
}
