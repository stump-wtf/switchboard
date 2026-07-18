package web

// Operator Personas view (capability-gated): persona cards (initials block, name, human-authored
// prompt, verb subset, derived skills, vended-as usage) and the publish/unpublish + delete
// mutations. Creating/editing is a full-page wizard (personawizard.go) per the SPEC-0015 wizard
// pattern — the modal is retired.
//
// Governing: SPEC-0015 REQ "Personas View And Wizard", ADR-0018 (charm-web design language).
// Persona records, verb-subset validation, and the well-known Agent Card endpoint live in SPEC-0009
// (internal/store/personas.go, internal/web/agentcard.go) — this layer is the operator surface only
// and implements NO scoping rules of its own: every mutation delegates to a store method whose
// sentinel errors (ErrScopeExceeded / ErrConflict / ErrNotFound) it maps to a generic response.

import (
	"errors"
	"net/http"
	"slices"
	"sort"
	"strings"
	"unicode"

	"github.com/go-chi/chi/v5"

	"github.com/joestump/switchboard/internal/auth"
	"github.com/joestump/switchboard/internal/store"
)

// SetPersonasEnabled toggles the personas capability for this handler. The server enables it by
// feature detection (the personas store + the well-known Agent Card route are both wired in the same
// Run), realizing the "hidden-not-broken" gating: while disabled the rail entry is hidden and
// every /personas route 404s. Governing: SPEC-0015 REQ "Personas View And Wizard" (capability
// gating carries over from SPEC-0013).
func (h *Handler) SetPersonasEnabled(enabled bool) { h.personasEnabled = enabled }

// personaCardView is the render model for one persona card (fragments/personas.html "persona_card"):
// the initials block, name, backing agent, published/draft state, the human-authored prompt, the raw
// verb subset, the derived Agent Card skills (all-of the required verbs), the resolvable agent-card
// URL, and the endpoints currently vended as this persona. Governing: SPEC-0015 REQ "Personas View
// And Wizard".
type personaCardView struct {
	ID           string
	Name         string
	Slug         string
	Initials     string // two-letter avatar tile derived from the persona name (design canvas)
	AgentID      string
	AgentName    string
	Discoverable bool
	SystemPrompt string
	Verbs        []string
	Queues       []string
	Skills       []agentSkill
	AgentCardURL string
	VendedAs     []string // agent names of ACTIVE endpoints bound to this persona (vended-as usage)
	CSRF         string   // per-session token for the card's Publish/Unpublish form
}

// personaAgentOption is one selectable backing agent in the wizard's identity step, carrying the
// union of the verbs/queues vended to it across its ACTIVE endpoints — the grant a persona's scope
// is bounded to. The wizard offers only these verbs/queues as selectable, so a verb the backing
// endpoint lacks is structurally not selectable. Governing: SPEC-0015 REQ "Personas View And
// Wizard" (verb subset), SPEC-0009 REQ "Persona Record".
type personaAgentOption struct {
	ID     string
	Name   string
	Verbs  []string
	Queues []string
}

// personasView is the whole-page model: the persona cards plus the wizard entry point.
type personasView struct {
	Cards []personaCardView
	CSRF  string
}

// agentCardURL builds a persona's canonical, resolvable well-known Agent Card path. It mirrors the
// route registered in internal/server (GET /a/{persona_id}/.well-known/agent-card.json) and the
// self-URL personaCard emits, so the URL shown on a card is exactly the one an A2A peer resolves.
// Governing: SPEC-0009 REQ "Well-Known Card Endpoint".
func agentCardURL(baseURL, personaID string) string {
	return strings.TrimRight(baseURL, "/") + "/a/" + personaID + "/.well-known/agent-card.json"
}

// pendingCardURLID is the placeholder path segment the create wizard's card preview shows in place
// of the persona id (ids are minted by the store on create, so the real resolvable URL cannot exist
// yet). The edit wizard — and every card — shows the real id instead. Using the id-based shape
// (never a name/slug-derived one) keeps the preview honest: the well-known endpoint resolves ONLY
// /a/{persona_id}/…, so a slug URL would never resolve. Governing: SPEC-0009 REQ "Well-Known Card
// Endpoint", SPEC-0015 REQ "Personas View And Wizard" (live card preview).
const pendingCardURLID = "{id}"

// personaCardViewFrom projects a store persona plus its backing agent name into the card render model,
// deriving the initials tile, the advertised skills, and the resolvable agent-card URL.
func (h *Handler) personaCardViewFrom(p store.Persona, agentName string) personaCardView {
	return personaCardView{
		ID:           p.ID,
		Name:         p.Name,
		Slug:         p.Slug,
		Initials:     personaInitials(p.Name),
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

// personaInitials derives the persona card's two-letter initials tile from the persona name: the
// first two letters/digits, upper-cased (mirroring the endpoint card's cardInitials). Names with no
// usable characters fall back to the persona glyph "PA" so the tile never renders blank.
func personaInitials(name string) string {
	var out []rune
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			out = append(out, unicode.ToUpper(r))
			if len(out) == 2 {
				break
			}
		}
	}
	if len(out) == 0 {
		return "PA"
	}
	return string(out)
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
// nothing to select. Governing: SPEC-0015 REQ "Personas View And Wizard", ADR-0008 (registration
// grants nothing).
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

// Personas renders the capability-gated Personas view: persona cards (initials, prompt, verb
// subset, derived skills, vended-as usage) with publish/unpublish and Edit (→ the edit wizard),
// plus the "+ New persona" entry into the create wizard. While the personas capability is disabled
// the route 404s (hidden-not-broken). Requires human. Governing: SPEC-0015 REQ "Personas View And
// Wizard", REQ "Application Shell And Navigation".
func (h *Handler) Personas(w http.ResponseWriter, r *http.Request) {
	if !h.personasEnabled {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	human, _ := auth.FromContext(r.Context())
	csrf := auth.CSRFFromContext(r.Context())
	sh, _ := h.buildShell(r.Context(), "personas", &human)

	var cards []personaCardView
	if sh.DBConnected {
		personas, err := h.store.ListPersonas(r.Context(), human.ID)
		if err != nil {
			// Suppressed to a log so the view still renders its shell + empty state (a reload recovers).
			h.log.Warn("personas list", "err", err)
		}
		names := map[string]string{}
		if agents, err := h.store.ListAgents(r.Context(), human.ID); err != nil {
			h.log.Warn("personas agents", "err", err)
		} else {
			for _, ag := range agents {
				names[ag.ID] = ag.Name
			}
		}
		// Vended-as usage: the ACTIVE endpoints bound to each persona (SPEC-0015 card contract). A
		// read failure degrades to cards without the vended-as row, never a failed page.
		vendedAs := map[string][]string{}
		if eps, err := h.store.ListEndpointCards(r.Context(), human.ID); err != nil {
			h.log.Warn("personas endpoint cards", "err", err)
		} else {
			for _, ep := range eps {
				if ep.PersonaID == "" || ep.State != "active" {
					continue
				}
				if !slices.Contains(vendedAs[ep.PersonaID], ep.AgentName) {
					vendedAs[ep.PersonaID] = append(vendedAs[ep.PersonaID], ep.AgentName)
				}
			}
		}
		for _, p := range personas {
			card := h.personaCardViewFrom(p, names[p.AgentID])
			card.VendedAs = vendedAs[p.ID]
			card.CSRF = csrf
			cards = append(cards, card)
		}
	}

	h.render(w, "personas", view{
		Title: "Personas", Human: &human, CSRF: csrf, Shell: sh,
		Personas: &personasView{Cards: cards, CSRF: csrf},
	})
}

// CreatePersona records a new persona (a scoped face of one backing agent) and applies its initial
// discoverable state. It is the direct single-form create path; the wizard's publish step executes
// the same store calls. The store validates the verb/queue subset against the agent's vended grant;
// ErrScopeExceeded (a forged out-of-grant verb) maps to 400. Requires human + CSRF.
// Governing: SPEC-0015 REQ "Personas View And Wizard", SPEC-0009 REQ "Persona Record".
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
	// Discoverability is a separate owner-controlled flag (SetPersonaDiscoverable); apply the form's
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
// subset against the backing agent's current grant and then applies the discoverable toggle, so
// publish state changes take effect on the well-known endpoint immediately. Requires human + CSRF.
// Governing: SPEC-0015 REQ "Personas View And Wizard" (publish toggle on the card), SPEC-0009.
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
	// Apply the discoverable toggle (UpdatePersona deliberately does not touch it).
	if _, err := h.store.SetPersonaDiscoverable(r.Context(), id, human.ID, formBool(r, "discoverable")); err != nil {
		h.respondPersonaError(w, "UpdatePersona.discoverable", err)
		return
	}
	h.redirectPersonas(w, r)
}

// DeletePersona removes a persona (reached from the edit wizard's publish step). Requires human +
// CSRF. Governing: SPEC-0015 REQ "Personas View And Wizard".
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
// HX-Redirect header for HTMX (so the client re-navigates and re-renders server truth) or a 303 for
// a plain form submit.
func (h *Handler) redirectPersonas(w http.ResponseWriter, r *http.Request) {
	if isHTMX(r) {
		w.Header().Set("HX-Redirect", "/personas")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, "/personas", http.StatusSeeOther)
}

// respondPersonaError maps a store persona error onto a generic HTTP response with no internal detail:
// an out-of-grant scope is a 400 (the wizard constrained the chips, so this is a forged request), a
// duplicate slug is 409, a cross-owner/missing id is 404, and anything else is a generic 500 (logged).
// Governing: SPEC-0015 (SPEC-0013 error-handling standards carry over).
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
