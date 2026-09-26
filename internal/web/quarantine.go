package web

// The owner's Quarantine view (ADR-0031, SPEC-0026 REQ-9): the held deliveries on the signed-in
// human's endpoints, each with its reason and detail, the actor and trust flags, the source, kind and
// receiving webhook, the escaped title, and the payload collapsed and shown as inert text. Three
// actions, each a CSRF-carrying POST: release (optionally to a named queue), discard (with a
// reason), and trust this actor and release.
//
// Everything a sender controls (title, payload, actor names, fault detail) reaches the page only
// through html/template's contextual escaping, and the payload is never rendered as HTML or
// Markdown: it is a <pre> text node. The view carries the same secureHeaders set, and its CSP forbids
// inline script, as every other page.
//
// This layer implements no release or trust rules of its own. Release and discard go through the
// intake service (ingest.ReleaseQuarantined / DiscardQuarantined), which reroutes the delivery
// through the webhook's current rules and applies the result under a row lock. Every lookup is
// scoped to the human: another human's item, an unknown id and a malformed id all answer 404, the
// same as SPEC-0026 "Tenancy" requires of every tenant surface.
//
// Actions answer an HTMX request with the refreshed panel (the notice and the list, announced
// through aria-live="polite"), and a plain form POST with a 303 to the Quarantine view. The redirect
// target is a constant path plus a notice code from a fixed table, never anything the request
// supplied (SPEC-0026 "Redirect Validation").
//
// Governing: ADR-0031, SPEC-0026 REQ-9 "Quarantine View and Owner Signals", REQ-7 "Release and
// Discard", REQ-5 "Trusted Actors", "Security Headers", "CSRF Protection", "Redirect Validation",
// "Tenancy"; ADR-0018 (charm-web design language).
//
// @joestump-agent 09/25/2026 - Added for #387.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/stump-wtf/switchboard/internal/auth"
	"github.com/stump-wtf/switchboard/internal/ingest"
	"github.com/stump-wtf/switchboard/internal/routing"
	"github.com/stump-wtf/switchboard/internal/store"
)

// Input limits for the Quarantine actions (#387 security checklist).
const (
	quarantineReasonMax = 500 // discard reason, in characters
	quarantineQueueMax  = 128 // a named release queue, in bytes
	quarantinePath      = "/quarantine"
)

// QuarantineService is what the view needs from intake: release a held delivery through routing,
// and discard one. *ingest.Ingest satisfies it; the server wires it with SetQuarantineService.
type QuarantineService interface {
	ReleaseQuarantined(ctx context.Context, ownerHumanID, todoID, by, queue string) ([]store.CreatedTodo, error)
	DiscardQuarantined(ctx context.Context, ownerHumanID, todoID, by, reason string) (store.Todo, error)
}

// SetQuarantineService wires the intake service the Quarantine actions call. Until it is set, every
// action answers "unavailable" and changes nothing (fail closed).
func (h *Handler) SetQuarantineService(q QuarantineService) { h.quarantine = q }

// quarantineNotice is the one-line outcome the panel announces after an action.
type quarantineNotice struct {
	Code string
	Kind string // ok | warn | danger
	Text string
}

// quarantineNotices is the fixed table of outcomes. A redirect carries only a key from it, and the
// page renders only its fixed text, so nothing from a request is ever reflected into the page.
var quarantineNotices = map[string]quarantineNotice{
	"released":            {Kind: "ok", Text: "released · routed through the webhook's rules"},
	"released_queue":      {Kind: "ok", Text: "released to the named queue"},
	"trusted":             {Kind: "ok", Text: "actor trusted · delivery released"},
	"discarded":           {Kind: "ok", Text: "discarded"},
	"conflict":            {Kind: "warn", Text: "conflict · the item was not released: it is no longer held, or the webhook's rules send it back to quarantine, fault on it, or drop it. Name a queue to release it, or discard it"},
	"trusted_conflict":    {Kind: "warn", Text: "actor trusted, but the release was refused (conflict): the item is no longer held, or the webhook's rules send it back to quarantine, fault on it, or drop it. Name a queue to release it, or discard it"},
	"trusted_unavailable": {Kind: "warn", Text: "actor trusted, but routing is unavailable right now, so the delivery was not released and is still held. Release it again shortly, or discard it"},
	"trusted_error":       {Kind: "danger", Text: "actor trusted, but the release failed; the delivery may still be held. Reload, then release or discard it"},
	"queue_refused":       {Kind: "danger", Text: "that queue is not one this webhook may route to"},
	"queue_invalid":       {Kind: "danger", Text: "a queue name is at most 128 characters, with no spaces"},
	"reason_required":     {Kind: "danger", Text: "discarding needs a reason of 1–500 characters"},
	"trust_refused":       {Kind: "danger", Text: "trust this actor applies only to deliveries held as untrusted_actor that name an actor; release or discard this one instead"},
	"trust_full":          {Kind: "danger", Text: "the webhook's trust list cannot take this actor (it is full, or the name is too long)"},
	"unavailable":         {Kind: "danger", Text: "routing is unavailable right now; nothing was changed. Try again shortly"},
}

func noticeFor(code string) *quarantineNotice {
	n, ok := quarantineNotices[code]
	if !ok {
		return nil
	}
	n.Code = code
	return &n
}

// quarantineRedirect is the only place an action redirects: the Quarantine view, with a notice key.
func quarantineRedirect(code string) string {
	if _, ok := quarantineNotices[code]; !ok {
		return quarantinePath
	}
	return quarantinePath + "?n=" + code
}

// quarantineItemView is one held delivery as the view shows it.
type quarantineItemView struct {
	ID          string
	ShortID     string
	Reason      string // untrusted_actor | rule_fault | rule_action
	ReasonLabel string
	Source      string
	Kind        string
	Title       string
	TrustMode   string
	WebhookID   string
	Endpoint    string // the receiving endpoint's agent name
	ReceivedAt  time.Time

	// Actor verdict (untrusted_actor, and any held delivery from a source with an actor projection).
	HasActor      bool
	Sender        string
	Author        string
	SenderTrusted string // "trusted" | "untrusted" | "" (not evaluated)
	AuthorTrusted string

	// Fault (rule_fault) and the quarantining rule (rule_action).
	FaultCause  string
	FaultDetail string
	RuleID      string

	Payload    string   // pretty-printed, rendered as escaped text only
	CanTrust   bool     // trust-this-actor is offered: untrusted_actor, and the action would add someone
	TrustNames []string // exactly who the trust action adds under the webhook's match mode
}

// TrustFor is who the trust action adds, for its visible text and accessible label.
func (v quarantineItemView) TrustFor() string { return strings.Join(v.TrustNames, " and ") }

// quarantinePanelView feeds the "quarantine_panel" fragment: the notice and the item list.
type quarantinePanelView struct {
	Items  []quarantineItemView
	Notice *quarantineNotice
	CSRF   string
	Error  bool // the list could not be read (degraded; a reload recovers)
}

// quarantineDetailView mirrors ingest's stored quarantine_detail JSON.
type quarantineDetailView struct {
	Actor  *routing.ActorTrust `json:"actor"`
	Fault  *routing.RuleFault  `json:"fault"`
	RuleID string              `json:"rule_id"`
}

var quarantineReasonLabels = map[string]string{
	routing.QuarantineUntrustedActor: "untrusted actor",
	routing.QuarantineRuleFault:      "rule fault",
	routing.QuarantineRuleAction:     "held by a rule",
}

func trustLabel(b *bool) string {
	switch {
	case b == nil:
		return ""
	case *b:
		return "trusted"
	default:
		return "untrusted"
	}
}

// quarantineItemFrom projects a held item for display. agents maps endpoint id → agent name.
func quarantineItemFrom(it store.QuarantinedItem, agents map[string]string) quarantineItemView {
	t := it.Todo
	v := quarantineItemView{
		ID: t.ID, ShortID: shortID(t.ID), Reason: t.QuarantineReason,
		ReasonLabel: quarantineReasonLabels[t.QuarantineReason],
		Source:      t.Source, Kind: it.Event.EventType, Title: t.Title,
		TrustMode: it.Event.TrustMode, WebhookID: it.Event.WebhookID,
		Endpoint: agents[t.EndpointID], ReceivedAt: t.CreatedAt,
	}
	if v.ReasonLabel == "" {
		v.ReasonLabel = t.QuarantineReason
	}
	if v.Source == "" {
		v.Source = it.Event.Provider
	}
	if v.Kind == "" {
		v.Kind = t.Kind
	}
	if !it.Event.ReceivedAt.IsZero() {
		v.ReceivedAt = it.Event.ReceivedAt
	}
	var d quarantineDetailView
	_ = json.Unmarshal(t.QuarantineDetail, &d) // an unreadable detail shows no actor or fault
	if a := d.Actor; a != nil {
		v.HasActor = true
		if a.Sender != nil {
			v.Sender = *a.Sender
		}
		if a.Author != nil {
			v.Author = *a.Author
		}
		v.SenderTrusted, v.AuthorTrusted = trustLabel(a.SenderTrusted), trustLabel(a.AuthorTrusted)
	}
	if f := d.Fault; f != nil {
		v.FaultCause, v.FaultDetail, v.RuleID = f.Cause, f.Detail, f.RuleID
	}
	if d.RuleID != "" {
		v.RuleID = d.RuleID
	}
	// Offer trust only when the action would add someone, and name exactly who, under the webhook's
	// match mode: the same trustNames the action itself runs. A missing webhook offers nothing.
	if v.Reason == routing.QuarantineUntrustedActor && d.Actor != nil && it.Trust != nil {
		v.TrustNames = trustNames(it.Trust.SourceType, it.Trust.TrustedActors, d.Actor)
		v.CanTrust = len(v.TrustNames) > 0
	}
	payload := it.Event.Payload
	if len(payload) == 0 {
		payload = t.Payload
	}
	v.Payload = prettyJSON(payload)
	return v
}

// quarantinePanel reads the human's held items and builds the panel.
func (h *Handler) quarantinePanel(ctx context.Context, human *store.Human, csrf string, notice *quarantineNotice) quarantinePanelView {
	p := quarantinePanelView{CSRF: csrf, Notice: notice}
	items, err := h.store.ListQuarantinedForHuman(ctx, human.ID, store.QuarantineListCap)
	if err != nil {
		h.log.Warn("quarantine list", "err", err)
		p.Error = true
		return p
	}
	agents := map[string]string{}
	if len(items) > 0 {
		cards, err := h.store.ListEndpointCards(ctx, human.ID)
		if err != nil {
			h.log.Warn("quarantine endpoint names", "err", err)
		}
		for _, c := range cards {
			agents[c.ID] = c.AgentName
		}
	}
	p.Items = make([]quarantineItemView, 0, len(items))
	for _, it := range items {
		p.Items = append(p.Items, quarantineItemFrom(it, agents))
	}
	return p
}

// Quarantine renders the view. GET /quarantine[?n=<notice>]. Requires human.
func (h *Handler) Quarantine(w http.ResponseWriter, r *http.Request) {
	human, _ := auth.FromContext(r.Context())
	csrf := auth.CSRFFromContext(r.Context())
	sh, _ := h.buildShell(r.Context(), "quarantine", &human)
	// With the database down the list is unknown, not empty: say so rather than "nothing held".
	panel := quarantinePanelView{CSRF: csrf, Notice: noticeFor(r.URL.Query().Get("n")), Error: !sh.DBConnected}
	if sh.DBConnected {
		panel = h.quarantinePanel(r.Context(), &human, csrf, panel.Notice)
	}
	h.render(w, "quarantine", view{
		Title: "Quarantine", Human: &human, CSRF: csrf, Shell: sh, Quarantine: &panel,
	})
}

// ReleaseQuarantined releases a held delivery: through the webhook's rules, or, when the form names
// a queue, straight to that queue (which must be within the owner's webhook-queue ceiling).
// POST /quarantine/{id}/release. Governing: SPEC-0026 REQ-7, REQ-9.
func (h *Handler) ReleaseQuarantined(w http.ResponseWriter, r *http.Request) {
	human, _ := auth.FromContext(r.Context())
	id := chi.URLParam(r, "id")
	queue := strings.TrimSpace(r.PostFormValue("queue"))
	if queue != "" && (len(queue) > quarantineQueueMax || strings.ContainsFunc(queue, isSpaceOrControl)) {
		h.quarantineRespond(w, r, &human, "queue_invalid")
		return
	}
	if h.quarantine == nil {
		h.quarantineRespond(w, r, &human, "unavailable")
		return
	}
	_, err := h.quarantine.ReleaseQuarantined(r.Context(), human.ID, id, "human:"+human.ID, queue)
	if err == nil {
		code := "released"
		if queue != "" {
			code = "released_queue"
		}
		h.quarantineRespond(w, r, &human, code)
		return
	}
	h.quarantineFail(w, r, &human, id, err)
}

// DiscardQuarantined completes a held delivery with the human's reason. POST /quarantine/{id}/discard.
// Governing: SPEC-0026 REQ-7, REQ-9.
func (h *Handler) DiscardQuarantined(w http.ResponseWriter, r *http.Request) {
	human, _ := auth.FromContext(r.Context())
	id := chi.URLParam(r, "id")
	reason := strings.TrimSpace(r.PostFormValue("reason"))
	if reason == "" || utf8.RuneCountInString(reason) > quarantineReasonMax || !utf8.ValidString(reason) {
		h.quarantineRespond(w, r, &human, "reason_required")
		return
	}
	if h.quarantine == nil {
		h.quarantineRespond(w, r, &human, "unavailable")
		return
	}
	if _, err := h.quarantine.DiscardQuarantined(r.Context(), human.ID, id, "human:"+human.ID, reason); err != nil {
		h.quarantineFail(w, r, &human, id, err)
		return
	}
	h.quarantineRespond(w, r, &human, "discarded")
}

// TrustQuarantinedActor adds the held delivery's actor to its webhook's trusted_actors, then
// releases it through routing. It is refused for anything but an untrusted_actor item that names an
// actor, and so always for rule_fault items. Which names are added follows the webhook's match
// mode: the sender (sender, the default), the author (author), or both (both); cairn adds the
// signed actor_id. POST /quarantine/{id}/trust. Governing: SPEC-0026 REQ-9 "Trust this actor", REQ-5.
func (h *Handler) TrustQuarantinedActor(w http.ResponseWriter, r *http.Request) {
	human, _ := auth.FromContext(r.Context())
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	if h.quarantine == nil {
		h.quarantineRespond(w, r, &human, "unavailable")
		return
	}
	item, err := h.store.QuarantinedForHuman(ctx, human.ID, id)
	if err != nil {
		h.quarantineFail(w, r, &human, id, err)
		return
	}
	var d quarantineDetailView
	_ = json.Unmarshal(item.Todo.QuarantineDetail, &d)
	if item.Todo.QuarantineReason != routing.QuarantineUntrustedActor || d.Actor == nil || item.Event.WebhookID == "" {
		h.quarantineRespond(w, r, &human, "trust_refused")
		return
	}
	// The webhook is read through the held todo's endpoint, which is the webhook's owner endpoint
	// (SPEC-0026 REQ-6), so the trust list changed is always one this human owns.
	wh, err := h.store.WebhookForEndpoint(ctx, item.Event.WebhookID, item.Todo.EndpointID)
	if errors.Is(err, store.ErrNotFound) {
		h.quarantineRespond(w, r, &human, "conflict") // the webhook is gone: discard instead
		return
	}
	if err != nil {
		h.quarantineFail(w, r, &human, id, err)
		return
	}
	names := trustNames(wh.SourceType, wh.TrustedActors, d.Actor)
	if len(names) == 0 {
		h.quarantineRespond(w, r, &human, "trust_refused")
		return
	}
	if _, _, err := h.store.AddWebhookTrustedActors(ctx, wh.ID, wh.EndpointID, names); err != nil {
		if errors.Is(err, store.ErrTrustListRefused) {
			h.quarantineRespond(w, r, &human, "trust_full")
			return
		}
		h.quarantineFail(w, r, &human, id, err)
		return
	}
	h.log.Info("quarantine: actor trusted", "webhook", wh.ID, "todo", id, "human", human.ID)
	// The trust list is committed now. Every release outcome from here on must say so: a notice that
	// claims nothing changed would leave the human believing the actor is still untrusted.
	if _, err := h.quarantine.ReleaseQuarantined(ctx, human.ID, id, "human:"+human.ID, ""); err != nil {
		code := trustedReleaseFailure(err)
		if code == "trusted_error" {
			h.log.Error("quarantine: release after trust", "todo", id, "err", err)
		}
		h.quarantineRespond(w, r, &human, code)
		return
	}
	h.quarantineRespond(w, r, &human, "trusted")
}

// trustedReleaseFailure maps a release error that followed a committed trust-list write onto a
// notice that says the actor was trusted. The item was read as the human's own held item a moment
// earlier, so not-found here is a lost race (released or discarded meanwhile), not a foreign id.
func trustedReleaseFailure(err error) string {
	switch {
	case errors.Is(err, ingest.ErrRoutingUnavailable):
		return "trusted_unavailable"
	case errors.Is(err, ingest.ErrReleaseConflict), errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrNotFound):
		return "trusted_conflict"
	default:
		return "trusted_error"
	}
}

// trustNames is who trusting a held delivery's actor adds, under the webhook's match mode. Both the
// view (the button's label, and whether it is offered) and the action call it, so they cannot
// disagree about who gets trusted.
func trustNames(sourceType string, trustedActors []byte, a *routing.ActorTrust) []string {
	var sender, author string
	if a.Sender != nil {
		sender = *a.Sender
	}
	if a.Author != nil {
		author = *a.Author
	}
	if sourceType == routing.SourceCairn {
		return nonEmpty(sender)
	}
	ta, _ := routing.DecodeTrustedActors(sourceType, trustedActors)
	switch ta.Match {
	case routing.MatchAuthor:
		if author == "" {
			return nonEmpty(sender)
		}
		return nonEmpty(author)
	case routing.MatchBoth:
		return nonEmpty(sender, author)
	default:
		return nonEmpty(sender)
	}
}

func nonEmpty(names ...string) []string {
	var out []string
	for _, n := range names {
		if n != "" {
			out = append(out, n)
		}
	}
	return out
}

func isSpaceOrControl(r rune) bool { return r <= ' ' || r == 0x7f }

// quarantineFail maps an action's error onto the response. Not found stays not found (404) unless
// the human owns the todo and it simply is no longer held, which is a lost race: that is a conflict,
// shown on the refreshed list. Internal detail never reaches the page.
func (h *Handler) quarantineFail(w http.ResponseWriter, r *http.Request, human *store.Human, id string, err error) {
	switch {
	case errors.Is(err, ingest.ErrQueueNotGranted):
		h.quarantineRespond(w, r, human, "queue_refused")
	case errors.Is(err, ingest.ErrRoutingUnavailable):
		h.quarantineRespond(w, r, human, "unavailable")
	case errors.Is(err, ingest.ErrReleaseConflict), errors.Is(err, store.ErrConflict):
		h.quarantineRespond(w, r, human, "conflict")
	case errors.Is(err, store.ErrNotFound):
		if _, gerr := h.store.GetTodoOperatorOwned(r.Context(), human.ID, id); gerr == nil {
			h.quarantineRespond(w, r, human, "conflict") // the human's todo, released or discarded meanwhile
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	default:
		h.log.Error("quarantine action", "todo", id, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// quarantineRespond answers an action: the refreshed panel for HTMX, or a 303 to the view.
func (h *Handler) quarantineRespond(w http.ResponseWriter, r *http.Request, human *store.Human, code string) {
	if !isHTMX(r) {
		http.Redirect(w, r, quarantineRedirect(code), http.StatusSeeOther)
		return
	}
	panel := h.quarantinePanel(r.Context(), human, auth.CSRFFromContext(r.Context()), noticeFor(code))
	frag, err := h.renderFragment("quarantine_panel", panel)
	if err != nil {
		h.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(frag))
}

// quarantineCount is the rail badge: the human's open quarantine items. A read failure hides it.
func (h *Handler) quarantineCount(ctx context.Context, humanID string) int {
	n, err := h.store.CountQuarantinedForHuman(ctx, humanID)
	if err != nil {
		h.log.Warn("shell quarantine count", "err", err)
		return 0
	}
	return n
}
