// Typed SSE event taxonomy for the operator board.
//
// Governing: SPEC-0013 REQ "Live Updates and Toasts", REQ "Board View — Live Incoming Lines" —
// committed store transitions become NAMED SSE events (event_received, todo_created, todo_claimed,
// todo_completed, todo_failed, todo_resurfaced, endpoint_seen, counts) whose payloads are
// pre-rendered HTML fragments; hx-swap-oob swaps let one event update its feed row, the count
// pills, and the toast region atomically. This layers the SPEC-0013 taxonomy on the SPEC-0012 hub in sse.go: same
// lossy fan-out, same reload-renders-DB-truth guarantee — a dropped frame stales a region, never
// state.
//
// Ownership routing (SPEC-0012/0013 — the stream is scoped to the human's own data): the hub
// routes an Event with a nonempty Owner only to that human's streams (sse.go). endpoint_seen has
// a real ownership edge (endpoint → agent → owner_human_id) and publishes scoped. Events, todos,
// and counts do NOT: in today's schema (0001_init.sql) a queue is a plain name with no owning
// human, todos hang off queues/events, and every authenticated human is an operator of the same
// single-tenant board — so those frames publish with Owner "" (all authenticated subscribers).
// Event.Owner is the scoping seam: when queues gain an owner edge, stamp Owner on the todo/event
// frames here and the hub routes per-human with no transport change. Counts frames are global
// aggregates (store.BoardStats / store.TodoCounts carry no per-human filter), so one identical
// frame fans out to all subscribers — recomputing per-human would render the same numbers.
package web

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/joestump/switchboard/internal/store"
)

const (
	// liveQueueSize bounds the ordered publish queue; overflow drops (best-effort presentation).
	liveQueueSize = 256
	// livePublishTimeout bounds the store reads behind one published transition.
	livePublishTimeout = 3 * time.Second
	// activityBuckets is the throughput tile's bar count (24-bucket chart per the design record).
	activityBuckets = 24
	// feedCap is how many feed rows the Board keeps visible (~8 per the design record).
	feedCap = 8
)

// feedRow is the render model for one Board incoming line (templates/fragments.html "feed_row").
type feedRow struct {
	RowID      string // stable DOM id: sb-ev-<event id>, or sb-td-<todo id> for event-less todos
	Source     string
	EventType  string
	TrustMode  string
	ReceivedAt time.Time
	State      string // "" (verifying) | pending | claimed | done | failed
	OwnerLabel string // human-readable lease owner for the claimed stage
	TodoID     string // enables the Claim action on pending rows
	Deduped    bool   // event collapsed onto another delivery's todo — render a truthful "deduped" stage
	OOB        bool   // render as an hx-swap-oob replacement of the existing row
}

// bar is one throughput-tile activity bar.
type bar struct {
	Pct     int  // height as % of the busiest bucket (min 4 so empty buckets stay visible)
	Current bool // the in-progress minute renders in the accent color
}

// tilesView feeds the "tiles" fragment (the Board stat band).
type tilesView struct {
	Stats store.BoardStats
	Bars  []bar
	OOB   bool
}

// countsView feeds the "counts" fragment: the OOB bundle of tiles + rail count + LIVE pill (Board)
// plus the Todos view filter-pill counts. Each region is an independent OOB swap, so a page updates
// only the regions it actually renders (the Todos pills are ignored on the Board and vice-versa).
type countsView struct {
	Tiles tilesView
	Todos store.TodoCounts
}

// endpointSeenView feeds the "endpoint_seen" fragment: the last-seen refresh for one vended
// endpoint's card/row. Endpoint screens render the same fragment server-side (OOB false), so the
// live swap target always exists wherever the endpoint is shown.
type endpointSeenView struct {
	ID     string
	SeenAt time.Time
	OOB    bool
}

// PublishEventReceived adapts a committed inbound event (store.EventHook) into the Board's
// event_received SSE frame plus a counts refresh. Never blocks the ingesting request: work is
// enqueued onto the ordered live queue and dropped on overflow.
func (h *Handler) PublishEventReceived(e store.EventSummary) {
	h.enqueueLive(func(ctx context.Context) {
		if frag, err := h.renderFragment("feed_row", h.feedRowFromEvent(ctx, e, false)); err == nil {
			h.events.Publish(Event{Name: "event_received", Data: frag})
		} else {
			h.log.Error("render event_received fragment", "event", e.ID, "err", err)
		}
		h.publishCounts(ctx)
	})
}

// PublishTodoTransition adapts a committed todo lifecycle transition (store.TodoTransitionHook)
// into its typed SSE frame: an OOB feed-row stage update, a toast for transitions the operator
// didn't initiate in this view, and a counts refresh. Never blocks the transitioning caller.
func (h *Handler) PublishTodoTransition(verb string, t store.Todo) {
	name, ok := sseEventNames[verb]
	if !ok {
		h.log.Warn("unknown todo transition verb", "verb", verb, "todo", t.ID)
		return
	}
	h.enqueueLive(func(ctx context.Context) {
		row := h.feedRowFromTodo(ctx, t, true)
		var payload strings.Builder
		if frag, err := h.renderFragment("feed_row", row); err == nil {
			payload.WriteString(frag)
		} else {
			h.log.Error("render todo fragment", "todo", t.ID, "err", err)
		}
		// The Todos view table row (a different DOM element than the Board feed row) is updated by
		// the SAME event via a second OOB swap, so an open Todos table advances its row live. The
		// reaper's re-surface (todo_resurfaced) flags the row to flash briefly. Rendering it needs
		// the enriched read model (trust mode, dedup count) — a best-effort store read; on error the
		// row swap is skipped and a reload renders truth. Governing: SPEC-0013 REQ "Todos View —
		// Durable Queue" (reaper re-surface is visible).
		if h.store != nil {
			if it, err := h.store.GetTodoItem(ctx, t.ID); err == nil {
				trow := h.todoRowFromItem(ctx, it, true, name == "todo_resurfaced")
				if frag, err := h.renderFragment("todo_row", trow); err == nil {
					payload.WriteString(frag)
				} else {
					h.log.Error("render todo_row fragment", "todo", t.ID, "err", err)
				}
			} else {
				h.log.Warn("live todo_row lookup", "todo", t.ID, "err", err)
			}
		}
		// Background transitions surface as transient toasts (SPEC-0013 "Toast on background
		// transition") announced with the todo id.
		if msg := h.toastText(ctx, name, t); msg != "" {
			if frag, err := h.renderFragment("toast", msg); err == nil {
				payload.WriteString(frag)
			} else {
				h.log.Error("render toast fragment", "todo", t.ID, "err", err)
			}
		}
		if payload.Len() > 0 {
			h.events.Publish(Event{Name: name, Data: payload.String()})
		}
		h.publishCounts(ctx)
	})
}

// PublishEndpointSeen adapts a committed endpoint last-seen stamp (store.EndpointSeenHook) into
// the endpoint_seen SSE frame: an OOB refresh of that endpoint's last-seen node. No counts refresh
// — an authentication changes no queue numbers. Never blocks the authenticating request.
// Whether this should coalesce to a slower tick is an open design question
// (docs/openspec/specs/operator-board/design.md); the hub is lossy either way.
// Governing: SPEC-0013 REQ "Live Updates and Toasts" (endpoint last-seen updates).
func (h *Handler) PublishEndpointSeen(endpointID string, seenAt time.Time) {
	h.enqueueLive(func(ctx context.Context) {
		// Scope the frame to the endpoint's owning human (endpoint → agent → owner_human_id):
		// endpoint cards are a per-owner surface, so another human's streams must not learn when
		// this credential authenticates. Fail CLOSED on a lookup miss — an unscoped broadcast
		// would leak activity across humans, while a dropped frame only stales a last-seen stamp
		// until reload. Governing: SPEC-0012 REQ "Live Updates via SSE" (stream scoped to the
		// human's own data), SPEC-0013 REQ "Live Updates and Toasts".
		owner := ""
		if h.store != nil {
			o, err := h.store.EndpointOwner(ctx, endpointID)
			if err != nil {
				h.log.Warn("endpoint_seen owner lookup", "endpoint", endpointID, "err", err)
				return
			}
			owner = o
		}
		frag, err := h.renderFragment("endpoint_seen", endpointSeenView{ID: endpointID, SeenAt: seenAt, OOB: true})
		if err != nil {
			h.log.Error("render endpoint_seen fragment", "endpoint", endpointID, "err", err)
			return
		}
		h.events.Publish(Event{Name: "endpoint_seen", Data: frag, Owner: owner})
	})
}

// PublishQueueNudge refreshes the live count regions in response to an out-of-band todo_ready
// wakeup (the LISTEN loop in internal/server/listen.go). A todo enqueued by another process never
// crosses this process's store hooks, so the wakeup re-renders the count bundle from the database
// — the queue name is the whole notification payload, so no feed row can be rendered here; the
// row appears on reload (DB truth) or via this process's own hooks. Governing: SPEC-0004 REQ
// "In-Database Wakeups via LISTEN/NOTIFY" ("the web UI can be nudged when new work arrives").
func (h *Handler) PublishQueueNudge(string) {
	h.enqueueLive(func(ctx context.Context) { h.publishCounts(ctx) })
}

// sseEventNames maps store transition verbs onto the SPEC-0013 typed event taxonomy.
var sseEventNames = map[string]string{
	"created": "todo_created",
	"claimed": "todo_claimed",
	"done":    "todo_completed",
	"failed":  "todo_failed",
	"pending": "todo_resurfaced", // reaper re-surface or retry back to the queue
}

// toastText renders the toast copy for a transition, or "" for transitions that only move the
// feed (creation is announced by its own row appearing). It resolves the lease owner's display name
// via the store (claimed · <agent name>).
func (h *Handler) toastText(ctx context.Context, name string, t store.Todo) string {
	id := shortID(t.ID)
	switch name {
	case "todo_claimed":
		return id + " · claimed · " + h.ownerLabel(ctx, t.Owner)
	case "todo_completed":
		return id + " · completed · ack sent"
	case "todo_failed":
		// A scheduled-backoff fail and a dead-letter both commit as 'failed'; NextRetryAt tells the
		// truthful story (SPEC-0003 scheduled backoff — design "will retry with backoff").
		if t.NextRetryAt != nil {
			return id + " · failed · will retry with backoff"
		}
		return id + " · failed · attempts exhausted"
	case "todo_resurfaced":
		return id + " · re-surfaced to queue"
	}
	return ""
}

// publishCounts re-renders the count bundle (tiles + rail count + LIVE pill) from the database
// and publishes it as a counts event. Store errors are logged, never published: subscribers just
// keep their last numbers and a reload renders truth.
func (h *Handler) publishCounts(ctx context.Context) {
	if h.store == nil {
		return
	}
	stats, err := h.store.BoardStats(ctx)
	if err != nil {
		h.log.Error("live counts stats", "err", err)
		return
	}
	buckets, err := h.store.EventBuckets(ctx, activityBuckets)
	if err != nil {
		h.log.Error("live counts buckets", "err", err)
	}
	// The Todos view filter-pill counts ride the same counts frame (OOB spans ignored on the Board).
	todos, err := h.store.TodoCounts(ctx)
	if err != nil {
		h.log.Error("live counts todo counts", "err", err)
	}
	frag, err := h.renderFragment("counts", countsView{
		Tiles: tilesView{Stats: stats, Bars: activityBars(buckets), OOB: true},
		Todos: todos,
	})
	if err != nil {
		h.log.Error("render counts fragment", "err", err)
		return
	}
	h.events.Publish(Event{Name: "counts", Data: frag})
}

// enqueueLive appends fn to the ordered live-publish queue, starting the single worker on first
// use. Ordering matters (event_received must precede todo_created for the same delivery so the
// row exists before its stage update); a full queue drops — lossy by design.
func (h *Handler) enqueueLive(fn func(context.Context)) {
	h.liveOnce.Do(func() {
		h.liveCh = make(chan func(context.Context), liveQueueSize)
		go func() {
			for f := range h.liveCh {
				ctx, cancel := context.WithTimeout(context.Background(), livePublishTimeout)
				f(ctx)
				cancel()
			}
		}()
	})
	select {
	case h.liveCh <- fn:
	default: // full queue: drop — a missed frame costs a swap, reload renders DB truth
	}
}

// renderFragment executes one named fragment from templates/fragments.html into a string ready
// for SSE framing (the transport strips newlines; HTML is whitespace-insensitive).
func (h *Handler) renderFragment(name string, data any) (string, error) {
	var buf bytes.Buffer
	if err := h.frags.ExecuteTemplate(&buf, name, data); err != nil {
		return "", fmt.Errorf("render fragment %q: %w", name, err)
	}
	return buf.String(), nil
}

// feedRowFromEvent builds the feed render model for an event summary (server render and the
// event_received frame share this path, so reload and live rows are pixel-identical).
func feedRowFromEvent(e store.EventSummary, oob bool) feedRow {
	return feedRow{
		RowID:      fmt.Sprintf("sb-ev-%d", e.ID),
		Source:     e.Source,
		EventType:  e.EventType,
		TrustMode:  e.TrustMode,
		ReceivedAt: e.ReceivedAt,
		State:      e.TodoState,
		OwnerLabel: ownerLabel(e.TodoOwner),
		TodoID:     e.TodoID,
		Deduped:    e.Deduped,
		OOB:        oob,
	}
}

// feedRowFromEvent (method) builds the feed row and resolves the lease-owner display name from the
// store (claimed · <agent name>), which the pure function above cannot do. Production render paths
// use this; tests exercise the pure function with a pre-set owner label.
func (h *Handler) feedRowFromEvent(ctx context.Context, e store.EventSummary, oob bool) feedRow {
	row := feedRowFromEvent(e, oob)
	row.OwnerLabel = h.ownerLabel(ctx, e.TodoOwner)
	return row
}

// feedRowFromTodo builds the feed render model for a todo transition, resolving the originating
// event for provenance (source, type, trust badge). Event-less todos (queue adapters, dev seeds)
// fall back to the todo's own fields under the queue trust mode.
func (h *Handler) feedRowFromTodo(ctx context.Context, t store.Todo, oob bool) feedRow {
	row := feedRow{
		RowID:      "sb-td-" + t.ID,
		Source:     t.Source,
		EventType:  t.Kind,
		TrustMode:  "queue",
		ReceivedAt: t.CreatedAt,
		State:      t.State,
		OwnerLabel: h.ownerLabel(ctx, t.Owner),
		TodoID:     t.ID,
		OOB:        oob,
	}
	if t.EventID != nil && h.store != nil {
		if e, err := h.store.EventByID(ctx, *t.EventID); err == nil {
			row.RowID = fmt.Sprintf("sb-ev-%d", e.ID)
			row.Source, row.EventType, row.TrustMode, row.ReceivedAt = e.Source, e.EventType, e.TrustMode, e.ReceivedAt
		} else {
			h.log.Warn("live feed event lookup", "todo", t.ID, "event", *t.EventID, "err", err)
		}
	}
	return row
}

// activityBars scales per-minute bucket counts into bar heights (percent of the busiest bucket,
// floor 4% so quiet minutes still render a tick). The last bucket is the in-progress minute.
func activityBars(buckets []int) []bar {
	if len(buckets) == 0 {
		return nil
	}
	max := 1
	for _, c := range buckets {
		if c > max {
			max = c
		}
	}
	bars := make([]bar, len(buckets))
	for i, c := range buckets {
		pct := c * 100 / max
		if pct < 4 {
			pct = 4
		}
		bars[i] = bar{Pct: pct, Current: i == len(buckets)-1}
	}
	return bars
}

// ownerLabel (method) resolves an agent lease owner to its display name via the store, rendering
// `agent · <agent name>` where the pure function can only show the id prefix. The operator owner and
// the empty owner fall through to the pure function; any store miss (unknown id, lookup error, nil
// store) degrades gracefully to the id-prefix label. Governing: SPEC-0013 REQ "Board View — Live
// Incoming Lines" (claimed · <agent name>).
func (h *Handler) ownerLabel(ctx context.Context, owner string) string {
	if h.store != nil && strings.HasPrefix(owner, "agent:") {
		id := strings.TrimPrefix(owner, "agent:")
		if name, err := h.store.AgentNameByID(ctx, id); err == nil && name != "" {
			return "agent · " + name
		}
	}
	return ownerLabel(owner)
}

// ownerLabel renders a lease owner for humans: vended agents claim as "agent:<uuid>" (SPEC-0006/
// 0014 owner convention), the operator claims as "op:<human id>" (SPEC-0013 operator Claim).
func ownerLabel(owner string) string {
	switch {
	case owner == "":
		return ""
	case strings.HasPrefix(owner, "op:"):
		return "operator"
	case strings.HasPrefix(owner, "agent:"):
		return "agent · " + shortID(strings.TrimPrefix(owner, "agent:"))
	default:
		return owner
	}
}

// shortID compacts a todo/agent id for display (td_8f2a-style, per the design record).
func shortID(id string) string {
	const n = 7
	if len(id) <= n {
		return id
	}
	return id[:n]
}
