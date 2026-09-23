package web

// Operator Endpoints view: the vended-endpoint cards (principal, persona, queues, verbs, MCP URL,
// credential tail, expiry countdown, rotate/revoke) plus the vend flow's execution and the revoke
// confirm page. Vending itself is the full-page wizard (vendwizard.go); this file owns the cards,
// the mint, and the one-time reveal.
//
// Governing: SPEC-0015 REQ "Endpoints View And Vend Wizard"; SPEC-0007 owns vend/revoke semantics
// (hash at rest, immutable scope, revoke = kill) — this layer renders and dispatches, it mints no
// policy of its own; SPEC-0014 REQ "HTTP Wiring Is the Only Wiring" for the one-time reveal;
// SPEC-0016 REQ "Credential Lifetime" (expiry countdown), ADR-0018.

import (
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/go-chi/chi/v5"

	"github.com/stump-wtf/switchboard/internal/auth"
	"github.com/stump-wtf/switchboard/internal/cred"
	"github.com/stump-wtf/switchboard/internal/mcp"
	"github.com/stump-wtf/switchboard/internal/store"
)

// vendVerbOption is one verb toggle chip in the vend wizard's verbs step: the verb name plus
// whether the chip starts checked. The vocabulary is enumerated from the agent-tools surface
// (internal/mcp verbs.go) rather than hardcoded here, so the wizard can never offer a verb the MCP
// layer does not serve.
type vendVerbOption struct {
	Name    string
	Checked bool
}

// vendVerbOptions builds the verb chips: the SPEC-0006 todo-drain surface starts checked (the core
// scope an endpoint exists to carry), while the webhook self-management and event-history verbs
// start unchecked so wider grants are always a deliberate toggle. The server re-validates the
// submission. Governing: SPEC-0015 REQ "Endpoints View And Vend Wizard" (verbs step).
func vendVerbOptions() []vendVerbOption {
	core := map[string]bool{}
	for _, v := range mcp.DrainVerbs() {
		core[v] = true
	}
	var opts []vendVerbOption
	for _, v := range mcp.AllVerbs() {
		opts = append(opts, vendVerbOption{Name: v, Checked: core[v]})
	}
	return opts
}

// vendSourceTypeOptions builds the webhooks step's source-type chips from mcp.WebhookSourceTypes —
// the types create_webhook actually accepts — checking the ones already in the draft. Deriving
// them from the server's own list means the wizard can neither offer a type the server refuses
// nor hide one it accepts. Governing: SPEC-0015 REQ "Endpoints View And Vend Wizard" (webhooks
// step), SPEC-0006 REQ "Webhook Self-Management Within a Vended Ceiling".
func vendSourceTypeOptions(chosen []string) []vendChipOption {
	var opts []vendChipOption
	for _, s := range mcp.WebhookSourceTypes() {
		opts = append(opts, vendChipOption{Name: s, Checked: slices.Contains(chosen, s)})
	}
	return opts
}

// endpointCard is the render model for one Endpoints-view card (fragments/endpoints.html
// "endpoint_card"): principal, persona, scope chips, the endpoint's MCP URL, the non-secret
// credential display prefix (never a reusable credential), expiry countdown data, and the
// last-seen / killed-at stamps. Governing: SPEC-0015 REQ "Endpoints View And Vend Wizard".
type endpointCard struct {
	ID          string
	AgentName   string
	Initials    string // two-letter avatar tile derived from the agent name (design canvas)
	Principal   string // the accountable human the endpoint is vended under (SPEC-0007)
	PersonaName string
	Slug        string
	URL         string // the endpoint's /mcp/{slug} URL — the capability's address (SPEC-0014)
	CredPrefix  string
	Queues      []string
	Verbs       []string
	State       string // active | revoked
	RevokedAt   *time.Time
	LastSeenAt  *time.Time
	// ExpiresAt is the vend-time credential lifetime the card renders as a countdown chip plus
	// machine-readable data (data-sb-expires-at); nil = valid until revoked. Governing: SPEC-0016
	// REQ "Credential Lifetime".
	ExpiresAt *time.Time
}

// cardInitials derives the endpoint card's two-letter avatar tile from the agent name: the first
// two letters/digits, upper-cased (the design canvas renders name.slice(0,2) upper-cased; skipping
// punctuation keeps hyphenated names like "-bot" legible). Empty names fall back to the endpoint
// glyph "EP" so the tile never renders blank.
func cardInitials(name string) string {
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
		return "EP"
	}
	return string(out)
}

// revealView feeds the one-time credential reveal (fragments/endpoints.html "vend_reveal"): the
// minted URL, the plaintext credential shown exactly once, and the ready-to-paste HTTP .mcp.json
// wiring — in two variants: bearer-credential wiring, and the URL-only wiring an OAuth-capable
// client uses to discover authorization itself (SPEC-0016). It is produced only as the response to
// a successful vend POST and is never persisted, so it cannot be re-rendered from any card or
// later page. Governing: SPEC-0015 REQ "Endpoints View And Vend Wizard" (one-time reveal +
// .mcp.json; URL-only variant), SPEC-0014 REQ "HTTP Wiring Is the Only Wiring".
type revealView struct {
	AgentName      string
	Slug           string
	URL            string // the minted /mcp/{slug} endpoint URL, shown as its own labeled field
	Token          string // plaintext credential — shown once, never stored
	MCPJSON        string
	MCPJSONURLOnly string // wiring with no embedded credential, for OAuth-capable clients (SPEC-0016)
	Queues         []string
	Verbs          []string
	ExpiresAt      *time.Time // vend-time expiry echoed on the reveal (nil = until revoked)
	CSRF           string
}

// cardFromStore projects a store.EndpointCard into the template render model. The persona name is
// dropped unless personas are enabled, so a card can never surface a persona the operator UI does
// not otherwise expose. The principal is the viewing human — cards are strictly owner-scoped by
// the store query (SPEC-0007 REQ "Human as Accountable Principal").
func (h *Handler) cardFromStore(c store.EndpointCard, principal string) endpointCard {
	card := endpointCard{
		ID:         c.ID,
		AgentName:  c.AgentName,
		Initials:   cardInitials(c.AgentName),
		Principal:  principal,
		Slug:       c.Slug,
		URL:        mcpEndpointURL(h.cfg.BaseURL, c.Slug),
		CredPrefix: c.CredentialPrefix,
		Queues:     c.ScopeQueues,
		Verbs:      c.ScopeVerbs,
		State:      c.State,
		RevokedAt:  c.RevokedAt,
		LastSeenAt: c.LastSeenAt,
		ExpiresAt:  c.ExpiresAt,
	}
	if h.personasEnabled {
		card.PersonaName = c.PersonaName
	}
	return card
}

// vendPersonaOption is one choice in the wizard's persona select: the persona id (the submitted
// value that binds endpoints.persona_id) and its display name. Only the vending human's personas
// are offered, so a vend can never reference another operator's persona.
type vendPersonaOption struct {
	ID   string
	Name string
}

// vendPersonaOptions lists the human's personas as persona-step choices, newest first. It is only
// consulted while personas are enabled; an empty slice renders the step with no bindable persona.
// Governing: SPEC-0015 REQ "Endpoints View And Vend Wizard" (persona step), ADR-0009.
func (h *Handler) vendPersonaOptions(r *http.Request, humanID string) []vendPersonaOption {
	if !h.personasEnabled {
		return nil
	}
	personas, err := h.store.ListPersonas(r.Context(), humanID)
	if err != nil {
		// Suppressed to a log: the step still renders (with no persona choices) and a reload
		// recovers. A missing persona list must never block vending an agent-level endpoint.
		h.log.Warn("vend persona options", "err", err)
		return nil
	}
	opts := make([]vendPersonaOption, 0, len(personas))
	for _, p := range personas {
		opts = append(opts, vendPersonaOption{ID: p.ID, Name: p.Name})
	}
	return opts
}

// vendQueueOptions lists the queue names already known to the store as the queues step's toggle
// chips (design canvas: queues are toggled, not typed). A read failure degrades to no chips — the
// step always renders its free-text add-queues field so vending is never blocked — rather than
// failing the page. Governing: SPEC-0015 REQ "Endpoints View And Vend Wizard" (queues step).
func (h *Handler) vendQueueOptions(r *http.Request) []string {
	queues, err := h.store.KnownQueues(r.Context())
	if err != nil {
		h.log.Warn("vend queue options", "err", err)
		return nil
	}
	return queues
}

// endpointCards loads and projects the human's cards for the Endpoints page (shared by the view
// and the post-vend reveal render). A read failure degrades to no cards — the page still renders
// its shell + empty state; a reload recovers.
func (h *Handler) endpointCards(r *http.Request, human *store.Human) []endpointCard {
	eps, err := h.store.ListEndpointCards(r.Context(), human.ID)
	if err != nil {
		h.log.Warn("endpoints list", "err", err)
		return nil
	}
	cards := make([]endpointCard, 0, len(eps))
	for _, ep := range eps {
		cards = append(cards, h.cardFromStore(ep, human.DisplayName))
	}
	return cards
}

// Endpoints renders the Endpoints view: the vended-endpoint cards plus the "+ Vend endpoint"
// wizard launcher. Requires human. Governing: SPEC-0015 REQ "Endpoints View And Vend Wizard".
func (h *Handler) Endpoints(w http.ResponseWriter, r *http.Request) {
	human, _ := auth.FromContext(r.Context())
	sh, _ := h.buildShell(r.Context(), "endpoints", &human)
	var cards []endpointCard
	if sh.DBConnected {
		cards = h.endpointCards(r, &human)
	}
	h.render(w, "endpoints", view{
		Title: "Endpoints", Human: &human, CSRF: auth.CSRFFromContext(r.Context()),
		Shell: sh, EndpointCards: cards, PersonasEnabled: h.personasEnabled,
	})
}

// vendSubmission is one validated-enough vend request: the wizard confirm step builds it from the
// server-side draft, and the direct form POST builds it from its own fields. executeVend is the
// single mint path for both.
type vendSubmission struct {
	Name      string
	PersonaID string
	Queues    []string
	Verbs     []string
	Lifetime  string // parseLifetime vocabulary; "" = valid until revoked
	// Webhook ceiling (ADR-0012, SPEC-0006). WebhookMax=0 disables self-managed webhooks.
	WebhookMax         int
	WebhookSourceTypes []string
	WebhookQueues      []string
}

// Vend mints a scoped endpoint from a direct form POST (the wizard's confirm step submits through
// VendStepSubmit, which calls executeVend with the server-side draft). It refuses (400, mints
// nothing) unless a name, at least one queue, and at least one verb are supplied — the validation
// runs BEFORE any store write, so a rejected submission leaves no agent and no endpoint. Requires
// human. Governing: SPEC-0015 REQ "Endpoints View And Vend Wizard", SPEC-0007 (hash at rest,
// immutable scope), SPEC-0014 (HTTP wiring reveal).
func (h *Handler) Vend(w http.ResponseWriter, r *http.Request) {
	human, _ := auth.FromContext(r.Context())
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	// The persona binding is only honored while personas are enabled — a submitted persona is
	// ignored (never bound) when the capability is off, mirroring the wizard which renders no
	// persona slot then.
	var personaID string
	if h.personasEnabled {
		personaID = strings.TrimSpace(r.FormValue("persona"))
	}
	// Webhook ceiling fields from the direct POST path. When absent the defaults (max=0, nil
	// source types, nil queues) disable webhook self-management — the same as the wizard when the
	// operator skips the webhooks step.
	webhookMax := 0
	if v := strings.TrimSpace(r.FormValue("webhook_max")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			webhookMax = n
		}
	}
	h.executeVendOn(w, r, &human, vendSubmission{
		Name:               strings.TrimSpace(r.FormValue("name")),
		PersonaID:          personaID,
		Queues:             multiValues(r, "queues"),
		Verbs:              multiValues(r, "verbs"),
		Lifetime:           strings.TrimSpace(r.FormValue("lifetime")),
		WebhookMax:         webhookMax,
		WebhookSourceTypes: multiValues(r, "webhook_source_types"),
		WebhookQueues:      multiValues(r, "webhook_queues"),
	}, "endpoints")
}

// executeVend is the one mint path: it validates the submission (scope gate + lifetime BEFORE any
// store write), creates the agent + endpoint in one transaction, and renders the one-time reveal
// inline on the Endpoints page. The plaintext appears exactly once — a later GET /endpoints has no
// token to render. Returns true only when an endpoint was minted (the wizard drops its draft on
// that answer alone, so a failed confirm never destroys the operator's entered state).
// Governing: SPEC-0007 (scope validation, hash at rest), SPEC-0016 REQ "Credential Lifetime",
// SPEC-0014 (HTTP wiring reveal).
// executeVendOn is executeVend with the caller choosing the page the mint renders on: the
// Endpoints view (the wizard and the direct POST) or the one-step quick-vend page.
func (h *Handler) executeVendOn(w http.ResponseWriter, r *http.Request, human *store.Human, sub vendSubmission, renderOn string) bool {
	// Scope validation is a hard gate: no name, no queue, or no verb → reject before minting anything.
	if sub.Name == "" || len(sub.Queues) == 0 || len(sub.Verbs) == 0 {
		http.Error(w, "name, at least one queue, and at least one verb are required", http.StatusBadRequest)
		return false
	}
	// Optional credential lifetime. Empty = no expiry, valid until revoked. A malformed or
	// non-positive lifetime is rejected BEFORE minting anything, like the scope gate above.
	// Governing: SPEC-0016 REQ "Credential Lifetime", ADR-0019.
	var expiresAt *time.Time
	if sub.Lifetime != "" {
		d, err := parseLifetime(sub.Lifetime)
		if err != nil {
			http.Error(w, "invalid lifetime", http.StatusBadRequest)
			return false
		}
		exp := time.Now().Add(d).UTC()
		expiresAt = &exp
	}

	token, hash, prefix, err := cred.Mint()
	if err != nil {
		h.fail(w, err)
		return false
	}
	// Governing: SPEC-0014 REQ "Streamable HTTP MCP Endpoint" — the per-endpoint URL slug is minted
	// and persisted at vend time; /mcp/{slug} is where this credential is honored. The slug is a
	// non-secret URL segment, so deriving it from the submitted name is fine even when a persona
	// reuses a differently named backing agent.
	slug, err := store.MintSlug(sub.Name)
	if err != nil {
		h.fail(w, err)
		return false
	}
	// Agent + endpoint are minted in one transaction, so a failure after the agent insert can never
	// leave an orphan agent. When a persona is bound the endpoint is vended on the persona's own
	// agent (owner + same-agent validated in the store); an unknown or foreign persona comes back
	// as ErrNotFound → a 400 that mints nothing.
	res, err := h.store.VendAgentEndpoint(r.Context(), store.VendParams{
		OwnerHumanID: human.ID, Name: sub.Name, PersonaID: sub.PersonaID,
		CredHash: hash, CredPrefix: prefix, Slug: slug, Queues: sub.Queues, Verbs: sub.Verbs,
		ExpiresAt: expiresAt, WebhookMax: sub.WebhookMax,
		WebhookSourceTypes: sub.WebhookSourceTypes, WebhookQueues: sub.WebhookQueues,
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "unknown persona", http.StatusBadRequest)
			return false
		}
		h.fail(w, err)
		return false
	}
	ep := res.Endpoint

	reveal := revealView{
		AgentName: res.AgentName, Slug: ep.Slug, Token: token,
		URL:            mcpEndpointURL(h.cfg.BaseURL, ep.Slug),
		MCPJSON:        buildMCPJSON(h.cfg.BaseURL, ep.Slug, token),
		MCPJSONURLOnly: buildMCPJSONURLOnly(h.cfg.BaseURL, ep.Slug),
		Queues:         ep.ScopeQueues, Verbs: ep.ScopeVerbs, ExpiresAt: ep.ExpiresAt,
		CSRF: auth.CSRFFromContext(r.Context()),
	}
	// The reveal renders inline at the top of the Endpoints page — a full server-rendered page, so
	// the wizard completes identically with JS disabled (SPEC-0015 REQ "Wizard Interaction
	// Pattern" scenario "JavaScript disabled").
	sh, _ := h.buildShell(r.Context(), "endpoints", human)
	var cards []endpointCard
	if sh.DBConnected {
		cards = h.endpointCards(r, human)
	}
	h.render(w, renderOn, view{
		Title: "Endpoint vended", Human: human, CSRF: auth.CSRFFromContext(r.Context()),
		Shell: sh, EndpointCards: cards, PersonasEnabled: h.personasEnabled,
		Reveal: &reveal,
	})
	return true
}

// revokeConfirmView feeds the revoke confirm page (templates/revoke.html): the card being revoked,
// so the operator sees exactly which capability dies before confirming.
type revokeConfirmView struct {
	Card endpointCard
}

// RevokeConfirm renders the full-page confirmation for revoking an endpoint — revocation is
// irreversible (the credential dies and the URL unroutes), so it confirms before executing, per
// the wizard interaction pattern. An unknown, foreign, or already-revoked endpoint 404s without
// leaking which of those it was. Requires human. Governing: SPEC-0015 REQ "Wizard Interaction
// Pattern" (destructive and irreversible steps confirm), SPEC-0007 REQ "Instant, Total
// Revocation".
func (h *Handler) RevokeConfirm(w http.ResponseWriter, r *http.Request) {
	human, _ := auth.FromContext(r.Context())
	id := chi.URLParam(r, "id")
	eps, err := h.store.ListEndpointCards(r.Context(), human.ID)
	if err != nil {
		h.fail(w, err)
		return
	}
	for _, ep := range eps {
		if ep.ID == id && ep.State == "active" {
			sh, _ := h.buildShell(r.Context(), "endpoints", &human)
			h.render(w, "revoke", view{
				Title: "Revoke endpoint", Human: &human, CSRF: auth.CSRFFromContext(r.Context()),
				Shell: sh, PersonasEnabled: h.personasEnabled,
				RevokeConfirm: &revokeConfirmView{Card: h.cardFromStore(ep, human.DisplayName)},
			})
			return
		}
	}
	http.Error(w, "not found", http.StatusNotFound)
}

// parseLifetime parses the vend wizard's optional credential lifetime into a duration: a Go
// duration string ("90m", "1h", "24h") or a whole-day/week shorthand ("7d", "4w") for the wizard's
// preset vocabulary, which time.ParseDuration alone does not speak. Zero and negative lifetimes
// are rejected — an endpoint can never be vended already expired. Governing: SPEC-0016 REQ
// "Credential Lifetime" (lifetime chosen at vend time).
func parseLifetime(s string) (time.Duration, error) {
	var d time.Duration
	var err error
	if n, ok := strings.CutSuffix(s, "d"); ok {
		d, err = daysToDuration(n, 24*time.Hour)
	} else if n, ok := strings.CutSuffix(s, "w"); ok {
		d, err = daysToDuration(n, 7*24*time.Hour)
	} else {
		d, err = time.ParseDuration(s)
	}
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, errors.New("lifetime must be positive")
	}
	return d, nil
}

// daysToDuration converts a whole-number count of a coarse unit (day, week) into a duration.
func daysToDuration(n string, unit time.Duration) (time.Duration, error) {
	count, err := strconv.Atoi(strings.TrimSpace(n))
	if err != nil {
		return 0, err
	}
	return time.Duration(count) * unit, nil
}

// multiValues collects a repeated form field (checkbox chips submit one value each) OR a single
// comma-separated value (the no-JS queue text input), trimming blanks. It underpins the vend
// form's queue/verb collection so both the enhanced chip UI and the plain form degrade to the same
// scope.
func multiValues(r *http.Request, field string) []string {
	var out []string
	for _, v := range r.Form[field] {
		out = append(out, splitCSV(v)...)
	}
	return out
}
