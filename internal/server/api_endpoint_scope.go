package server

// Human API: widen an endpoint's scope, additively
//
// POST /api/v1/endpoints/{ref}/scope/widen adds to an endpoint's grant without re-vending it: more
// allowed webhook queues, more allowed ingest source types, more verbs, a larger webhook ceiling. It
// is the only change to an endpoint's scope that exists, and it exists because the change it covers
// cannot break anything that works today — nothing is removed, and an existing route keeps working.
// Narrowing, replacing a verb list or swapping a queue stays a revoke and a re-vend (SPEC-0007), and
// a body naming anything of that kind is refused rather than ignored.
//
// A widen touches no credential: the secret shown once at vend stays valid, the ingest URL does not
// change, and nothing here reads or rewrites either. That is the whole point — a re-vend rotates the
// secret and forces an edit in the source system's hook configuration, which is why adding one lane to
// an existing routing setup cost a full re-mint (issue #590).
//
// Confirmation parity with vending: a widen is an explicit act by the operator who already controls the
// endpoint, so the call that does not say confirm shows the whole change — the scope as it is and as it
// would be — and applies nothing. Sending confirm applies exactly what the preview showed and writes
// the audit trail; every field that moved is recorded with its old and new value (ADR-0049).
//
// The route follows SPEC-0035: {ref} is a slug or id among the caller's own endpoints, another human's
// endpoint is the same 404 as one that does not exist, a write to a revoked endpoint is 409 with the
// state, and errors use {"error", "code"}.
//
// Governing: ADR-0049 (proposed), ADR-0008 (scope is fixed at vend), ADR-0012 (the webhook ceiling),
// SPEC-0006 REQ "Webhook Self-Management Within a Vended Ceiling", SPEC-0035 REQ "Reach on Every
// Route", ADR-0022 (tenant isolation).
//
// @joestump-agent 10/21/2026 - Added for stump.wtf/switchboard#590.

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/stump-wtf/switchboard/internal/mcp"
	"github.com/stump-wtf/switchboard/internal/store"
)

// widenScopeIn is the widen body. Every field is additive: there is no field that removes, replaces or
// narrows, and an unknown key (remove_queues, replace_verbs, webhook_queues) is refused at decode.
type widenScopeIn struct {
	AddWebhookQueues      []string `json:"add_webhook_queues"`
	AddWebhookSourceTypes []string `json:"add_webhook_source_types"`
	AddScopeVerbs         []string `json:"add_scope_verbs"`
	WebhookMax            *int     `json:"webhook_max"`
	Confirm               bool     `json:"confirm"`
}

// scopeOut is one view of the widen-reachable grant. scope_queues is shown because a webhook grant is
// computed from it, but a widen never touches it: changing which queues an endpoint may claim is not an
// additive change to a route.
type scopeOut struct {
	ScopeQueues        []string `json:"scope_queues"`
	ScopeVerbs         []string `json:"scope_verbs"`
	WebhookMax         int      `json:"webhook_max"`
	WebhookSourceTypes []string `json:"webhook_source_types"`
	WebhookQueues      []string `json:"webhook_queues"`
}

// wideningOut is one audit entry as the API reports it.
type wideningOut struct {
	Field     string `json:"field"`
	Before    any    `json:"before"`
	After     any    `json:"after"`
	CreatedAt string `json:"created_at,omitempty"`
}

// widenScopeOut is the widen response, applied or previewed.
type widenScopeOut struct {
	ID                   string        `json:"endpoint_id"`
	Slug                 string        `json:"slug"`
	AgentName            string        `json:"agent_name"`
	Applied              bool          `json:"applied"`
	RequiresConfirmation bool          `json:"requires_confirmation"`
	Changed              bool          `json:"changed"`
	Before               scopeOut      `json:"before"`
	After                scopeOut      `json:"after"`
	Widenings            []wideningOut `json:"widenings"`
}

func scopeOutFrom(sc store.EndpointScope) scopeOut {
	return scopeOut{
		ScopeQueues:        nonNilStrings(sc.ScopeQueues),
		ScopeVerbs:         nonNilStrings(sc.ScopeVerbs),
		WebhookMax:         sc.WebhookMax,
		WebhookSourceTypes: nonNilStrings(sc.WebhookSourceTypes),
		WebhookQueues:      nonNilStrings(sc.WebhookQueues),
	}
}

func wideningsOutFrom(entries []store.ScopeWidening) []wideningOut {
	out := make([]wideningOut, 0, len(entries))
	for _, e := range entries {
		w := wideningOut{Field: e.Field, Before: e.Before, After: e.After}
		if !e.CreatedAt.IsZero() {
			w.CreatedAt = e.CreatedAt.UTC().Format(time.RFC3339)
		}
		out = append(out, w)
	}
	return out
}

// WidenEndpointScope is POST /api/v1/endpoints/{ref}/scope/widen. Without "confirm": true it changes
// nothing and returns the proposed scope; with it, the additions are applied and audited.
func (a *apiHandler) WidenEndpointScope(w http.ResponseWriter, r *http.Request) {
	human, _ := operatorFromContext(r.Context())
	found, ok := a.ownedEndpoint(w, r, "widen endpoint scope")
	if !ok {
		return
	}

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var in widenScopeIn
	if err := dec.Decode(&in); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeAPIError(w, http.StatusRequestEntityTooLarge, "invalid_argument", "request body is too large", nil)
			return
		}
		writeAPIError(w, http.StatusBadRequest, "invalid_argument",
			"invalid JSON body: "+err.Error()+
				" (a widen may only add: add_webhook_queues, add_webhook_source_types, add_scope_verbs, webhook_max; "+
				"narrowing or replacing scope is a revoke and a re-vend)", nil)
		return
	}

	add, problem := a.scopeAdditions(in)
	if problem != "" {
		writeAPIError(w, http.StatusBadRequest, "invalid_argument", problem, nil)
		return
	}

	before, err := a.st.EndpointScopeForHuman(r.Context(), found.ID, human.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeAPIError(w, http.StatusNotFound, "not_found", "no endpoint by that name or id belongs to you", nil)
		return
	case err != nil:
		a.fail(w, "widen endpoint scope", err)
		return
	}

	after, err := store.WidenScope(before, add)
	switch {
	case errors.Is(err, store.ErrNothingToWiden):
		writeAPIError(w, http.StatusBadRequest, "invalid_argument",
			"nothing to widen: name at least one addition", nil)
		return
	case errors.Is(err, store.ErrScopeNarrowing):
		writeScopeRefusal(w, before)
		return
	case errors.Is(err, store.ErrReservedQueue):
		writeAPIError(w, http.StatusBadRequest, "invalid_argument", store.QueueQuarantine+
			" is reserved and cannot be granted to an endpoint", nil)
		return
	case err != nil:
		a.fail(w, "widen endpoint scope", err)
		return
	}

	out := widenScopeOut{
		ID: found.ID, Slug: found.Slug, AgentName: found.AgentName,
		Before:    scopeOutFrom(before),
		After:     scopeOutFrom(after),
		Widenings: wideningsOutFrom(store.ScopeWidenings(before, after)),
		Changed:   len(store.ScopeWidenings(before, after)) > 0,
	}
	if !in.Confirm {
		out.RequiresConfirmation = true
		writeJSON(w, http.StatusOK, out)
		return
	}

	res, err := a.st.WidenEndpointScopeForHuman(r.Context(), found.ID, human.ID, add)
	switch {
	case errors.Is(err, store.ErrConflict):
		writeAPIError(w, http.StatusConflict, "conflict", "endpoint is "+found.State+"; its scope can no longer change",
			map[string]any{"state": found.State, "slug": found.Slug})
		return
	case errors.Is(err, store.ErrNotFound):
		writeAPIError(w, http.StatusNotFound, "not_found", "no endpoint by that name or id belongs to you", nil)
		return
	case errors.Is(err, store.ErrScopeNarrowing):
		writeScopeRefusal(w, before)
		return
	case errors.Is(err, store.ErrNothingToWiden):
		writeAPIError(w, http.StatusBadRequest, "invalid_argument", "nothing to widen: name at least one addition", nil)
		return
	case errors.Is(err, store.ErrReservedQueue):
		writeAPIError(w, http.StatusBadRequest, "invalid_argument", store.QueueQuarantine+
			" is reserved and cannot be granted to an endpoint", nil)
		return
	case err != nil:
		a.fail(w, "widen endpoint scope", err)
		return
	}

	out.Applied = true
	out.Changed = len(res.Widened) > 0
	out.Before = scopeOutFrom(res.Before)
	out.After = scopeOutFrom(res.After)
	out.Widenings = wideningsOutFrom(res.Widened)
	// The widen is the one time an endpoint's grant changes after it was minted, so the log names the
	// fields that moved and both values of each: an operator asking "when did this endpoint start
	// routing to merge?" must be able to get the answer from the server's own records.
	a.log.Info("api endpoint scope widened", "human", human.ID, "slug", found.Slug, "endpoint", found.ID,
		"fields", strings.Join(fieldsOf(res.Widened), ","), "before", scopeOutFrom(res.Before), "after", scopeOutFrom(res.After))
	writeJSON(w, http.StatusOK, out)
}

// ListEndpointScopeWidenings is GET /api/v1/endpoints/{ref}/scope/widenings: the endpoint's widenings,
// oldest first — who widened which field, from what to what, and when.
func (a *apiHandler) ListEndpointScopeWidenings(w http.ResponseWriter, r *http.Request) {
	human, _ := operatorFromContext(r.Context())
	found, ok := a.ownedEndpoint(w, r, "list endpoint scope widenings")
	if !ok {
		return
	}
	entries, err := a.st.ListEndpointScopeWidenings(r.Context(), found.ID, human.ID)
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "not_found", "no endpoint by that name or id belongs to you", nil)
		return
	}
	if err != nil {
		a.fail(w, "list endpoint scope widenings", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"endpoint_id": found.ID,
		"slug":        found.Slug,
		"agent_name":  found.AgentName,
		"widenings":   wideningsOutFrom(entries),
	})
}

// scopeAdditions turns the body into store additions, resolving verbs the way a vend resolves them
// (presets included, so the two CLIs stay coherent — ADR-0047) and refusing an unknown ingest source
// type before anything is written. It returns the refusal text, empty when the additions are sound.
func (a *apiHandler) scopeAdditions(in widenScopeIn) (store.ScopeAdditions, string) {
	add := store.ScopeAdditions{
		WebhookQueues:      trimNames(in.AddWebhookQueues),
		WebhookSourceTypes: trimNames(in.AddWebhookSourceTypes),
	}
	if len(in.AddScopeVerbs) > 0 {
		grant, err := mcp.VendVerbGrant(in.AddScopeVerbs)
		if err != nil {
			return add, "add_scope_verbs: " + err.Error()
		}
		add.WebhookVerbs = grant
	}
	if in.WebhookMax != nil {
		if *in.WebhookMax < 0 {
			return add, "webhook_max must be 0 or more; a widen never lowers it"
		}
		add.WebhookMax = in.WebhookMax
	}
	for _, s := range add.WebhookSourceTypes {
		if !slices.Contains(mcp.WebhookSourceTypes(), s) {
			return add, "unknown source type " + strconv.Quote(s) +
				" (valid: " + strings.Join(mcp.WebhookSourceTypes(), ", ") + ")"
		}
	}
	for _, q := range add.WebhookQueues {
		if q == "" {
			return add, "add_webhook_queues must not contain an empty queue name"
		}
	}
	if add.Empty() {
		return add, "nothing to widen: name at least one addition"
	}
	return add, ""
}

// writeScopeRefusal answers a widen that would lower the webhook ceiling: the widening refuses, and it
// says why in the number that made it refuse.
func writeScopeRefusal(w http.ResponseWriter, before store.EndpointScope) {
	writeAPIError(w, http.StatusBadRequest, "invalid_argument",
		"webhook_max is a widening only: an endpoint already allows "+
			strconv.Itoa(before.WebhookMax)+
			" webhooks, and a widen never lowers it — narrowing scope is a revoke and a re-vend", nil)
}

func fieldsOf(entries []store.ScopeWidening) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Field)
	}
	return out
}

func trimNames(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" {
			out = append(out, n)
		}
	}
	return out
}
