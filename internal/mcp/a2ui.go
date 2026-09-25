package mcp

// This file implements the A2UI-over-MCP resource surface (#102): two read-only resources that
// render the todo queue and individual todo detail as application/a2ui+json payloads, so an
// A2UI-capable host renders them inline instead of forcing the agent to re-render raw JSON.
//
// Resources:
//   - switchboard://queue/{name}/a2ui — pending todos in one queue as a Card list with state badges
//   - switchboard://todo/{id}/a2ui    — one todo's full detail, attempt history and action context
//
// Both are read-only at the A2UI layer; the existing claim/complete/fail tools remain the only
// mutation path. Per-credential scoping is identical to the underlying tools: the surface only
// ever shows what the authorizing endpoint can already see.
//
// Governing: #102 Epic "A2UI resources over MCP", ADR-0005 (contract shape), SPEC-0006 REQ
// "Todo Drain Verbs" (the queue these surfaces render), SPEC-0034 REQ-13 (attempt history).
//
// @joestump-agent 09/25/2026 - The todo detail lists the todo's attempts, for #329 (epic #313).

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stump-wtf/switchboard/internal/store"
)

const (
	// a2uiMIMEType is the MIME type an A2UI-capable host detects and routes to its renderer.
	a2uiMIMEType = "application/a2ui+json"

	// a2uiVersion is the A2UI protocol version these payloads target.
	a2uiVersion = "v0.9"

	// a2uiQueueLimit caps the number of todos rendered in the queue view.
	a2uiQueueLimit = 25

	// a2uiAttemptLimit caps the attempts the todo detail lists: get_todo's default page
	// (SPEC-0034 REQ-8), the same number the Board drawer shows.
	a2uiAttemptLimit = 20
)

// a2uiAttempts is the attempt history the todo detail renders: newest first, with the todo's
// attempts_total so the view can say how many it left out.
type a2uiAttempts struct {
	Rows  []store.Attempt
	Total int
}

// --- A2UI payload types (flat adjacency-list components) ---

// a2uiComponent is one node in the flat component list. The renderer resolves the tree from the
// root (the component nothing else references as a child).
type a2uiComponent struct {
	Component string   `json:"component"`
	ID        string   `json:"id"`
	Text      string   `json:"text,omitempty"`
	Variant   string   `json:"variant,omitempty"`
	Child     string   `json:"child,omitempty"`
	Children  []string `json:"children,omitempty"`
}

// a2uiPayload is a single updateComponents message against a fresh surface.
type a2uiPayload struct {
	Version          string `json:"version"`
	UpdateComponents struct {
		SurfaceID  string          `json:"surfaceId"`
		Components []a2uiComponent `json:"components"`
	} `json:"updateComponents"`
}

// registerA2UIResources installs the two A2UI resource templates on the per-session server. They
// are gated on list_todos scope: an endpoint that cannot read the queue must not read these
// surfaces either, matching the read-parity contract the event-history resource established.
// Governing: #102, SPEC-0006 REQ "Scope Enforcement at the Boundary".
func (h *Handler) registerA2UIResources(srv *sdk.Server, ep store.AuthEndpoint) {
	// ADR-0023: the A2UI surface is an advanced capability, hidden unless the operator flag is on.
	if !h.a2uiOn() {
		return
	}
	if !hasScope(ep.ScopeVerbs, "list_todos") {
		return
	}
	srv.AddResourceTemplate(&sdk.ResourceTemplate{
		URITemplate: "switchboard://queue/{queue}/a2ui{?w}",
		Name:        "queue_a2ui",
		Description: "A2UI view of pending todos in one queue. Read-only; use claim/complete/fail tools to mutate.",
		MIMEType:    a2uiMIMEType,
		Annotations: &sdk.Annotations{Audience: []sdk.Role{"user"}},
	}, h.queueA2UIResource(ep))

	srv.AddResourceTemplate(&sdk.ResourceTemplate{
		URITemplate: "switchboard://todo/{id}/a2ui{?w}",
		Name:        "todo_a2ui",
		Description: "A2UI detail view of a single todo. Read-only; use claim/complete/fail tools to mutate.",
		MIMEType:    a2uiMIMEType,
		Annotations: &sdk.Annotations{Audience: []sdk.Role{"user"}},
	}, h.todoA2UIResource(ep))
}

// queueA2UIResource serves switchboard://queue/{queue}/a2ui: the endpoint's pending todos in the
// named queue, rendered as a Column of Cards. The queue must be in the endpoint's granted scope;
// an out-of-scope queue is a stable forbidden error, identical to the list_todos tool.
func (h *Handler) queueA2UIResource(ep store.AuthEndpoint) sdk.ResourceHandler {
	return func(ctx context.Context, req *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
		queue := uriSegment(req.Params.URI, "queue")
		if queue == "" || !hasScope(ep.ScopeQueues, queue) {
			return nil, &toolError{codeForbidden, "queue not in this endpoint's scope"}
		}
		todos, err := h.store.ListTodos(ctx, ep.ID, []string{queue}, "pending", a2uiQueueLimit)
		if err != nil {
			return nil, h.mapStoreErr(ep, "resources/read queue/a2ui", err)
		}
		payload := renderQueueA2UI(queue, todos)
		body, err := json.Marshal(payload)
		if err != nil {
			h.log.Error("mcp queue a2ui marshal", "slug", ep.Slug, "err", err)
			return nil, &toolError{codeInternal, "internal error"}
		}
		return &sdk.ReadResourceResult{Contents: []*sdk.ResourceContents{{
			URI:      req.Params.URI,
			MIMEType: a2uiMIMEType,
			Text:     string(body),
		}}}, nil
	}
}

// todoA2UIResource serves switchboard://todo/{id}/a2ui: a single todo rendered as a Card with
// full detail and a row of action buttons (claim/complete/fail — rendered as labels, not wired
// to a2ui_action until crush#221 proves the round-trip). A todo outside the endpoint's tenant
// scope is not_found, identical to the get/claim tools.
func (h *Handler) todoA2UIResource(ep store.AuthEndpoint) sdk.ResourceHandler {
	return func(ctx context.Context, req *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
		id := uriSegment(req.Params.URI, "todo")
		if id == "" {
			return nil, &toolError{codeNotFound, "todo id is required"}
		}
		t, err := h.store.GetTodo(ctx, ep.ID, id)
		if err != nil {
			return nil, h.mapStoreErr(ep, "resources/read todo/a2ui", err)
		}
		if !hasScope(ep.ScopeQueues, t.Queue) {
			return nil, &toolError{codeForbidden, "todo's queue not in this endpoint's scope"}
		}
		// The attempt history comes through the same endpoint-scoped read get_todo uses, so the
		// surface shows nothing the endpoint could not already read. Governing: SPEC-0034 REQ-10,
		// REQ-13 "Operator Surfaces" (the A2UI todo detail lists the attempts).
		atts, total, _, err := h.store.TodoAttempts(ctx, ep.ID, id, a2uiAttemptLimit)
		if err != nil {
			return nil, h.mapStoreErr(ep, "resources/read todo/a2ui attempts", err)
		}
		payload := renderTodoA2UI(t, a2uiAttempts{Rows: atts, Total: total})
		body, err := json.Marshal(payload)
		if err != nil {
			h.log.Error("mcp todo a2ui marshal", "slug", ep.Slug, "err", err)
			return nil, &toolError{codeInternal, "internal error"}
		}
		return &sdk.ReadResourceResult{Contents: []*sdk.ResourceContents{{
			URI:      req.Params.URI,
			MIMEType: a2uiMIMEType,
			Text:     string(body),
		}}}, nil
	}
}

// --- renderers (pure functions, testable without a store) ---

// renderQueueA2UI builds the A2UI payload for the queue view: a header, a count, and a Card per
// pending todo showing id, title, state badge, and age. An empty queue renders a placeholder
// message rather than an empty list.
func renderQueueA2UI(queue string, todos []store.Todo) a2uiPayload {
	var p a2uiPayload
	p.Version = a2uiVersion
	p.UpdateComponents.SurfaceID = "queue-" + sanitizeID(queue)

	// The Column at comps[1] owns every section; each branch below appends only the children it
	// actually emits, so no reference dangles (an unresolved child renders as a missing-component
	// error in A2UI hosts).
	comps := []a2uiComponent{
		{Component: "Card", ID: "root", Child: "col"},
		{Component: "Column", ID: "col", Children: []string{"title"}},
		{Component: "Text", ID: "title", Variant: "h2", Text: "Queue: " + queue},
	}

	if len(todos) == 0 {
		comps[1].Children = append(comps[1].Children, "empty")
		comps = append(comps, a2uiComponent{Component: "Text", ID: "empty", Text: "No pending todos."})
		p.UpdateComponents.Components = comps
		return p
	}

	comps[1].Children = append(comps[1].Children, "count", "list")
	comps = append(comps, a2uiComponent{Component: "Text", ID: "count", Text: fmt.Sprintf("%d pending", len(todos))})

	listChildren := make([]string, 0, len(todos))
	for i, t := range todos {
		cardID := fmt.Sprintf("todo-card-%d", i)
		colID := fmt.Sprintf("todo-col-%d", i)
		titleID := fmt.Sprintf("todo-title-%d", i)
		metaID := fmt.Sprintf("todo-meta-%d", i)

		listChildren = append(listChildren, cardID)
		comps = append(comps,
			a2uiComponent{Component: "Card", ID: cardID, Child: colID},
			a2uiComponent{Component: "Column", ID: colID, Children: []string{titleID, metaID}},
			a2uiComponent{Component: "Text", ID: titleID, Text: todoTitle(t)},
			a2uiComponent{Component: "Text", ID: metaID, Variant: "caption", Text: todoMeta(t)},
		)
	}
	comps = append(comps, a2uiComponent{Component: "List", ID: "list", Children: listChildren})

	p.UpdateComponents.Components = comps
	return p
}

// renderTodoA2UI builds the A2UI payload for the detail view: a single Card with the todo's
// title, full metadata, payload summary, attempt history, and a row of action buttons. The
// buttons are label-only for now (claim/complete/fail) — wiring them to a2ui_action handlers is
// parked until crush#221.
func renderTodoA2UI(t store.Todo, hist a2uiAttempts) a2uiPayload {
	var p a2uiPayload
	p.Version = a2uiVersion
	p.UpdateComponents.SurfaceID = "todo-" + sanitizeID(t.ID)

	var payloadSummary string
	if len(t.Payload) > 0 {
		var v any
		if err := json.Unmarshal(t.Payload, &v); err == nil {
			b, _ := json.MarshalIndent(v, "", "  ")
			payloadSummary = string(b)
		} else {
			payloadSummary = string(t.Payload)
		}
	}

	colChildren := []string{"title", "state", "divider1", "meta", "divider2", "actions"}
	comps := []a2uiComponent{
		{Component: "Card", ID: "root", Child: "col"},
		{Component: "Column", ID: "col", Children: colChildren},
		{Component: "Text", ID: "title", Variant: "h2", Text: todoTitle(t)},
		{Component: "Text", ID: "state", Variant: "caption", Text: fmt.Sprintf("State: %s", t.State)},
		{Component: "Divider", ID: "divider1"},
		{Component: "Text", ID: "meta", Variant: "caption", Text: todoDetail(t)},
		{Component: "Divider", ID: "divider2"},
	}

	if payloadSummary != "" {
		// Insert payload section before the actions.
		comps[1].Children = insertBefore(comps[1].Children, "actions", "divider3", "payload")
		comps = append(comps,
			a2uiComponent{Component: "Divider", ID: "divider3"},
			a2uiComponent{Component: "Text", ID: "payload", Variant: "caption", Text: "Payload:\n" + payloadSummary},
		)
	}

	// Attempt history, before the actions like the payload.
	comps[1].Children = insertBefore(comps[1].Children, "actions", "divider-attempts", "attempts")
	comps = append(comps, a2uiComponent{Component: "Divider", ID: "divider-attempts"})
	comps = append(comps, renderAttemptsA2UI(hist)...)

	// Action buttons — label-only, parked until crush#221.
	comps = append(comps,
		a2uiComponent{Component: "Row", ID: "actions", Children: []string{"btn-claim", "btn-complete", "btn-fail"}},
		a2uiComponent{Component: "Button", ID: "btn-claim", Child: "btn-claim-label"},
		a2uiComponent{Component: "Text", ID: "btn-claim-label", Text: "Claim"},
		a2uiComponent{Component: "Button", ID: "btn-complete", Child: "btn-complete-label"},
		a2uiComponent{Component: "Text", ID: "btn-complete-label", Text: "Complete"},
		a2uiComponent{Component: "Button", ID: "btn-fail", Child: "btn-fail-label"},
		a2uiComponent{Component: "Text", ID: "btn-fail-label", Text: "Fail"},
	)

	p.UpdateComponents.Components = comps
	return p
}

// renderAttemptsA2UI renders a todo's attempt history as the "attempts" Column: a heading, then a
// Card per attempt, newest first, with the same facts as the Board drawer (number, claimant,
// times, outcome and disposition, a died marker with the last heartbeat, summary, artifact). A2UI
// has no link component, so every artifact, https or mcp://cairn, is text. store.Attempt carries
// no session, claimer endpoint or lease-token hash, so none can appear here.
// Governing: SPEC-0034 REQ-4, REQ-13 "Operator Surfaces".
func renderAttemptsA2UI(hist a2uiAttempts) []a2uiComponent {
	col := a2uiComponent{Component: "Column", ID: "attempts", Children: []string{"attempts-title"}}
	comps := []a2uiComponent{{Component: "Text", ID: "attempts-title", Variant: "h3",
		Text: fmt.Sprintf("Attempts (%d)", hist.Total)}}
	if len(hist.Rows) == 0 {
		col.Children = append(col.Children, "attempts-empty")
		comps = append(comps, a2uiComponent{Component: "Text", ID: "attempts-empty", Variant: "caption", Text: "No attempts yet."})
		return append([]a2uiComponent{col}, comps...)
	}
	list := a2uiComponent{Component: "List", ID: "attempts-list"}
	col.Children = append(col.Children, "attempts-list")
	for i, a := range hist.Rows {
		cardID, colID := fmt.Sprintf("attempt-card-%d", i), fmt.Sprintf("attempt-col-%d", i)
		headID, timesID := fmt.Sprintf("attempt-head-%d", i), fmt.Sprintf("attempt-times-%d", i)
		inner := a2uiComponent{Component: "Column", ID: colID, Children: []string{headID, timesID}}
		comps = append(comps,
			a2uiComponent{Component: "Card", ID: cardID, Child: colID},
			a2uiComponent{Component: "Text", ID: headID, Text: attemptHead(a)},
			a2uiComponent{Component: "Text", ID: timesID, Variant: "caption", Text: attemptTimes(a)},
		)
		if a.Summary != "" {
			id := fmt.Sprintf("attempt-summary-%d", i)
			text := "Summary: " + a.Summary
			if a.SummaryTruncated {
				text += " (truncated)"
			}
			inner.Children = append(inner.Children, id)
			comps = append(comps, a2uiComponent{Component: "Text", ID: id, Text: text})
		}
		if a.Artifact != "" {
			id := fmt.Sprintf("attempt-artifact-%d", i)
			inner.Children = append(inner.Children, id)
			comps = append(comps, a2uiComponent{Component: "Text", ID: id, Variant: "caption", Text: "Artifact: " + a.Artifact})
		}
		comps = append(comps, inner)
		list.Children = append(list.Children, cardID)
	}
	comps = append(comps, list)
	if hidden := hist.Total - len(hist.Rows); hidden > 0 {
		col.Children = append(col.Children, "attempts-more")
		comps = append(comps, a2uiComponent{Component: "Text", ID: "attempts-more", Variant: "caption",
			Text: fmt.Sprintf("%d earlier attempts not shown.", hidden)})
	}
	return append([]a2uiComponent{col}, comps...)
}

// attemptHead is an attempt's first line: number, who claimed, how it ended, and the died marker
// in words, so it never depends on how a host styles the text.
func attemptHead(a store.Attempt) string {
	who := a.Claimant
	if who == "" {
		who = "agent"
		if a.ClaimerKind == "owner" {
			who = "owner (Board)"
		}
	}
	parts := []string{fmt.Sprintf("#%d", a.Seq), who}
	switch {
	case a.Outcome == "":
		parts = append(parts, "in progress")
	case a.Disposition != "":
		parts = append(parts, a.Outcome+" → "+a.Disposition)
	default:
		parts = append(parts, a.Outcome)
	}
	if a.Died {
		parts = append(parts, "✝ died: no report")
	}
	return strings.Join(parts, " · ")
}

// attemptTimes is an attempt's time line: claimed, ended, and the last heartbeat (or, for a death,
// that there was none).
func attemptTimes(a store.Attempt) string {
	parts := []string{"claimed " + a.ClaimedAt.UTC().Format(time.RFC3339)}
	if a.EndedAt != nil {
		parts = append(parts, "ended "+a.EndedAt.UTC().Format(time.RFC3339))
	}
	switch {
	case a.LastHeartbeatAt != nil:
		parts = append(parts, "last heartbeat "+a.LastHeartbeatAt.UTC().Format(time.RFC3339))
	case a.Died:
		parts = append(parts, "no heartbeat")
	}
	return strings.Join(parts, " · ")
}

// --- helpers ---

// uriSegment extracts the path variable from a switchboard:// URI. For
// switchboard://queue/reviews/a2ui it returns "reviews". For
// switchboard://todo/td_123/a2ui it returns "td_123". The SDK's ResourceTemplate Matches()
// dispatches the call, but does not hand back the captured variables, so we parse the URI
// ourselves. The pathVar argument selects which shape to extract: "queue" matches
// queue/{value}/a2ui, "todo" matches todo/{value}/a2ui.
func uriSegment(uri, pathPrefix string) string {
	rest := strings.TrimPrefix(uri, "switchboard://")
	// A query string (an A2UI host's optional ?w= width hint) rides on the
	// /a2ui suffix; strip it before splitting so parts[2] stays "a2ui".
	rest, _, _ = strings.Cut(rest, "?")
	parts := strings.Split(rest, "/")
	if len(parts) != 3 {
		return ""
	}
	if parts[0] != pathPrefix {
		return ""
	}
	if parts[2] != "a2ui" {
		return ""
	}
	return parts[1]
}

// todoTitle returns the display title for a todo, falling back to the id.
func todoTitle(t store.Todo) string {
	if t.Title != "" {
		return t.Title
	}
	return t.ID
}

// todoMeta returns a one-line metadata summary for a queue-list card.
func todoMeta(t store.Todo) string {
	age := ageSince(t.CreatedAt)
	var parts []string
	parts = append(parts, fmt.Sprintf("state: %s", t.State))
	if t.Attempt > 0 {
		parts = append(parts, fmt.Sprintf("attempt %d/%d", t.Attempt, t.MaxAttempts))
	}
	if t.Owner != "" {
		parts = append(parts, fmt.Sprintf("owner: %s", t.Owner))
	}
	if t.LeaseExpiresAt != nil {
		parts = append(parts, fmt.Sprintf("lease: %s", t.LeaseExpiresAt.UTC().Format(time.RFC3339)))
	}
	parts = append(parts, fmt.Sprintf("age: %s", age))
	return strings.Join(parts, " · ")
}

// todoDetail returns a multi-line metadata block for the detail card.
func todoDetail(t store.Todo) string {
	var lines []string
	lines = append(lines, fmt.Sprintf("Queue: %s", t.Queue))
	if t.Source != "" {
		lines = append(lines, fmt.Sprintf("Source: %s", t.Source))
	}
	if t.Kind != "" {
		lines = append(lines, fmt.Sprintf("Kind: %s", t.Kind))
	}
	lines = append(lines, fmt.Sprintf("Attempts: %d / %d", t.Attempt, t.MaxAttempts))
	if t.Owner != "" {
		lines = append(lines, fmt.Sprintf("Owner: %s", t.Owner))
	}
	if t.Assignee != "" {
		lines = append(lines, fmt.Sprintf("Assignee: %s", t.Assignee))
	}
	if t.LeaseExpiresAt != nil {
		lines = append(lines, fmt.Sprintf("Lease expires: %s", t.LeaseExpiresAt.UTC().Format(time.RFC3339)))
	}
	lines = append(lines, fmt.Sprintf("Created: %s", t.CreatedAt.UTC().Format(time.RFC3339)))
	if t.NextRetryAt != nil {
		lines = append(lines, fmt.Sprintf("Next retry: %s", t.NextRetryAt.UTC().Format(time.RFC3339)))
	}
	return strings.Join(lines, "\n")
}

// ageSince renders a human-friendly duration from t to now.
func ageSince(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// sanitizeID replaces non-alphanumeric characters in a queue or todo id for use as an A2UI
// surfaceId (which must be a simple identifier).
func sanitizeID(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	return b.String()
}

// insertBefore inserts one or more items into a slice just before a target item.
func insertBefore(slice []string, target string, items ...string) []string {
	for i, s := range slice {
		if s == target {
			result := make([]string, 0, len(slice)+len(items))
			result = append(result, slice[:i]...)
			result = append(result, items...)
			result = append(result, slice[i:]...)
			return result
		}
	}
	return append(slice, items...)
}
