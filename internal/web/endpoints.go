package web

// Operator Endpoints view (the vended-endpoint cards) and the "+ Vend endpoint" modal, folding the
// retired SPEC-0012 dashboard/agent/vended screens into one surface.
//
// Governing: SPEC-0013 REQ "Endpoints View and Vend Modal"; SPEC-0007 owns vend/revoke semantics
// (hash at rest, immutable scope, revoke = kill) — this layer renders and dispatches, it mints no
// policy of its own; SPEC-0014 REQ "HTTP Wiring Is the Only Wiring" for the one-time reveal.

import (
	"net/http"
	"strings"
	"time"

	"github.com/joestump/switchboard/internal/auth"
	"github.com/joestump/switchboard/internal/cred"
	"github.com/joestump/switchboard/internal/store"
)

// drainVerbs is the vend-modal verb vocabulary: the SPEC-0006 todo-drain surface an endpoint can be
// scoped to. Rendered as toggle chips (all on by default); the server re-validates the submission.
var drainVerbs = []string{"list_todos", "claim", "complete", "fail"}

// endpointCard is the render model for one Endpoints-view card (fragments.html "endpoint_card"). It
// carries only the non-secret credential display prefix — never a reusable credential — plus the
// backing agent name, bound persona (when personas are enabled), scope chips, status, and the
// last-seen / killed-at stamps.
type endpointCard struct {
	ID          string
	AgentName   string
	PersonaName string
	Slug        string
	CredPrefix  string
	Queues      []string
	Verbs       []string
	State       string // active | revoked
	RevokedAt   *time.Time
	LastSeenAt  *time.Time
}

// revealView feeds the one-time credential reveal (fragments.html "vend_reveal"): the minted URL,
// the plaintext credential shown exactly once, and the ready-to-paste HTTP .mcp.json wiring. It is
// produced only as the response to a successful vend POST and is never persisted, so it cannot be
// re-rendered from any card or later page. Governing: SPEC-0013 (credential reveal is one-time),
// SPEC-0014 REQ "HTTP Wiring Is the Only Wiring".
type revealView struct {
	AgentName string
	Slug      string
	Token     string // plaintext credential — shown once, never stored
	MCPJSON   string
	Queues    []string
	Verbs     []string
	CSRF      string
}

// cardFromStore projects a store.EndpointCard into the template render model. The persona name is
// dropped unless personas are enabled, so a card can never surface a persona the operator UI does
// not otherwise expose.
func cardFromStore(c store.EndpointCard, personasEnabled bool) endpointCard {
	card := endpointCard{
		ID:         c.ID,
		AgentName:  c.AgentName,
		Slug:       c.Slug,
		CredPrefix: c.CredentialPrefix,
		Queues:     c.ScopeQueues,
		Verbs:      c.ScopeVerbs,
		State:      c.State,
		RevokedAt:  c.RevokedAt,
		LastSeenAt: c.LastSeenAt,
	}
	if personasEnabled {
		card.PersonaName = c.PersonaName
	}
	return card
}

// Endpoints renders the Endpoints view: the vended-endpoint cards plus the "+ Vend endpoint" action.
// Requires human. Governing: SPEC-0013 REQ "Endpoints View and Vend Modal".
func (h *Handler) Endpoints(w http.ResponseWriter, r *http.Request) {
	human, _ := auth.FromContext(r.Context())
	sh, _ := h.buildShell(r.Context(), "endpoints", &human)
	var cards []endpointCard
	if sh.DBConnected {
		eps, err := h.store.ListEndpointCards(r.Context(), human.ID)
		if err != nil {
			// Suppressed to a log so the view still renders its shell + empty state; a reload recovers.
			h.log.Warn("endpoints list", "err", err)
		}
		cards = make([]endpointCard, 0, len(eps))
		for _, ep := range eps {
			cards = append(cards, cardFromStore(ep, h.personasEnabled))
		}
	}
	h.render(w, "endpoints", view{
		Title: "Endpoints", Human: &human, CSRF: auth.CSRFFromContext(r.Context()),
		Shell: sh, EndpointCards: cards, PersonasEnabled: h.personasEnabled, VerbOptions: drainVerbs,
	})
}

// VendModal returns the "+ Vend endpoint" modal fragment for the overlay slot (HTMX), or the full
// Endpoints page with the modal pre-opened as a no-JS fallback. Requires human.
// Governing: SPEC-0013 REQ "Endpoints View and Vend Modal".
func (h *Handler) VendModal(w http.ResponseWriter, r *http.Request) {
	human, _ := auth.FromContext(r.Context())
	if isHTMX(r) {
		frag, err := h.renderFragment("vend_modal", view{
			CSRF: auth.CSRFFromContext(r.Context()), PersonasEnabled: h.personasEnabled, VerbOptions: drainVerbs,
		})
		if err != nil {
			h.fail(w, err)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(frag))
		return
	}
	// No-JS fallback: render the Endpoints page with the vend form shown inline (VendOpen), so the
	// same fields are reachable without the overlay.
	sh, _ := h.buildShell(r.Context(), "endpoints", &human)
	var cards []endpointCard
	if sh.DBConnected {
		if eps, err := h.store.ListEndpointCards(r.Context(), human.ID); err != nil {
			h.log.Warn("endpoints list for vend form", "err", err)
		} else {
			cards = make([]endpointCard, 0, len(eps))
			for _, e := range eps {
				cards = append(cards, cardFromStore(e, h.personasEnabled))
			}
		}
	}
	h.render(w, "endpoints", view{
		Title: "Vend endpoint", Human: &human, CSRF: auth.CSRFFromContext(r.Context()),
		Shell: sh, EndpointCards: cards, PersonasEnabled: h.personasEnabled, VerbOptions: drainVerbs, VendOpen: true,
	})
}

// Vend mints a scoped endpoint from the modal: it creates the named agent, mints the credential, and
// reveals the plaintext + HTTP wiring exactly once. It refuses (400, mints nothing) unless a name,
// at least one queue, and at least one verb are supplied — the validation runs BEFORE any store
// write, so a rejected submission leaves no agent and no endpoint. Requires human.
// Governing: SPEC-0013 REQ "Endpoints View and Vend Modal" (scenario "Vend modal validates scope"),
// SPEC-0007 (hash at rest, immutable scope), SPEC-0014 (HTTP wiring reveal).
func (h *Handler) Vend(w http.ResponseWriter, r *http.Request) {
	human, _ := auth.FromContext(r.Context())
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	queues := multiValues(r, "queues")
	verbs := multiValues(r, "verbs")
	// Scope validation is a hard gate: no name, no queue, or no verb → reject before minting anything.
	if name == "" || len(queues) == 0 || len(verbs) == 0 {
		http.Error(w, "name, at least one queue, and at least one verb are required", http.StatusBadRequest)
		return
	}

	ag, err := h.store.CreateAgent(r.Context(), human.ID, name, "")
	if err != nil {
		h.fail(w, err)
		return
	}
	token, hash, prefix, err := cred.Mint()
	if err != nil {
		h.fail(w, err)
		return
	}
	// Governing: SPEC-0014 REQ "Streamable HTTP MCP Endpoint" — the per-endpoint URL slug is minted
	// and persisted at vend time; /mcp/{slug} is where this credential is honored.
	slug, err := store.MintSlug(ag.Name)
	if err != nil {
		h.fail(w, err)
		return
	}
	ep, err := h.store.CreateEndpoint(r.Context(), ag.ID, hash, prefix, slug, queues, verbs)
	if err != nil {
		h.fail(w, err)
		return
	}

	reveal := revealView{
		AgentName: ag.Name, Slug: ep.Slug, Token: token,
		MCPJSON: buildMCPJSON(h.cfg.BaseURL, ep.Slug, token),
		Queues:  ep.ScopeQueues, Verbs: ep.ScopeVerbs, CSRF: auth.CSRFFromContext(r.Context()),
	}
	if isHTMX(r) {
		frag, err := h.renderFragment("vend_reveal", reveal)
		if err != nil {
			h.fail(w, err)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(frag))
		return
	}
	// No-JS fallback: render the Endpoints page with the reveal shown inline at the top. The plaintext
	// still appears exactly once — a later GET /endpoints has no token to render.
	sh, _ := h.buildShell(r.Context(), "endpoints", &human)
	var cards []endpointCard
	if sh.DBConnected {
		if eps, err := h.store.ListEndpointCards(r.Context(), human.ID); err != nil {
			h.log.Warn("endpoints list after vend", "err", err)
		} else {
			cards = make([]endpointCard, 0, len(eps))
			for _, e := range eps {
				cards = append(cards, cardFromStore(e, h.personasEnabled))
			}
		}
	}
	h.render(w, "endpoints", view{
		Title: "Endpoint vended", Human: &human, CSRF: auth.CSRFFromContext(r.Context()),
		Shell: sh, EndpointCards: cards, PersonasEnabled: h.personasEnabled, VerbOptions: drainVerbs,
		Reveal: &reveal,
	})
}

// multiValues collects a repeated form field (checkbox chips submit one value each) OR a single
// comma-separated value (the no-JS queue text input), trimming blanks. It underpins the vend form's
// queue/verb collection so both the enhanced chip UI and the plain form degrade to the same scope.
func multiValues(r *http.Request, field string) []string {
	var out []string
	for _, v := range r.Form[field] {
		out = append(out, splitCSV(v)...)
	}
	return out
}
