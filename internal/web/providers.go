package web

// Operator Providers view: connected providers grouped by family (webhook · push / queue · pull)
// with per-line glyph, kind, trust chip, in-rate, last-seen, enabled state, and lifecycle controls
// (disable / re-enable / rotate / remove with confirmation), plus the honest provider catalog —
// implemented-but-unconnected kinds read as connectable, unimplemented kinds (SQS/NATS/AMQP) read
// as available-only with NO functional connect path.
//
// Governing: SPEC-0017 REQ "Providers View" (secrets never render — only configured/missing), REQ
// "Provider Catalog" (available ≠ connectable; the UI never fakes a backend), REQ "Provider
// Lifecycle" (removal keeps every ingested event and todo); ADR-0020 (runtime registry), ADR-0018
// (charm-web design language). The connect wizard itself is a separate story (#36) — until it
// lands, connectable catalog cards state that plainly instead of offering a dead-end entry point.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/joestump/switchboard/internal/auth"
	"github.com/joestump/switchboard/internal/store"
)

// providerLine is the render model for one CONNECTED provider (a registry row joined to its
// ingest-side health). It carries no secret material by construction: store.Adapter has no secret
// field, and SecretStatus is derived presence classification only.
// Governing: SPEC-0017 REQ "Providers View" (scenario "Trust at a glance").
type providerLine struct {
	Name      string
	Kind      string // concrete implementation, falling back to the name for pre-registry rows
	Family    string // webhook | queue
	TrustMode string // signed | token | open | queue (SPEC-0001 vocabulary)
	Tag       string // two-letter glyph (providerTag)
	Enabled   bool
	// SecretStatus is the ONLY secret trace that ever renders: configured | missing |
	// none-by-design (open/queue kinds hold no secret on purpose).
	SecretStatus string
	Queue        string     // target todo queue (non-secret config)
	Path         string     // webhook ingestion path; "" for queue-family lines
	InRate       int        // accepted deliveries in the trailing minute
	LastSeenAt   *time.Time // newest accepted delivery; nil = never
	LastPollAt   *time.Time // queue family: last poll-loop attempt
	LastError    string     // queue family: credential-free error text of the last failed poll
	CanRotate    bool       // token/signed webhook kinds only (SPEC-0017 REQ "Provider Lifecycle")
	RowID        string     // stable DOM id sb-pr-<name>
}

// providerFamily is one family section of the view: webhook · push, then queue · pull.
type providerFamily struct {
	Key   string // webhook | queue
	Title string
	Lines []providerLine
}

// catalogCard is one Provider Catalog entry. State separates the two honest catalog tiers:
// "connectable" (implemented, wizard #36 will connect it) vs "available" (SQS/NATS/AMQP — planned,
// visually distinct, NO connect path). Governing: SPEC-0017 REQ "Provider Catalog" (scenario
// "Catalog honesty").
type catalogCard struct {
	Kind   string // github | stripe | slack | generic | redis | sqs | nats | amqp
	Title  string
	Family string // webhook | queue
	Trust  string // the trust mode connecting would enforce (chip vocabulary)
	Desc   string // what connecting means / will mean
	State  string // connectable | available
}

// providersPanelView feeds the "providers_panel" fragment: the family sections, the catalog split
// into its two tiers, and the CSRF token for the lifecycle forms.
type providersPanelView struct {
	Families    []providerFamily
	Connectable []catalogCard
	Available   []catalogCard
	CSRF        string
	Total       int // connected-line count (the view-head aside)
}

// providerConfirmView feeds the shared lifecycle confirmation modal (disable / rotate / remove).
// Governing: SPEC-0017 REQ "Provider Lifecycle" (removal requires confirmation; the modal copy
// states exactly what each action does — and what it never does).
type providerConfirmView struct {
	Action string // disable | rotate | remove — the POST target segment
	Name   string
	Title  string
	Body   string // consequence copy, stated plainly (docs/design/05-voice.md)
	Button string
	CSRF   string
}

// providerRevealView feeds the post-rotate one-time secret reveal: the NEW shared secret shown
// exactly once, never persisted in plaintext, never re-renderable from any later page — the same
// discipline as the vend credential reveal. Governing: SPEC-0017 (secrets through the envelope,
// revealed once); ADR-0020.
type providerRevealView struct {
	Name      string
	TrustMode string
	Secret    string // plaintext, shown once
	Path      string // the ingestion path the sender presents the secret to
	CSRF      string
}

// providerFamilyOrder fixes the section order: push lines first, then pull — the design canvas
// order (webhook · push above queue · pull), independent of the store's alphabetical family sort.
var providerFamilyOrder = []struct{ Key, Title string }{
	{"webhook", "webhook · push"},
	{"queue", "queue · pull"},
}

// implementedCatalog enumerates every kind the backend actually implements today — the source of
// the "connectable" catalog tier. Single-instance signed kinds drop out of the catalog once
// connected; generic and redis stay connectable (many instances make sense).
var implementedCatalog = []catalogCard{
	{Kind: "github", Title: "GitHub", Family: "webhook", Trust: "signed", Desc: "push/PR/issue webhooks · HMAC-verified per delivery", State: "connectable"},
	{Kind: "stripe", Title: "Stripe", Family: "webhook", Trust: "signed", Desc: "billing events · signature-verified per delivery", State: "connectable"},
	{Kind: "slack", Title: "Slack", Family: "webhook", Trust: "signed", Desc: "workspace events · signature-verified per delivery", State: "connectable"},
	{Kind: "generic", Title: "Generic webhook", Family: "webhook", Trust: "token", Desc: "any homelab sender · shared-secret token (or explicit open)", State: "connectable"},
	{Kind: "redis", Title: "Redis", Family: "queue", Trust: "queue", Desc: "drain a stream, list, or pub/sub channel into a todo queue", State: "connectable"},
}

// availableCatalog enumerates the not-yet-implemented kinds — the "available" tier. These cards
// describe what connecting WILL mean and expose no connect path at all: no form, no wizard entry,
// nothing that could dead-end. Governing: SPEC-0017 REQ "Provider Catalog" (the UI never fakes a
// backend that does not exist; scenario "Catalog honesty").
var availableCatalog = []catalogCard{
	{Kind: "sqs", Title: "Amazon SQS", Family: "queue", Trust: "queue", Desc: "will drain an SQS queue into todos · planned, not yet implemented", State: "available"},
	{Kind: "nats", Title: "NATS", Family: "queue", Trust: "queue", Desc: "will drain a NATS subject into todos · planned, not yet implemented", State: "available"},
	{Kind: "amqp", Title: "AMQP", Family: "queue", Trust: "queue", Desc: "will drain an AMQP queue into todos · planned, not yet implemented", State: "available"},
}

// singleInstanceKinds are the signed webhook kinds where one connected provider saturates the kind
// (their route is /webhooks/<kind>); their catalog card drops once connected.
var singleInstanceKinds = map[string]bool{"github": true, "stripe": true, "slack": true}

// providerLineFrom projects one registry row + its health onto the render model.
func providerLineFrom(a store.Adapter, health map[string]store.ProviderHealth) providerLine {
	kind := a.Kind
	if kind == "" {
		kind = a.Name // pre-registry rows (runner-registered pull adapters) carry no kind
	}
	line := providerLine{
		Name:       a.Name,
		Kind:       kind,
		Family:     a.Family,
		TrustMode:  a.TrustMode,
		Tag:        providerTag(kind),
		Enabled:    a.Enabled,
		Queue:      providerQueueFromConfig(a.Config, a.Name),
		RowID:      "sb-pr-" + a.Name,
		LastPollAt: a.LastPollAt,
	}
	if a.LastError != nil {
		line.LastError = *a.LastError
	}
	if h, ok := health[a.Name]; ok {
		line.InRate = h.EventsPerMin
		line.LastSeenAt = h.LastSeenAt
	}
	switch a.Family {
	case "webhook":
		switch a.TrustMode {
		case "signed":
			line.Path = "/webhooks/" + a.Name
		case "token", "open":
			line.Path = "/webhooks/generic/" + a.Name
		}
		switch a.TrustMode {
		case "signed", "token":
			line.CanRotate = true
			if a.SecretConfigured {
				line.SecretStatus = "configured"
			} else {
				line.SecretStatus = "missing"
			}
		default: // open holds no secret on purpose
			line.SecretStatus = "none-by-design"
		}
	default: // queue — broker DSN lives in env, never the registry
		line.SecretStatus = "none-by-design"
	}
	return line
}

// providerFamiliesFrom buckets lines into the canonical family sections, dropping empty ones (the
// whole-view empty state covers "none at all").
func providerFamiliesFrom(lines []providerLine) []providerFamily {
	byKey := map[string][]providerLine{}
	for _, l := range lines {
		byKey[l.Family] = append(byKey[l.Family], l)
	}
	var families []providerFamily
	for _, f := range providerFamilyOrder {
		if len(byKey[f.Key]) == 0 {
			continue
		}
		families = append(families, providerFamily{Key: f.Key, Title: f.Title, Lines: byKey[f.Key]})
	}
	return families
}

// connectableCatalog computes the connectable tier against what is already connected: a
// single-instance signed kind disappears from the catalog once its line exists; generic and redis
// remain connectable regardless.
func connectableCatalog(lines []providerLine) []catalogCard {
	connected := map[string]bool{}
	for _, l := range lines {
		connected[l.Kind] = true
	}
	var out []catalogCard
	for _, c := range implementedCatalog {
		if singleInstanceKinds[c.Kind] && connected[c.Kind] {
			continue
		}
		out = append(out, c)
	}
	return out
}

// providersPanel assembles the full panel render model from the registry + health reads.
func providersPanel(adapters []store.Adapter, health map[string]store.ProviderHealth, csrf string) providersPanelView {
	lines := make([]providerLine, 0, len(adapters))
	for _, a := range adapters {
		lines = append(lines, providerLineFrom(a, health))
	}
	return providersPanelView{
		Families:    providerFamiliesFrom(lines),
		Connectable: connectableCatalog(lines),
		Available:   availableCatalog,
		CSRF:        csrf,
		Total:       len(lines),
	}
}

// loadProvidersPanel reads the registry + per-source health and builds the panel. A health-read
// failure degrades to idle stats (logged) rather than failing the page; a registry-read failure is
// returned — the view is ABOUT the registry, rendering without it would misrepresent state.
func (h *Handler) loadProvidersPanel(r *http.Request) (providersPanelView, error) {
	adapters, err := h.store.ListProviders(r.Context())
	if err != nil {
		return providersPanelView{}, err
	}
	health, err := h.store.ProviderHealthBySource(r.Context())
	if err != nil {
		h.log.Warn("providers health", "err", err)
		health = nil
	}
	return providersPanel(adapters, health, auth.CSRFFromContext(r.Context())), nil
}

// Providers renders the Providers view: family sections with trust chips and health over the
// runtime registry, lifecycle controls per line, and the two-tier catalog. HTMX GETs receive just
// the panel fragment. Requires human. Governing: SPEC-0017 REQ "Providers View", REQ "Provider
// Catalog"; SPEC-0015 REQ "Application Shell And Navigation" (sixth IA entry).
func (h *Handler) Providers(w http.ResponseWriter, r *http.Request) {
	human, _ := auth.FromContext(r.Context())
	sh, _ := h.buildShell(r.Context(), "providers", &human)
	var panel providersPanelView
	if sh.DBConnected {
		p, err := h.loadProvidersPanel(r)
		if err != nil {
			// Degraded render: the shell shows the disconnect; the panel shows its empty state.
			h.log.Warn("providers list", "err", err)
		} else {
			panel = p
		}
	}
	if panel.Available == nil {
		// The catalog renders even with no database: it is static honesty, not registry state.
		panel.Available = availableCatalog
		panel.Connectable = connectableCatalog(nil)
		panel.CSRF = auth.CSRFFromContext(r.Context())
	}
	if isHTMX(r) {
		frag, err := h.renderFragment("providers_panel", panel)
		if err != nil {
			h.fail(w, err)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(frag))
		return
	}
	h.render(w, "providers", view{
		Title: "Providers", Human: &human, CSRF: auth.CSRFFromContext(r.Context()), Shell: sh,
		Providers: &panel,
	})
}

// providerConfirmCopy builds the confirmation modal copy per action. The copy states consequences
// plainly (docs/design/05-voice.md): what stops, what keeps working, and — for remove — that
// history is never deleted. Governing: SPEC-0017 REQ "Provider Lifecycle".
func providerConfirmCopy(action, name string) (title, body, button string, ok bool) {
	switch action {
	case "disable":
		return "Disable " + name + "?",
			"Its ingestion stops accepting new deliveries immediately. Everything already ingested — events and todos — stays queryable. Re-enable restores the line as it was.",
			"Disable provider", true
	case "rotate":
		return "Rotate the secret for " + name + "?",
			"A new secret is generated and shown once. The old secret stops working immediately — update the sender before its next delivery, or those deliveries will be rejected.",
			"Rotate secret", true
	case "remove":
		return "Remove " + name + "?",
			"Its ingestion URL goes dead and the line disappears from this view. Previously ingested events and todos are never deleted — history stays queryable.",
			"Remove provider", true
	}
	return "", "", "", false
}

// ProviderConfirmModal serves the lifecycle confirmation modal (GET
// /providers/{name}/confirm/{action}) into the shared overlay slot. It verifies the provider
// exists (404 otherwise) and, for rotate, that the line's kind is rotatable — open and queue
// kinds hold no rotatable secret (409). Requires human.
// Governing: SPEC-0017 REQ "Provider Lifecycle" (confirmation before destructive actions).
func (h *Handler) ProviderConfirmModal(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	action := chi.URLParam(r, "action")
	title, body, button, ok := providerConfirmCopy(action, name)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	a, err := h.store.GetAdapter(r.Context(), name)
	if err != nil {
		h.notFoundOr(w, err)
		return
	}
	if action == "rotate" && !providerRotatable(a) {
		http.Error(w, "provider kind holds no rotatable secret", http.StatusConflict)
		return
	}
	confirm := providerConfirmView{
		Action: action, Name: name, Title: title, Body: body, Button: button,
		CSRF: auth.CSRFFromContext(r.Context()),
	}
	if !isHTMX(r) {
		// No-JS fallback: the confirm buttons are plain GET forms, so render the confirmation
		// inline on the full page instead of returning a bare overlay fragment.
		human, _ := auth.FromContext(r.Context())
		sh, _ := h.buildShell(r.Context(), "providers", &human)
		panel, err := h.loadProvidersPanel(r)
		if err != nil {
			h.fail(w, err)
			return
		}
		h.render(w, "providers", view{
			Title: "Providers", Human: &human, CSRF: confirm.CSRF, Shell: sh,
			Providers: &panel, ProviderConfirm: &confirm,
		})
		return
	}
	frag, err := h.renderFragment("provider_confirm_modal", confirm)
	if err != nil {
		h.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(frag))
}

// providerRotatable reports whether a line's kind carries an operator-rotatable secret: token or
// signed webhook kinds only (SPEC-0017 REQ "Provider Lifecycle"). Open holds none by design; a
// queue line's broker DSN lives in env, not the registry.
func providerRotatable(a store.Adapter) bool {
	return a.Family == "webhook" && (a.TrustMode == "signed" || a.TrustMode == "token")
}

// DisableProvider stops a provider's line (POST /providers/{name}/disable): its ingestion URL
// rejects new deliveries / its poll loop parks on the next cycle, while everything already
// ingested stays queryable. Requires human + CSRF. Governing: SPEC-0017 REQ "Provider Lifecycle"
// (scenario "Disable stops the line").
func (h *Handler) DisableProvider(w http.ResponseWriter, r *http.Request) {
	h.setProviderEnabled(w, r, false)
}

// EnableProvider restores a disabled line (POST /providers/{name}/enable). Requires human + CSRF.
// Governing: SPEC-0017 REQ "Provider Lifecycle" (re-enable).
func (h *Handler) EnableProvider(w http.ResponseWriter, r *http.Request) {
	h.setProviderEnabled(w, r, true)
}

func (h *Handler) setProviderEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	name := chi.URLParam(r, "name")
	if err := h.store.SetAdapterEnabled(r.Context(), name, enabled); err != nil {
		h.notFoundOr(w, err)
		return
	}
	verb := "disabled · new deliveries rejected · history kept"
	if enabled {
		verb = "re-enabled"
	}
	h.respondProviderAction(w, r, "provider "+name+" "+verb, true)
}

// RotateProvider rotates a token/signed webhook provider's secret (POST /providers/{name}/rotate):
// a fresh 256-bit secret is generated server-side, sealed through the envelope, and shown exactly
// once in the standard reveal pattern — the old secret is dead on the next request. Open and queue
// kinds are refused (409). Requires human + CSRF. Governing: SPEC-0017 REQ "Provider Lifecycle"
// (rotate for token/signed webhook kinds), ADR-0020 (secrets through the envelope, revealed once).
func (h *Handler) RotateProvider(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	a, err := h.store.GetAdapter(r.Context(), name)
	if err != nil {
		h.notFoundOr(w, err)
		return
	}
	if !providerRotatable(a) {
		http.Error(w, "provider kind holds no rotatable secret", http.StatusConflict)
		return
	}
	secret, err := mintProviderSecret()
	if err != nil {
		h.fail(w, err)
		return
	}
	if err := h.store.RotateProviderSecret(r.Context(), name, secret); err != nil {
		h.notFoundOr(w, err)
		return
	}
	line := providerLineFrom(a, nil)
	reveal := providerRevealView{
		Name: name, TrustMode: a.TrustMode, Secret: secret, Path: line.Path,
		CSRF: auth.CSRFFromContext(r.Context()),
	}
	if !isHTMX(r) {
		// No-JS fallback: render the page with the reveal inline — a redirect would lose the
		// one-time plaintext (mirrors the vend flow's inline reveal).
		human, _ := auth.FromContext(r.Context())
		sh, _ := h.buildShell(r.Context(), "providers", &human)
		panel, err := h.loadProvidersPanel(r)
		if err != nil {
			h.fail(w, err)
			return
		}
		h.render(w, "providers", view{
			Title: "Providers", Human: &human, CSRF: auth.CSRFFromContext(r.Context()), Shell: sh,
			Providers: &panel, ProviderReveal: &reveal,
		})
		return
	}
	frag, err := h.renderFragment("provider_rotate_reveal", reveal)
	if err != nil {
		h.fail(w, err)
		return
	}
	// The reveal replaces the confirm modal in the overlay; the panel refreshes OOB alongside it
	// so the line's updated stamp reflects immediately.
	if panel, err := h.loadProvidersPanel(r); err == nil {
		if pf, err := h.renderFragment("providers_panel_oob", panel); err == nil {
			frag += pf
		} else {
			h.log.Error("render providers panel oob", "err", err)
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(frag))
}

// RemoveProvider removes a provider's registry line (POST /providers/{name}/remove, behind the
// confirmation modal). The ingestion URL goes dead; previously ingested events and todos are NEVER
// deleted — the store's delete touches only the registry row. Requires human + CSRF.
// Governing: SPEC-0017 REQ "Provider Lifecycle" (removal requires confirmation and keeps history).
func (h *Handler) RemoveProvider(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if err := h.store.RemoveProvider(r.Context(), name); err != nil {
		h.notFoundOr(w, err)
		return
	}
	h.respondProviderAction(w, r, "provider "+name+" removed · ingested events and todos kept", true)
}

// respondProviderAction re-renders the panel (HTMX) with a toast, clearing the shared overlay when
// the action came from a confirmation modal; a non-HTMX submit redirects back to the view.
func (h *Handler) respondProviderAction(w http.ResponseWriter, r *http.Request, toast string, closeOverlay bool) {
	if !isHTMX(r) {
		// Post-action redirect targets a fixed same-origin path (no user-supplied target).
		http.Redirect(w, r, "/providers", http.StatusSeeOther)
		return
	}
	panel, err := h.loadProvidersPanel(r)
	if err != nil {
		h.fail(w, err)
		return
	}
	frag, err := h.renderFragment("providers_panel", panel)
	if err != nil {
		h.fail(w, err)
		return
	}
	if toast != "" {
		if t, err := h.renderFragment("toast", toast); err == nil {
			frag += t
		} else {
			h.log.Error("render provider toast", "err", err)
		}
	}
	if closeOverlay {
		if oc, err := h.renderFragment("overlay_clear", nil); err == nil {
			frag += oc
		} else {
			h.log.Error("render overlay clear", "err", err)
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(frag))
}

// mintProviderSecret generates a fresh shared secret for rotate: 32 random bytes, hex-encoded (64
// chars) — paste-safe for any sender config, no prefix (this is a webhook shared secret, not an
// sbk_ MCP credential). Never logged, never stored in plaintext.
func mintProviderSecret() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("mint provider secret: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// providerQueueFromConfig extracts the target todo queue from a registry row's non-secret config
// jsonb ({"queue":…} for webhooks; stream/list/channel for queue adapters), defaulting to the
// provider name — the same default the receivers apply. Kept independent of internal/ingest so the
// web layer needs no ingest import.
func providerQueueFromConfig(config []byte, name string) string {
	var c struct {
		Queue   string `json:"queue"`
		Stream  string `json:"stream"`
		List    string `json:"list"`
		Channel string `json:"channel"`
	}
	if len(config) > 0 {
		_ = json.Unmarshal(config, &c)
	}
	for _, v := range []string{c.Queue, c.Stream, c.List, c.Channel} {
		if v != "" {
			return v
		}
	}
	return name
}
