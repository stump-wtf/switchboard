package web

// Operator Personas view (capability-gated): persona cards, the create/edit modal, and the
// publish/unpublish + delete mutations.
//
// Governing: SPEC-0013 REQ "Personas View" (cards + modal + capability gating), ADR-0016 (Operator
// design language). Persona records, verb-subset validation, and the well-known Agent Card endpoint
// live in SPEC-0009 (internal/store/personas.go, internal/web/agentcard.go) — this layer is the
// operator surface only and implements NO scoping rules of its own: every mutation delegates to a
// store method whose sentinel errors (ErrScopeExceeded / ErrConflict / ErrNotFound) it maps to a
// generic response.

import (
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/joestump/switchboard/internal/auth"
	"github.com/joestump/switchboard/internal/store"
)

// SetPersonasEnabled toggles the personas capability for this handler. The server enables it by
// feature detection (the personas store + the well-known Agent Card route are both wired in the same
// Run), realizing SPEC-0013's "hidden-not-broken" gating: while disabled the rail entry is hidden and
// every /personas route 404s. Governing: SPEC-0013 REQ "Personas View" (capability-gated),
// design.md "Capability gating for Personas and Friends".
func (h *Handler) SetPersonasEnabled(enabled bool) { h.personasEnabled = enabled }

// personaCardView is the render model for one persona card (fragments.html "persona_card") and for
// the edit-modal prefill. Skills are the derived Agent Card skills (all-of the required verbs) that
// the card advertises; Verbs is the raw verb_subset chip list. AgentCardURL is the persona's actual
// resolvable well-known path. Governing: SPEC-0013 REQ "Personas View".
type personaCardView struct {
	ID           string
	Name         string
	Slug         string
	AgentID      string
	AgentName    string
	Discoverable bool
	SystemPrompt string
	Verbs        []string
	Queues       []string
	Skills       []agentSkill
	AgentCardURL string
	CSRF         string // per-session token for the card's Publish/Unpublish form
}

// personaAgentOption is one selectable backing agent in the create/edit modal, carrying the union of
// the verbs/queues vended to it across its ACTIVE endpoints — the grant a persona's scope is bounded
// to. The modal offers only these verbs/queues as selectable, so a verb the backing endpoint lacks is
// structurally not selectable. Governing: SPEC-0013 REQ "Personas View" (scenario "Verb subset is
// constrained").
type personaAgentOption struct {
	ID     string
	Name   string
	Verbs  []string
	Queues []string
}

// personaModalView feeds the create/edit modal fragment. Edit distinguishes the two modes: create
// posts to /personas with a selectable backing agent; edit posts to /personas/{id} with the backing
// agent pinned (immutable, ADR-0008) and a Delete action. Selected is the agent whose verb/queue
// chips are rendered server-side (the initially-checked constraint); Checked* pre-check the persona's
// current scope. URLPreview is the live agent-card URL preview. Governing: SPEC-0013 REQ "Personas
// View".
type personaModalView struct {
	Edit          bool
	CSRF          string
	Persona       *personaCardView
	Agents        []personaAgentOption
	Selected      personaAgentOption
	CheckedVerbs  map[string]bool
	CheckedQueues map[string]bool
	Discoverable  bool
	URLPreview    string
	BaseURL       string
}

// personasView is the whole-page model: the persona cards plus the create modal (and per-persona edit
// modals) that the page embeds as hidden templates opened into the shared overlay slot.
type personasView struct {
	Cards     []personaCardView
	Agents    []personaAgentOption
	NewModal  personaModalView
	EditModal map[string]personaModalView // keyed by persona id
	CSRF      string
	BaseURL   string
}

// agentCardURL builds a persona's canonical, resolvable well-known Agent Card path. It mirrors the
// route registered in internal/server (GET /a/{persona_id}/.well-known/agent-card.json) and the
// self-URL personaCard emits, so the URL shown on a card is exactly the one an A2A peer resolves.
// Governing: SPEC-0009 REQ "Well-Known Card Endpoint".
func agentCardURL(baseURL, personaID string) string {
	return strings.TrimRight(baseURL, "/") + "/a/" + personaID + "/.well-known/agent-card.json"
}

// personaSlug derives the human-meaningful URL slug from a persona name, mirroring the store's
// slugifyPersona so the modal's live URL preview matches the slug the store will persist. Presentation
// only — the store remains authoritative.
func personaSlug(name string) string {
	var b strings.Builder
	prevDash := true
	for _, r := range strings.ToLower(name) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		case !prevDash:
			b.WriteByte('-')
			prevDash = true
		}
	}
	base := strings.TrimSuffix(b.String(), "-")
	if base == "" {
		return "persona"
	}
	return base
}

// slugPreviewURL is the create-modal's illustrative agent-card URL preview, keyed on the slug derived
// from the persona name (the persona has no id until saved). Edit modals show the real id-based
// AgentCardURL instead. Governing: SPEC-0013 REQ "Personas View" (name with a live agent-card URL
// preview).
func slugPreviewURL(baseURL, name string) string {
	return strings.TrimRight(baseURL, "/") + "/a/" + personaSlug(name) + "/.well-known/agent-card.json"
}

// personaCardViewFrom projects a store persona plus its backing agent name into the card render model,
// deriving the advertised skills and the resolvable agent-card URL.
func (h *Handler) personaCardViewFrom(p store.Persona, agentName string) personaCardView {
	return personaCardView{
		ID:           p.ID,
		Name:         p.Name,
		Slug:         p.Slug,
		AgentID:      p.AgentID,
		AgentName:    agentName,
		Discoverable: p.Discoverable,
		SystemPrompt: p.SystemPrompt,
		Verbs:        p.VerbSubset,
		Queues:       p.Queues,
		Skills:       deriveSkills(p.VerbSubset),
		AgentCardURL: agentCardURL(h.cfg.BaseURL, p.ID),
	}
}

// unionActiveGrant returns the sorted union of verbs and queues across an agent's ACTIVE endpoints —
// the agent's total vended grant a persona's scope is bounded to (matching the store's
// agentVendedGrant, which is the authority). Revoked endpoints grant nothing.
func unionActiveGrant(eps []store.Endpoint) (verbs, queues []string) {
	vset := map[string]struct{}{}
	qset := map[string]struct{}{}
	for _, ep := range eps {
		if ep.State != "active" {
			continue
		}
		for _, v := range ep.ScopeVerbs {
			vset[v] = struct{}{}
		}
		for _, q := range ep.ScopeQueues {
			qset[q] = struct{}{}
		}
	}
	return sortedKeys(vset), sortedKeys(qset)
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// personaAgentOptions lists the human's agents that have a live vended grant (at least one verb),
// each with its verb/queue union — the backing-agent choices a persona can be a scoped face of. An
// agent with no active endpoint is omitted: a persona must be bounded to a real grant, so there is
// nothing to select. Governing: SPEC-0013 REQ "Personas View", ADR-0008 (registration grants nothing).
func (h *Handler) personaAgentOptions(r *http.Request, humanID string) ([]personaAgentOption, error) {
	agents, err := h.store.ListAgents(r.Context(), humanID)
	if err != nil {
		return nil, err
	}
	var opts []personaAgentOption
	for _, ag := range agents {
		eps, err := h.store.ListEndpoints(r.Context(), ag.ID)
		if err != nil {
			return nil, err
		}
		verbs, queues := unionActiveGrant(eps)
		if len(verbs) == 0 {
			continue // no vended grant → cannot back a persona
		}
		opts = append(opts, personaAgentOption{ID: ag.ID, Name: ag.Name, Verbs: verbs, Queues: queues})
	}
	return opts, nil
}

// agentOptionByID finds a backing-agent option by id, reporting whether it was found.
func agentOptionByID(opts []personaAgentOption, id string) (personaAgentOption, bool) {
	for _, o := range opts {
		if o.ID == id {
			return o, true
		}
	}
	return personaAgentOption{}, false
}

// Personas renders the capability-gated Personas view: persona cards with publish/unpublish + edit,
// and the create modal (plus a hidden per-persona edit modal) opened into the shared overlay slot.
// While the personas capability is disabled the route 404s (hidden-not-broken). Requires human.
// Governing: SPEC-0013 REQ "Personas View", REQ "Information Architecture and Navigation".
func (h *Handler) Personas(w http.ResponseWriter, r *http.Request) {
	if !h.personasEnabled {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	human, _ := auth.FromContext(r.Context())
	csrf := auth.CSRFFromContext(r.Context())
	sh, _ := h.buildShell(r.Context(), "personas", &human)

	var (
		cards  []personaCardView
		agents []personaAgentOption
	)
	if sh.DBConnected {
		opts, err := h.personaAgentOptions(r, human.ID)
		if err != nil {
			h.log.Warn("personas agent options", "err", err)
		}
		agents = opts
		personas, err := h.store.ListPersonas(r.Context(), human.ID)
		if err != nil {
			// Suppressed to a log so the view still renders its shell + empty state (a reload recovers).
			h.log.Warn("personas list", "err", err)
		}
		names := map[string]string{}
		for _, o := range agents {
			names[o.ID] = o.Name
		}
		for _, p := range personas {
			card := h.personaCardViewFrom(p, names[p.AgentID])
			card.CSRF = csrf
			cards = append(cards, card)
		}
	}

	pv := personasView{
		Cards:     cards,
		Agents:    agents,
		CSRF:      csrf,
		BaseURL:   h.cfg.BaseURL,
		NewModal:  h.buildPersonaModal(csrf, agents, nil),
		EditModal: map[string]personaModalView{},
	}
	for _, c := range cards {
		card := c
		pv.EditModal[c.ID] = h.buildPersonaModal(csrf, agents, &card)
	}

	h.render(w, "personas", view{
		Title: "Personas", Human: &human, CSRF: csrf, Shell: sh, Personas: &pv,
	})
}

// buildPersonaModal assembles the create (card == nil) or edit modal model, selecting the backing
// agent whose verb/queue chips render server-side (the constrained set) and pre-checking the persona's
// current scope in edit mode.
func (h *Handler) buildPersonaModal(csrf string, agents []personaAgentOption, card *personaCardView) personaModalView {
	m := personaModalView{
		Edit:          card != nil,
		CSRF:          csrf,
		Persona:       card,
		Agents:        agents,
		CheckedVerbs:  map[string]bool{},
		CheckedQueues: map[string]bool{},
		BaseURL:       h.cfg.BaseURL,
	}
	if card == nil {
		if len(agents) > 0 {
			m.Selected = agents[0]
		}
		m.URLPreview = slugPreviewURL(h.cfg.BaseURL, "")
		return m
	}
	// Edit: pin the persona's backing agent as the selected constraint and pre-check its scope.
	if opt, ok := agentOptionByID(agents, card.AgentID); ok {
		m.Selected = opt
	} else {
		// The backing agent has no live grant anymore (every endpoint revoked); still show the
		// persona's own verbs/queues as the (now un-extendable) constraint so the edit renders.
		m.Selected = personaAgentOption{ID: card.AgentID, Name: card.AgentName, Verbs: card.Verbs, Queues: card.Queues}
	}
	for _, v := range card.Verbs {
		m.CheckedVerbs[v] = true
	}
	for _, q := range card.Queues {
		m.CheckedQueues[q] = true
	}
	m.Discoverable = card.Discoverable
	m.URLPreview = card.AgentCardURL
	return m
}

// CreatePersona records a new persona (a scoped face of one backing agent) and applies its initial
// discoverable state. The store validates the verb/queue subset against the agent's vended grant;
// ErrScopeExceeded (a forged out-of-grant verb) maps to 400. Requires human + CSRF.
// Governing: SPEC-0013 endpoints table POST /personas, SPEC-0009 REQ "Persona Record".
func (h *Handler) CreatePersona(w http.ResponseWriter, r *http.Request) {
	if !h.personasEnabled {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	human, _ := auth.FromContext(r.Context())
	agentID := strings.TrimSpace(r.FormValue("agent_id"))
	name := strings.TrimSpace(r.FormValue("name"))
	if agentID == "" || name == "" {
		http.Error(w, "name and backing agent are required", http.StatusBadRequest)
		return
	}
	p, err := h.store.CreatePersona(r.Context(), store.CreatePersonaParams{
		OwnerHumanID: human.ID,
		AgentID:      agentID,
		Name:         name,
		SystemPrompt: r.FormValue("system_prompt"),
		VerbSubset:   r.Form["verbs"],
		Queues:       r.Form["queues"],
		Description:  strings.TrimSpace(r.FormValue("description")),
	})
	if err != nil {
		h.respondPersonaError(w, "CreatePersona", err)
		return
	}
	// Discoverability is a separate owner-controlled flag (SetPersonaDiscoverable); apply the modal's
	// toggle once the record exists so publishing takes effect on the well-known endpoint immediately.
	if formBool(r, "discoverable") {
		if _, err := h.store.SetPersonaDiscoverable(r.Context(), p.ID, human.ID, true); err != nil {
			h.respondPersonaError(w, "CreatePersona.discoverable", err)
			return
		}
	}
	h.redirectPersonas(w, r)
}

// UpdatePersona edits a persona or, when the request is a card publish/unpublish toggle
// (toggle_discoverable), flips only its discoverable flag. A full edit re-validates the verb/queue
// subset against the backing agent's current grant and then applies the modal's discoverable toggle,
// so publish state changes take effect on the well-known endpoint immediately. Requires human + CSRF.
// Governing: SPEC-0013 endpoints table POST /personas/{id} (update incl. publish toggle), SPEC-0009.
func (h *Handler) UpdatePersona(w http.ResponseWriter, r *http.Request) {
	if !h.personasEnabled {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	human, _ := auth.FromContext(r.Context())
	id := chi.URLParam(r, "id")

	// Card Publish/Unpublish: flip discoverability only, without re-submitting the whole persona.
	if formBool(r, "toggle_discoverable") {
		if _, err := h.store.SetPersonaDiscoverable(r.Context(), id, human.ID, formBool(r, "discoverable")); err != nil {
			h.respondPersonaError(w, "UpdatePersona.toggle", err)
			return
		}
		h.redirectPersonas(w, r)
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	if _, err := h.store.UpdatePersona(r.Context(), store.UpdatePersonaParams{
		ID:           id,
		OwnerHumanID: human.ID,
		Name:         name,
		SystemPrompt: r.FormValue("system_prompt"),
		VerbSubset:   r.Form["verbs"],
		Queues:       r.Form["queues"],
		Description:  strings.TrimSpace(r.FormValue("description")),
	}); err != nil {
		h.respondPersonaError(w, "UpdatePersona", err)
		return
	}
	// Apply the modal's discoverable toggle (UpdatePersona deliberately does not touch it).
	if _, err := h.store.SetPersonaDiscoverable(r.Context(), id, human.ID, formBool(r, "discoverable")); err != nil {
		h.respondPersonaError(w, "UpdatePersona.discoverable", err)
		return
	}
	h.redirectPersonas(w, r)
}

// DeletePersona removes a persona (available only from the edit modal). Requires human + CSRF.
// Governing: SPEC-0013 endpoints table POST /personas/{id}/delete.
func (h *Handler) DeletePersona(w http.ResponseWriter, r *http.Request) {
	if !h.personasEnabled {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	human, _ := auth.FromContext(r.Context())
	if err := h.store.DeletePersona(r.Context(), chi.URLParam(r, "id"), human.ID); err != nil {
		h.respondPersonaError(w, "DeletePersona", err)
		return
	}
	h.redirectPersonas(w, r)
}

// redirectPersonas returns the operator to the authoritative Personas list after a mutation: an
// HX-Redirect header for HTMX (so the client re-navigates and re-renders server truth, and the open
// overlay is discarded) or a 303 for a plain form submit.
func (h *Handler) redirectPersonas(w http.ResponseWriter, r *http.Request) {
	if isHTMX(r) {
		w.Header().Set("HX-Redirect", "/personas")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, "/personas", http.StatusSeeOther)
}

// respondPersonaError maps a store persona error onto a generic HTTP response with no internal detail:
// an out-of-grant scope is a 400 (the client constrained the chips, so this is a forged request), a
// duplicate slug is 409, a cross-owner/missing id is 404, and anything else is a generic 500 (logged).
// Governing: SPEC-0013 REQ "Error Handling Standards".
func (h *Handler) respondPersonaError(w http.ResponseWriter, handler string, err error) {
	switch {
	case errors.Is(err, store.ErrScopeExceeded):
		http.Error(w, "persona scope exceeds the agent's vended grant", http.StatusBadRequest)
	case errors.Is(err, store.ErrConflict):
		http.Error(w, "a persona with that name already exists", http.StatusConflict)
	case errors.Is(err, store.ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	default:
		h.log.Error("persona action", "handler", handler, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// formBool reports whether a form field is a truthy checkbox/flag value ("1", "true", "on").
func formBool(r *http.Request, name string) bool {
	switch strings.ToLower(strings.TrimSpace(r.FormValue(name))) {
	case "1", "true", "on", "yes":
		return true
	default:
		return false
	}
}
