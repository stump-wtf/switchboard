package web

// Operator Todos view (the durable queue) and the todo detail drawer, in the charm-web language.
//
// Governing: SPEC-0015 REQ "Todos View And Drawer" (ADR-0018 restyle — filterable table + drawer,
// all actions and SSE row updates preserved); SPEC-0012 REQ "Error Handling and Server-Side
// Logging"; SPEC-0003 owns all lifecycle transitions (this layer renders and dispatches, it
// implements NO lifecycle rules of its own — every action delegates to a store method whose
// sentinel errors distinguish not-found from conflict).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/joestump/switchboard/internal/auth"
	"github.com/joestump/switchboard/internal/store"
)

// todoListCap bounds the Todos table listing (a busy queue paginates on reload; the operator filters
// and searches to narrow). The store clamps too.
const todoListCap = 100

// todoRow is the render model for one Todos table row (fragments/todos.html "todo_row") and the drawer
// header. The lease and retry-backoff countdowns are driven by server-stamped deadlines (data
// attributes) that sb.js animates toward — the UI never computes lifecycle, only presents it.
type todoRow struct {
	ID              string
	ShortID         string
	Source          string // originating source, or the queue name for event-less todos
	Kind            string
	TrustMode       string
	State           string
	OwnerLabel      string
	HasLease        bool
	LeaseSecs       int   // remaining lease seconds at render time (claimed)
	LeaseDeadlineMS int64 // unix-ms lease deadline for the sb.js countdown
	RetryScheduled  bool  // failed with a backoff retry pending (SPEC-0003 scheduled backoff)
	RetrySecs       int   // seconds until the scheduled retry at render time (failed)
	RetryDeadlineMS int64 // unix-ms retry deadline for the sb.js countdown
	Attempt         int
	MaxAttempts     int
	DedupCount      int
	CreatedAt       time.Time
	Flash           bool // reaper re-surface — flash the row briefly on swap-in
	OOB             bool // render as an hx-swap-oob replacement (live SSE update)
}

// drawerView feeds the "drawer" fragment (and the standalone "todo" page fallback): the todo detail
// with lease/retry cards, metadata, escaped payload, lifecycle timeline, and state-appropriate
// footer actions. Governing: SPEC-0015 REQ "Todos View And Drawer" (drawer).
type drawerView struct {
	Row            todoRow
	IdempotencyKey string
	PayloadJSON    string // pretty-printed, rendered through html/template escaping (inert)
	HasEvent       bool
	ReceivedAt     time.Time
	ClaimedAt      *time.Time
	CompletedAt    *time.Time
	CSRF           string
}

// panelView feeds the "todos_panel" fragment (filter pills + table) returned on HTMX filter/search
// swaps and embedded server-side in the full Todos page.
type panelView struct {
	Filter string
	Query  string
	Counts store.TodoCounts
	Rows   []todoRow
}

// todoFilters is the canonical ordered set of filter chips (SPEC-0015: all/pending/claimed/done/failed).
var todoFilters = []string{"all", "pending", "claimed", "done", "failed"}

// normalizeFilter maps a query-param filter to a canonical pill key, defaulting to "all".
func normalizeFilter(f string) string {
	switch strings.ToLower(strings.TrimSpace(f)) {
	case "pending":
		return "pending"
	case "claimed":
		return "claimed"
	case "done":
		return "done"
	case "failed":
		return "failed"
	default:
		return "all"
	}
}

// filterState turns a canonical pill key into the store's state filter ("" = all states).
func filterState(filter string) string {
	if filter == "all" {
		return ""
	}
	return filter
}

// isHTMX reports whether a request came from HTMX (an in-page fragment swap) rather than a full
// navigation, so a handler can return a fragment instead of a whole page.
func isHTMX(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true"
}

// Todos renders the durable-queue view: filter pills with live counts, a text search over id/source/
// kind, and the todo table. HTMX filter/search requests receive just the panel fragment; full
// navigations render the whole page. Requires human. Governing: SPEC-0015 REQ "Todos View And
// Drawer" (filterable table).
func (h *Handler) Todos(w http.ResponseWriter, r *http.Request) {
	human, _ := auth.FromContext(r.Context())
	filter := normalizeFilter(r.URL.Query().Get("filter"))
	query := strings.TrimSpace(r.URL.Query().Get("q"))

	sh, _ := h.buildShell(r.Context(), "todos", &human)
	var (
		counts store.TodoCounts
		rows   []todoRow
	)
	if sh.DBConnected {
		var err error
		if counts, err = h.store.TodoCounts(r.Context()); err != nil {
			// Suppressed to a log so the view still renders (degraded pill counts); a reload recovers.
			h.log.Warn("todos counts", "err", err)
		}
		items, err := h.store.ListTodoItems(r.Context(), filterState(filter), query, todoListCap)
		if err != nil {
			h.log.Warn("todos list", "err", err)
		}
		rows = make([]todoRow, 0, len(items))
		for _, it := range items {
			rows = append(rows, h.todoRowFromItem(r.Context(), it, false, false))
		}
	}

	// HTMX filter/search: swap only the pills+table panel, keeping the search box focus intact.
	if isHTMX(r) {
		frag, err := h.renderFragment("todos_panel", panelView{Filter: filter, Query: query, Counts: counts, Rows: rows})
		if err != nil {
			h.fail(w, err)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(frag))
		return
	}

	h.render(w, "todos", view{
		Title: "Todos", Human: &human, CSRF: auth.CSRFFromContext(r.Context()),
		Shell: sh, Counts: counts, TodoItems: rows, Filter: filter, Query: query,
	})
}

// TodoDrawer renders one todo's detail. Requested via HTMX (from a table row) it returns the drawer
// fragment swapped into the overlay slot; a direct navigation renders a standalone page so deep
// links and no-JS degrade sanely. Requires human. Governing: SPEC-0015 REQ "Todos View And Drawer".
func (h *Handler) TodoDrawer(w http.ResponseWriter, r *http.Request) {
	human, _ := auth.FromContext(r.Context())
	id := chi.URLParam(r, "id")
	it, err := h.store.GetTodoItem(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		h.fail(w, err)
		return
	}
	dv := h.buildDrawer(r.Context(), it, auth.CSRFFromContext(r.Context()))

	if isHTMX(r) {
		frag, err := h.renderFragment("drawer", dv)
		if err != nil {
			h.fail(w, err)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(frag))
		return
	}
	sh, _ := h.buildShell(r.Context(), "todos", &human)
	h.render(w, "todo", view{
		Title: "Todo " + shortID(id), Human: &human, CSRF: auth.CSRFFromContext(r.Context()),
		Shell: sh, Drawer: &dv,
	})
}

// CompleteTodo acks a claimed todo the operator owns. POST /todos/{id}/complete.
// Governing: SPEC-0015 REQ "Todos View And Drawer" (actions preserved), SPEC-0003 complete.
func (h *Handler) CompleteTodo(w http.ResponseWriter, r *http.Request) {
	human, _ := auth.FromContext(r.Context())
	t, err := h.store.CompleteTodoAnyEndpoint(r.Context(), chi.URLParam(r, "id"), "op:"+human.ID, []byte(`{}`))
	h.respondTodoAction(w, r, "CompleteTodo", t, err)
}

// FailTodo marks a claimed todo failed (SPEC-0003 retries until attempts exhausted, then dead-letters).
// POST /todos/{id}/fail.
func (h *Handler) FailTodo(w http.ResponseWriter, r *http.Request) {
	human, _ := auth.FromContext(r.Context())
	t, err := h.store.FailTodoAnyEndpoint(r.Context(), chi.URLParam(r, "id"), "op:"+human.ID, []byte(`{}`))
	h.respondTodoAction(w, r, "FailTodo", t, err)
}

// RetryTodo re-queues a dead-lettered (failed) todo now. POST /todos/{id}/retry.
func (h *Handler) RetryTodo(w http.ResponseWriter, r *http.Request) {
	t, err := h.store.RetryTodo(r.Context(), chi.URLParam(r, "id"))
	h.respondTodoAction(w, r, "RetryTodo", t, err)
}

// ExtendTodo extends the visibility lease on a claimed todo the operator owns (heartbeat semantics).
// POST /todos/{id}/extend. Governing: SPEC-0015 REQ "Todos View And Drawer" (Extend lease), SPEC-0003
// REQ "Visibility Window, Lease, Heartbeat".
func (h *Handler) ExtendTodo(w http.ResponseWriter, r *http.Request) {
	human, _ := auth.FromContext(r.Context())
	t, err := h.store.HeartbeatTodoAnyEndpoint(r.Context(), chi.URLParam(r, "id"), "op:"+human.ID, operatorLeaseTTL)
	h.respondTodoAction(w, r, "ExtendTodo", t, err)
}

// ReleaseTodo hands a claimed todo the operator owns back to the queue immediately (→ pending).
// POST /todos/{id}/release. Governing: SPEC-0015 REQ "Todos View And Drawer" (Release).
func (h *Handler) ReleaseTodo(w http.ResponseWriter, r *http.Request) {
	human, _ := auth.FromContext(r.Context())
	t, err := h.store.ReleaseTodo(r.Context(), chi.URLParam(r, "id"), "op:"+human.ID)
	h.respondTodoAction(w, r, "ReleaseTodo", t, err)
}

// respondTodoAction maps a store transition result onto an HTTP response: sentinel errors become
// generic status codes (409 conflict / 404 not-found) with no internal detail, other errors log and
// 500, and success re-renders the fragment appropriate to WHERE the action was invoked from —
// determined by the HTMX target: the drawer (overlay) gets the refreshed drawer, a table row gets
// its refreshed row, and anything else (the Board lane card's Claim, hx-swap="none") gets the OOB
// lane movement. Governing: SPEC-0013 REQ "Error Handling Standards", SPEC-0015 REQ "Patch Panel
// Board".
func (h *Handler) respondTodoAction(w http.ResponseWriter, r *http.Request, handler string, t store.Todo, err error) {
	switch {
	case errors.Is(err, store.ErrConflict):
		http.Error(w, "conflict", http.StatusConflict)
		return
	case errors.Is(err, store.ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
		return
	case err != nil:
		// Wrapped error carries the handler + todo id into the log; the user sees only "internal error".
		h.log.Error("todo action", "handler", handler, "todo", chi.URLParam(r, "id"), "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	ctx := r.Context()
	target := r.Header.Get("HX-Target")
	var frag string
	switch {
	case target == "sb-overlay":
		it, err := h.store.GetTodoItem(ctx, t.ID)
		if err != nil {
			h.fail(w, err)
			return
		}
		frag, err = h.renderFragment("drawer", h.buildDrawer(ctx, it, auth.CSRFFromContext(ctx)))
		if err != nil {
			h.fail(w, err)
			return
		}
	case strings.HasPrefix(target, "sb-tr-"):
		it, err := h.store.GetTodoItem(ctx, t.ID)
		if err != nil {
			h.fail(w, err)
			return
		}
		frag, err = h.renderFragment("todo_row", h.todoRowFromItem(ctx, it, false, false))
		if err != nil {
			h.fail(w, err)
			return
		}
	default:
		// The Board lane card's action (hx-swap="none"): respond with the OOB lane movement so the
		// card crosses lanes immediately even if the SSE frame is dropped — the pair is idempotent
		// when both apply (delete no-ops, insert lands once). Governing: SPEC-0015 REQ "Patch Panel
		// Board" (live movement), REQ "Live Fragment Architecture" (OOB removal + insertion).
		frag, err = h.renderFragment("lane_move", h.laneMoveForTodo(ctx, t, ""))
		if err != nil {
			h.fail(w, err)
			return
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(frag))
}

// todoRowFromItem builds a table-row render model, resolving the lease owner's display name and
// stamping the lease deadline for the client countdown. Event-less todos show their queue as the
// source. oob marks a live SSE replacement; flash requests a brief highlight (reaper re-surface).
func (h *Handler) todoRowFromItem(ctx context.Context, it store.TodoItem, oob, flash bool) todoRow {
	source := it.Source
	if source == "" {
		source = it.Queue
	}
	row := todoRow{
		ID:          it.ID,
		ShortID:     shortID(it.ID),
		Source:      source,
		Kind:        it.Kind,
		TrustMode:   it.TrustMode,
		State:       it.State,
		OwnerLabel:  h.ownerLabel(ctx, it.Owner),
		Attempt:     it.Attempt,
		MaxAttempts: it.MaxAttempts,
		DedupCount:  it.DedupCount,
		CreatedAt:   it.CreatedAt,
		Flash:       flash,
		OOB:         oob,
	}
	if it.State == "claimed" && it.LeaseExpiresAt != nil {
		secs := int(time.Until(*it.LeaseExpiresAt).Seconds())
		if secs < 0 {
			secs = 0
		}
		row.HasLease = true
		row.LeaseSecs = secs
		row.LeaseDeadlineMS = it.LeaseExpiresAt.UnixMilli()
	}
	// A failed todo with a retry window renders the scheduled-backoff countdown (drawer card + row
	// sub-line); one without (next_retry_at NULL) is a dead-letter — retry is manual-only.
	// Governing: SPEC-0003 REQ "Bounded Retries via max_attempts" (scheduled backoff).
	if it.State == "failed" && it.NextRetryAt != nil {
		secs := int(time.Until(*it.NextRetryAt).Seconds())
		if secs < 0 {
			secs = 0
		}
		row.RetryScheduled = true
		row.RetrySecs = secs
		row.RetryDeadlineMS = it.NextRetryAt.UnixMilli()
	}
	return row
}

// buildDrawer assembles the detail-drawer render model: the header row, pretty-printed (and inert)
// payload, and the lifecycle timeline stamps.
func (h *Handler) buildDrawer(ctx context.Context, it store.TodoItem, csrf string) drawerView {
	row := h.todoRowFromItem(ctx, it, false, false)
	dv := drawerView{
		Row:            row,
		IdempotencyKey: it.IdempotencyKey,
		PayloadJSON:    prettyJSON(it.Payload),
		HasEvent:       it.EventID != nil,
		ReceivedAt:     it.CreatedAt,
		ClaimedAt:      it.ClaimedAt,
		CompletedAt:    it.CompletedAt,
		CSRF:           csrf,
	}
	// The originating event's received-at anchors the timeline's first step ("received/verified").
	if it.EventID != nil {
		if e, err := h.store.EventByID(ctx, *it.EventID); err == nil {
			dv.ReceivedAt = e.ReceivedAt
		} else {
			h.log.Warn("drawer event lookup", "todo", it.ID, "event", *it.EventID, "err", err)
		}
	}
	return dv
}

// prettyJSON indents raw JSON payload bytes for display. Non-JSON or empty payloads render as-is (or
// a placeholder). The result is emitted through html/template's contextual escaping by the caller,
// so any HTML/script inside the payload renders inert (SPEC-0015 REQ "Todos View And Drawer" — payload renders safely).
func prettyJSON(raw []byte) string {
	if len(raw) == 0 {
		return "(no payload)"
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return string(raw) // not valid JSON — show the raw bytes, still escaped by the template
	}
	return buf.String()
}
