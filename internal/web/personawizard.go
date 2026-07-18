package web

// The persona wizard: the create/edit flow for personas on the shared wizard machinery
// (wizard.go), replacing the SPEC-0013 persona modal. It walks identity → scope → publish as
// routed step pages with server-side state; the scope and publish steps render a LIVE preview of
// the real A2A Agent Card projected from the UNSAVED draft — the operator sees exactly what peers
// will discover before anything is persisted. The preview endpoint renders the same personaCard
// projection (skills via internal/persona/skills.go) that the well-known endpoint serves, so
// preview and published card can never disagree.
//
// Governing: SPEC-0015 REQ "Personas View And Wizard" (wizard with live card preview; scenario
// "Preview before publish"), REQ "Wizard Interaction Pattern" (full pages, server-side step state,
// no-JS completion); SPEC-0009 REQ "Agent Card Mapping", REQ "Skills Derived From Vended
// Capability"; ADR-0018.

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/joestump/switchboard/internal/auth"
	"github.com/joestump/switchboard/internal/store"
)

// personaWizard is the persona flow's wizard definition. The step order is the SPEC-0015 contract:
// identity (name, backing agent, prompt) → scope (verb subset + queues, live card preview) →
// publish (final preview + discoverability + save).
var personaWizard = wizardDef{
	name:  "persona",
	base:  "/personas/wizard",
	steps: []string{"identity", "scope", "publish"},
}

// personaPreviewView feeds the "persona_card_preview" fragment: the A2A Agent Card projected from
// the unsaved draft plus its exact JSON serialization — the same bytes the well-known endpoint
// would serve once the persona is published. Governing: SPEC-0015 REQ "Personas View And Wizard"
// (live card preview), SPEC-0009 REQ "Agent Card Mapping".
type personaPreviewView struct {
	Card agentCard
	JSON string
}

// personaWizStepView is the render model for one persona-wizard step page (templates/personawiz.html).
// Every field prefills from the server-side wizard state, so Back navigation and revisits re-render
// the operator's entered values (SPEC-0015 value-preserving back nav).
type personaWizStepView struct {
	Step      string
	StepNum   int // 1-based
	StepTotal int
	Steps     []vendStepTab
	BackURL   string // "" on the first step
	ActionURL string
	Error     string // step validation error, re-rendered inline on the same page

	Edit     bool   // editing an existing persona (backing agent pinned, Delete offered)
	EditID   string // the persona id being edited ("" for create)
	EditName string // the persona's saved name, for the page tagline

	// identity step
	Name         string
	SystemPrompt string
	Description  string
	AgentID      string
	AgentName    string // the pinned backing agent's name (edit mode)
	Agents       []personaAgentOption

	// scope step
	VerbOptions  []vendChipOption
	QueueOptions []vendChipOption

	// publish step
	Discoverable bool
	Verbs        []string
	Queues       []string

	// Live A2A card preview, rendered on the scope and publish steps from the unsaved draft.
	Preview *personaPreviewView
}

// PersonaWizardStart begins the persona CREATE wizard: it mints fresh server-side state and
// redirects to the first step page. 404s while the personas capability is disabled
// (hidden-not-broken). Requires human. Governing: SPEC-0015 REQ "Personas View And Wizard", REQ
// "Wizard Interaction Pattern".
func (h *Handler) PersonaWizardStart(w http.ResponseWriter, r *http.Request) {
	if !h.personasEnabled {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	token, values, err := h.wizardBegin(w, personaWizard)
	if err != nil {
		h.fail(w, err)
		return
	}
	h.wizards.save(token, values)
	http.Redirect(w, r, personaWizard.stepPath(personaWizard.first()), http.StatusSeeOther)
}

// PersonaWizardEdit begins the persona EDIT wizard: it seeds fresh server-side state from the
// operator's own persona (ownership enforced by the store read) and redirects to the first step.
// The backing agent is pinned — it is immutable after create (ADR-0008/ADR-0009); the wizard
// carries it only to constrain the scope chips. Requires human.
// Governing: SPEC-0015 REQ "Personas View And Wizard".
func (h *Handler) PersonaWizardEdit(w http.ResponseWriter, r *http.Request) {
	if !h.personasEnabled {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	human, _ := auth.FromContext(r.Context())
	p, err := h.store.GetPersona(r.Context(), chi.URLParam(r, "id"), human.ID)
	if err != nil {
		h.notFoundOr(w, err)
		return
	}
	token, values, err := h.wizardBegin(w, personaWizard)
	if err != nil {
		h.fail(w, err)
		return
	}
	values.Set("edit_id", p.ID)
	values.Set("edit_name", p.Name)
	values.Set("name", p.Name)
	values.Set("system_prompt", p.SystemPrompt)
	values.Set("description", p.Description)
	values.Set("agent_id", p.AgentID)
	values["verbs"] = append([]string(nil), p.VerbSubset...)
	values["queues"] = append([]string(nil), p.Queues...)
	if p.Discoverable {
		values.Set("discoverable", "1")
	}
	h.wizards.save(token, values)
	http.Redirect(w, r, personaWizard.stepPath(personaWizard.first()), http.StatusSeeOther)
}

// PersonaWizardStep renders one wizard step page from the server-side state. A request with no live
// wizard state (expired, cleared, or deep-linked cold) restarts the flow rather than rendering a
// half-broken page. Requires human.
func (h *Handler) PersonaWizardStep(w http.ResponseWriter, r *http.Request) {
	if !h.personasEnabled {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	human, _ := auth.FromContext(r.Context())
	slug := chi.URLParam(r, "step")
	if personaWizard.index(slug) < 0 {
		http.NotFound(w, r)
		return
	}
	_, values, ok := h.wizardValues(r, personaWizard)
	if !ok {
		http.Redirect(w, r, personaWizard.base, http.StatusSeeOther)
		return
	}
	h.renderPersonaWizStep(w, r, &human, slug, values, "", http.StatusOK)
}

// PersonaWizardStepSubmit validates and saves one step's fields into the server-side state, then
// advances (POST-redirect-GET, so Back/refresh never re-posts). The publish step is the executing
// one: its POST creates or updates the persona and applies discoverability. A validation failure
// re-renders the SAME step with the entered values and an inline error — never a dead end.
// Requires human + CSRF. Governing: SPEC-0015 REQ "Personas View And Wizard", REQ "Wizard
// Interaction Pattern".
func (h *Handler) PersonaWizardStepSubmit(w http.ResponseWriter, r *http.Request) {
	if !h.personasEnabled {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	human, _ := auth.FromContext(r.Context())
	slug := chi.URLParam(r, "step")
	if personaWizard.index(slug) < 0 {
		http.NotFound(w, r)
		return
	}
	token, values, ok := h.wizardValues(r, personaWizard)
	if !ok {
		http.Redirect(w, r, personaWizard.base, http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	switch slug {
	case "identity":
		name := strings.TrimSpace(r.FormValue("name"))
		if name == "" {
			h.renderPersonaWizStep(w, r, &human, slug, values, "a persona name is required", http.StatusBadRequest)
			return
		}
		values.Set("name", name)
		values.Set("system_prompt", r.FormValue("system_prompt"))
		values.Set("description", strings.TrimSpace(r.FormValue("description")))
		// The backing agent is chosen once, at create; the edit wizard pins it (immutable).
		if values.Get("edit_id") == "" {
			agentID := strings.TrimSpace(r.FormValue("agent_id"))
			opts, err := h.personaAgentOptions(r, human.ID)
			if err != nil {
				h.fail(w, err)
				return
			}
			if _, ok := agentOptionByID(opts, agentID); !ok {
				h.renderPersonaWizStep(w, r, &human, slug, values,
					"choose a backing agent with a vended grant", http.StatusBadRequest)
				return
			}
			if agentID != values.Get("agent_id") {
				// A different backing agent bounds a different grant: drop the drafted subset so the
				// scope step can never carry chips the new agent does not vend.
				delete(values, "verbs")
				delete(values, "queues")
			}
			values.Set("agent_id", agentID)
		}
	case "scope":
		// The scope chips are already constrained to the backing agent's vended grant server-side;
		// the store re-validates the subset at save regardless (SPEC-0009: never trust the client).
		values["verbs"] = r.Form["verbs"]
		values["queues"] = r.Form["queues"]
	case "publish":
		// The executing step: create/update from the SERVER-SIDE state; the publish form carries
		// only the CSRF token and the discoverable toggle. An incomplete draft bounces to its first
		// unfinished step instead of 400ing.
		if strings.TrimSpace(values.Get("name")) == "" {
			http.Redirect(w, r, personaWizard.stepPath("identity"), http.StatusSeeOther)
			return
		}
		values.Set("discoverable", "")
		if formBool(r, "discoverable") {
			values.Set("discoverable", "1")
		}
		if done := h.executePersonaSave(w, r, &human, values); done {
			h.wizardClear(w, r, personaWizard)
			http.Redirect(w, r, "/personas", http.StatusSeeOther)
		} else {
			// The failure re-render needs the draft (incl. the chosen toggle) preserved.
			h.wizards.save(token, values)
		}
		return
	}

	h.wizards.save(token, values)
	http.Redirect(w, r, personaWizard.stepPath(personaWizard.next(slug)), http.StatusSeeOther)
}

// executePersonaSave runs the wizard's terminal store mutation (create or update + discoverability)
// and reports whether it succeeded. A scope/conflict rejection re-renders the publish step with an
// inline error so the operator can go Back and adjust — never a dead-end error page; unexpected
// errors are generic 500s. Governing: SPEC-0015 REQ "Personas View And Wizard"; SPEC-0009 REQ
// "Persona Record" (subset validation lives in the store).
func (h *Handler) executePersonaSave(w http.ResponseWriter, r *http.Request, human *store.Human, values url.Values) bool {
	discoverable := values.Get("discoverable") == "1"
	editID := values.Get("edit_id")

	var err error
	if editID == "" {
		var p store.Persona
		p, err = h.store.CreatePersona(r.Context(), store.CreatePersonaParams{
			OwnerHumanID: human.ID,
			AgentID:      values.Get("agent_id"),
			Name:         values.Get("name"),
			SystemPrompt: values.Get("system_prompt"),
			VerbSubset:   values["verbs"],
			Queues:       values["queues"],
			Description:  values.Get("description"),
		})
		if err == nil && discoverable {
			_, err = h.store.SetPersonaDiscoverable(r.Context(), p.ID, human.ID, true)
		}
	} else {
		_, err = h.store.UpdatePersona(r.Context(), store.UpdatePersonaParams{
			ID:           editID,
			OwnerHumanID: human.ID,
			Name:         values.Get("name"),
			SystemPrompt: values.Get("system_prompt"),
			VerbSubset:   values["verbs"],
			Queues:       values["queues"],
			Description:  values.Get("description"),
		})
		if err == nil {
			_, err = h.store.SetPersonaDiscoverable(r.Context(), editID, human.ID, discoverable)
		}
	}
	switch {
	case err == nil:
		return true
	case errors.Is(err, store.ErrScopeExceeded):
		h.renderPersonaWizStep(w, r, human, "publish", values,
			"the drafted scope exceeds the agent's vended grant — go Back and adjust the verbs/queues", http.StatusBadRequest)
	case errors.Is(err, store.ErrConflict):
		h.renderPersonaWizStep(w, r, human, "publish", values,
			"a persona with that name already exists — go Back and choose another", http.StatusConflict)
	case errors.Is(err, store.ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	default:
		h.log.Error("persona wizard save", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
	return false
}

// PersonaCardPreview is the HTMX live-preview endpoint: it renders the "persona_card_preview"
// fragment from the UNSAVED draft — the server-side wizard state overlaid with the posted form
// fields (the scope step posts its verb/queue chips on every change) — without persisting
// anything. The projection is the same personaCard the well-known endpoint serves (skills derived
// via internal/persona/skills.go), so editing the verb subset updates the previewed card's
// advertised skills to the derived set before anything is saved. Requires human + CSRF (the
// layout's hx-headers carries the token). Governing: SPEC-0015 REQ "Personas View And Wizard"
// (scenario "Preview before publish"); SPEC-0009 REQ "Agent Card Mapping".
func (h *Handler) PersonaCardPreview(w http.ResponseWriter, r *http.Request) {
	if !h.personasEnabled {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	human, _ := auth.FromContext(r.Context())
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	_, values, ok := h.wizardValues(r, personaWizard)
	if !ok {
		values = url.Values{}
	}
	// Overlay the posted fields on the draft. The verb/queue chip sets are ALWAYS replaced — an
	// all-unchecked form posts no "verbs" key, and the preview must then show the skills of the
	// empty subset, not a stale draft.
	values["verbs"] = r.Form["verbs"]
	values["queues"] = r.Form["queues"]
	for _, f := range []string{"name", "system_prompt", "description"} {
		if v, posted := r.Form[f]; posted && len(v) > 0 {
			values.Set(f, v[0])
		}
	}

	out, err := h.renderFragment("persona_card_preview", h.buildPersonaPreview(&human, values))
	if err != nil {
		h.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(out))
}

// buildPersonaPreview projects the wizard draft into the A2A Agent Card preview via the SAME pure
// personaCard projection the well-known endpoint serves — same skill derivation
// (internal/persona/skills.go), same description fallback, same provider mapping — so the preview
// can never drift from the published card. A create draft has no id yet; its URL renders with the
// "{id}" placeholder (never a slug-derived shape the endpoint would not resolve).
func (h *Handler) buildPersonaPreview(human *store.Human, values url.Values) *personaPreviewView {
	id := values.Get("edit_id")
	if id == "" {
		id = pendingCardURLID
	}
	card := personaCard(h.cfg.BaseURL, store.DiscoverablePersona{
		Persona: store.Persona{
			ID:           id,
			Name:         values.Get("name"),
			Description:  values.Get("description"),
			SystemPrompt: values.Get("system_prompt"),
			VerbSubset:   values["verbs"],
			Queues:       values["queues"],
		},
		OwnerDisplayName: human.DisplayName,
	})
	// The exact bytes a peer would fetch: the preview shows the real serialization, not a paraphrase.
	body, err := json.MarshalIndent(card, "", "  ")
	if err != nil {
		// Marshal of a plain struct cannot realistically fail; degrade to the field view only.
		h.log.Error("persona preview marshal", "err", err)
		return &personaPreviewView{Card: card}
	}
	return &personaPreviewView{Card: card, JSON: string(body)}
}

// renderPersonaWizStep builds the step view from the saved draft and renders the full step page.
func (h *Handler) renderPersonaWizStep(w http.ResponseWriter, r *http.Request, human *store.Human, slug string, values url.Values, errMsg string, status int) {
	sh, _ := h.buildShell(r.Context(), "personas", human)
	idx := personaWizard.index(slug)
	v := personaWizStepView{
		Step: slug, StepNum: idx + 1, StepTotal: len(personaWizard.steps),
		ActionURL: personaWizard.stepPath(slug),
		Error:     errMsg,
		Edit:      values.Get("edit_id") != "",
		EditID:    values.Get("edit_id"),
		EditName:  values.Get("edit_name"),
	}
	if prev := personaWizard.prev(slug); prev != "" {
		v.BackURL = personaWizard.stepPath(prev)
	}
	for i, s := range personaWizard.steps {
		v.Steps = append(v.Steps, vendStepTab{Slug: s, Num: i + 1, Current: i == idx, Done: i < idx})
	}

	switch slug {
	case "identity":
		v.Name = values.Get("name")
		v.SystemPrompt = values.Get("system_prompt")
		v.Description = values.Get("description")
		v.AgentID = values.Get("agent_id")
		if v.Edit {
			v.AgentName = h.wizardAgentName(r, human.ID, values.Get("agent_id"))
		} else if opts, err := h.personaAgentOptions(r, human.ID); err != nil {
			// Degrade to the empty-agents explanation rather than failing the page; a reload recovers.
			h.log.Warn("persona wizard agent options", "err", err)
		} else {
			v.Agents = opts
		}
	case "scope":
		verbs, queues := h.wizardGrant(r, human, values)
		chosenVerbs, chosenQueues := values["verbs"], values["queues"]
		for _, verb := range verbs {
			v.VerbOptions = append(v.VerbOptions, vendChipOption{Name: verb, Checked: slices.Contains(chosenVerbs, verb)})
		}
		for _, q := range queues {
			v.QueueOptions = append(v.QueueOptions, vendChipOption{Name: q, Checked: slices.Contains(chosenQueues, q)})
		}
		v.Preview = h.buildPersonaPreview(human, values)
	case "publish":
		v.Name = values.Get("name")
		v.Verbs = values["verbs"]
		v.Queues = values["queues"]
		v.Discoverable = values.Get("discoverable") == "1"
		v.Preview = h.buildPersonaPreview(human, values)
	}

	h.renderStatus(w, status, "personawiz", view{
		Title: "Persona wizard", Human: human, CSRF: auth.CSRFFromContext(r.Context()),
		Shell: sh, PersonasEnabled: h.personasEnabled, PersonaWiz: &v,
	})
}

// wizardGrant resolves the verb/queue vocabulary the scope step may offer: the backing agent's
// current vended grant. When the agent has no live grant anymore (every endpoint revoked — only
// possible mid-edit), it degrades to the persona's own drafted subset so the edit still renders as
// the (now un-extendable) constraint, mirroring the store's validation authority.
func (h *Handler) wizardGrant(r *http.Request, human *store.Human, values url.Values) (verbs, queues []string) {
	opts, err := h.personaAgentOptions(r, human.ID)
	if err != nil {
		h.log.Warn("persona wizard grant", "err", err)
	}
	if opt, ok := agentOptionByID(opts, values.Get("agent_id")); ok {
		return opt.Verbs, opt.Queues
	}
	return values["verbs"], values["queues"]
}

// wizardAgentName resolves the pinned backing agent's display name for the edit wizard's identity
// step ("" degrades to the id-less pinned row; the store re-validates ownership regardless).
func (h *Handler) wizardAgentName(r *http.Request, humanID, agentID string) string {
	agents, err := h.store.ListAgents(r.Context(), humanID)
	if err != nil {
		h.log.Warn("persona wizard agent name", "err", err)
		return ""
	}
	for _, ag := range agents {
		if ag.ID == agentID {
			return ag.Name
		}
	}
	return ""
}
