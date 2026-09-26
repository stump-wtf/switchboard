// Typed SSE event taxonomy for the patch-panel board.
//
// Governing: SPEC-0015 REQ "Patch Panel Board", REQ "Live Fragment Architecture" — the board is
// three lanes (received · verified · patched through) and every live transition is a NAMED SSE
// event whose payload expresses lane movement as an out-of-band REMOVAL (hx-swap-oob="delete" of
// the card's stable id) plus an out-of-band INSERTION (hx-swap-oob="afterbegin:#sb-lane-…-cards"
// into the destination lane):
//
//   - lane_received / lane_rejected / lane_deduped — the EPHEMERAL received lane, fed by ingest
//     instrumentation (internal/ingest instrument.go), SSE-only, never persisted: a delivery
//     entering verification, a redacted rejection surface, an idempotency collapse. A reload
//     renders the received lane empty — that is truthful (design.md "Received lane is ephemeral
//     by design"; SPEC-0001 rejection doctrine unchanged: rejected payloads are never stored).
//   - todo_created / todo_claimed / todo_completed / todo_failed / todo_resurfaced — committed
//     store transitions (store.TodoTransitionHook): created moves the in-flight card from
//     received into *verified* (durable, unclaimed); claimed moves verified → *patched through*;
//     completed/failed update in place within patched through; resurfaced returns the card to
//     *verified*. Each frame also carries the Todos-view row OOB swap and (for background
//     transitions) a toast.
//   - counts — the OOB count bundle: stat tiles, rail badge, LIVE pill, Todos filter pills, and
//     the verified/patched lane-header counts. The received lane's count is DOM-derived
//     client-side (sb-live.js) because its cards are ephemeral.
//   - endpoint_seen — the last-seen refresh for one vended endpoint's card.
//
// The remove+insert pair is idempotent under redelivery (the delete no-ops when the id is absent,
// the insert always lands), so a directly-responded claim and its SSE frame can both apply. The
// SPEC-0012 hub in sse.go is UNCHANGED: same lossy fan-out, same reload-renders-DB-truth guarantee
// — a dropped frame stales a lane, never state.
//
// Ownership routing (SPEC-0012 — the stream is scoped to the human's own data). Every frame that
// carries persisted content now publishes scoped, via the same ownership edge endpoint_seen has
// always used: endpoint → agent → owner_human_id.
//
// This file used to say the opposite, and said it as a justification: "a queue is a plain name with
// no owning human and every authenticated human operates the same single-tenant board, so those
// frames publish with an empty Owner". The premise was about QUEUES; the frames carry TODOS,
// which have had an owning human all along. So the board broadcast every tenant's lane cards, todo rows and
// counts to every connected browser — a push leak, needing no navigation to trigger. Six humans
// held accounts. Fixed by resolving each todo's owner and rendering counts per subscribed human.
//
// STILL UNSCOPED, deliberately and documented: the three pre-persistence in-flight frames
// (lane_received / lane_rejected / lane_deduped). They fire from ingest.Instrument BEFORE any todo
// exists, so there is no ownership edge to resolve yet — the Instrument interface is handed only
// (provider, eventType, trust, key). They carry no payload and no todo content, but they do
// disclose that a delivery arrived and from which provider. Narrowing them needs the ingest
// instrument to pass the resolved webhook, which is a separate change (see #176).
//
// Event.Owner remains the scoping seam.
package web

import (
	"bytes"
	"context"
	"fmt"
	"hash/fnv"
	"strings"
	"time"

	"github.com/stump-wtf/switchboard/internal/store"
)

const (
	// liveQueueSize bounds the ordered publish queue; overflow drops (best-effort presentation).
	liveQueueSize = 256
	// livePublishTimeout bounds the store reads behind one published transition.
	livePublishTimeout = 3 * time.Second
	// activityBuckets is the throughput tile's bar count (24-bucket chart per the design record).
	activityBuckets = 24
	// laneCap is how many cards each lane renders server-side and keeps visible live (sb-live.js
	// trims to the data-sb-lane-cap stamp).
	laneCap = 8

	// Ephemeral received-lane cards expire client-side (data-sb-ephemeral, ms): an in-flight card
	// whose resolution frame was lost self-cleans, and resolution surfaces (rejected/deduped) are
	// transient by design — nothing behind them is persisted (SPEC-0015 received lane semantics).
	inflightTTLMS = 60000
	resolvedTTLMS = 8000
)

// Lane keys — the DOM insertion targets are #sb-lane-<key>-cards (templates/fragments/board.html).
const (
	laneReceived = "received"
	laneVerified = "verified"
	lanePatched  = "patched"
)

// laneCard is the render model for one patch-panel card (fragments/board.html "lane_card"):
// provider glyph (tag), title, trust chip, state chip, detail line, and age (SPEC-0015 REQ
// "Patch Panel Board" card anatomy).
type laneCard struct {
	DomID      string // stable card id: sb-td-<todo id> (durable) or sb-rx-<hash> (ephemeral)
	Lane       string // received | verified | patched — the insertion target
	Source     string
	Kind       string // event type / kind headline
	Title      string // one-line summary under the headline
	TrustMode  string
	State      string // verifying | rejected | deduped (ephemeral) · pending | claimed | done | failed (durable)
	Detail     string // foot line: "checking signature", the redacted rejection reason, "idem ok", owner…
	OwnerLabel string
	TodoID     string // enables the Claim action on pending cards
	Held       bool   // on the quarantine queue: never offers Claim (SPEC-0026 REQ-6)
	At         time.Time
	TTLMS      int  // client-side expiry for ephemeral cards (0 = durable)
	OOB        bool // render as an hx-swap-oob afterbegin insertion into the lane
}

// laneMove is the OOB removal+insertion pair (fragments/board.html "lane_move"): delete the card's
// previous DOM node by id, then insert the refreshed card into its (possibly new) lane.
// Governing: SPEC-0015 REQ "Live Fragment Architecture" (typed events, OOB removal + insertion).
type laneMove struct {
	RemoveID string // "" = pure insertion (no prior card to remove)
	Card     laneCard
}

// laneCounts are the verified/patched lane-header counts (the received count is DOM-derived
// client-side — its cards are ephemeral and never counted in the database).
type laneCounts struct {
	Verified int // durable todos not yet claimed (pending)
	Patched  int // claimed or beyond (claimed + done + failed)
}

// toastMsg is the render model for one toast notification (fragments/shared.html "toast"):
// the transition type drives the accent color and icon, the message is the one-line summary.
type toastMsg struct {
	Kind string // claimed | completed | failed | resurfaced
	Text string // e.g. "td_42 · claimed · operator"
}

// lanesView feeds the "board_lanes" fragment: the server-rendered three-lane panel. Received is
// empty on every full render (ephemeral, SSE-only); verified/patched render from the durable queue.
type lanesView struct {
	Received []laneCard
	Verified []laneCard
	Patched  []laneCard
	Counts   laneCounts
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
// plus the Todos view filter-pill counts and the board's lane-header counts. Each region is an
// independent OOB swap, so a page updates only the regions it actually renders.
type countsView struct {
	Tiles tilesView
	Todos store.TodoCounts
	Lanes laneCounts
}

// endpointSeenView feeds the "endpoint_seen" fragment: the last-seen refresh for one vended
// endpoint's card/row. Endpoint screens render the same fragment server-side (OOB false), so the
// live swap target always exists wherever the endpoint is shown.
type endpointSeenView struct {
	ID     string
	SeenAt time.Time
	OOB    bool
}

// rxCardID derives the stable DOM id for a delivery's ephemeral received-lane card from its
// provider + idempotency key — the two facts BOTH the ingest instrumentation (arrival/rejection)
// and the committed todo hook (advance) know, so the arrival card and its resolution frame always
// target the same node. Hashed so provider keys (GUIDs, sha256:… hashes, webhook-scoped ids) are
// always CSS-selector-safe.
func rxCardID(provider, key string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(provider))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(key))
	return fmt.Sprintf("sb-rx-%016x", h.Sum64())
}

// inflightCard builds the ephemeral received-lane card for a delivery under verification. Only
// redacted, presentation-safe facts arrive here (instrument.go contract) — never payloads,
// headers, signatures, or tokens.
func inflightCard(provider, eventType, trust, key string, at time.Time) laneCard {
	detail := "checking source"
	switch trust {
	case "signed":
		detail = "checking signature"
	case "token":
		detail = "checking token"
	case "open":
		detail = "open line · no verification"
	}
	return laneCard{
		DomID:     rxCardID(provider, key),
		Lane:      laneReceived,
		Source:    provider,
		Kind:      eventType,
		TrustMode: trust,
		State:     "verifying",
		Detail:    detail,
		At:        at,
		TTLMS:     inflightTTLMS,
	}
}

// DeliveryReceived implements ingest.Instrument (satisfied structurally — web imports no ingest): a delivery entered verification, so its
// ephemeral card appears in the received lane. SSE-only — nothing is persisted for this frame, and
// a quiet reload truthfully renders the lane empty. Never blocks the ingesting request.
// Governing: SPEC-0015 REQ "Patch Panel Board" (received lane), design.md.
func (h *Handler) DeliveryReceived(provider, eventType, trust, key string) {
	at := time.Now()
	h.enqueueLive(func(ctx context.Context) {
		card := inflightCard(provider, eventType, trust, key, at)
		card.OOB = true
		frag, err := h.renderFragment("lane_card", card)
		if err != nil {
			h.log.Error("render lane_received fragment", "provider", provider, "err", err)
			return
		}
		h.events.Publish(Event{Name: "lane_received", Data: frag})
	})
}

// DeliveryRejected implements ingest.Instrument: verification failed and the payload was
// NOT persisted (SPEC-0001 unchanged) — the in-flight card is replaced by a transient, redacted
// rejection surface that expires client-side. reason is the client-safe rejection message; no
// signature, token, or body detail ever reaches the frame.
// Governing: SPEC-0015 REQ "Patch Panel Board" scenario "Rejected caller".
func (h *Handler) DeliveryRejected(provider, eventType, trust, key, reason string) {
	at := time.Now()
	h.enqueueLive(func(ctx context.Context) {
		card := inflightCard(provider, eventType, trust, key, at)
		card.State, card.Detail, card.TTLMS, card.OOB = "rejected", reason, resolvedTTLMS, true
		frag, err := h.renderFragment("lane_move", laneMove{RemoveID: card.DomID, Card: card})
		if err != nil {
			h.log.Error("render lane_rejected fragment", "provider", provider, "err", err)
			return
		}
		h.events.Publish(Event{Name: "lane_rejected", Data: frag})
	})
}

// DeliveryDeduped implements ingest.Instrument: an accepted redelivery collapsed onto an
// existing live todo (idempotency dedup), so the in-flight card resolves transiently without a
// lane advance — the durable card it collapsed onto already sits in its lane.
// Governing: SPEC-0015 REQ "Patch Panel Board"; SPEC-0001 REQ "Idempotency Key Extraction and Dedup".
func (h *Handler) DeliveryDeduped(provider, eventType, trust, key string) {
	at := time.Now()
	h.enqueueLive(func(ctx context.Context) {
		card := inflightCard(provider, eventType, trust, key, at)
		card.State, card.Detail, card.TTLMS, card.OOB = "deduped", "collapsed onto an existing todo", resolvedTTLMS, true
		frag, err := h.renderFragment("lane_move", laneMove{RemoveID: card.DomID, Card: card})
		if err != nil {
			h.log.Error("render lane_deduped fragment", "provider", provider, "err", err)
			return
		}
		h.events.Publish(Event{Name: "lane_deduped", Data: frag})
	})
}

// PublishEventReceived adapts a committed inbound event (store.EventHook) into a counts refresh:
// the event moved the throughput/verified numbers. The LANE movement for an accepted delivery
// rides the todo_created frame (the todo hook fires right after, in order), so the committed event
// itself publishes no card — the received lane never shows persisted state. Never blocks.
func (h *Handler) PublishEventReceived(store.EventSummary) {
	h.enqueueLive(func(ctx context.Context) { h.publishCounts(ctx) })
}

// PublishTodoTransition adapts a committed todo lifecycle transition (store.TodoTransitionHook)
// into its typed SSE frame: the lane movement (OOB removal + insertion), the Todos-view row OOB
// swap, a toast for transitions the operator didn't initiate in this view, and a counts refresh.
// Never blocks the transitioning caller.
func (h *Handler) PublishTodoTransition(verb string, t store.Todo) {
	name, ok := sseEventNames[verb]
	if !ok {
		h.log.Warn("unknown todo transition verb", "verb", verb, "todo", t.ID)
		return
	}
	h.enqueueLive(func(ctx context.Context) {
		// Resolve the owning human FIRST. Every fragment below carries this todo's content — lane
		// card, table row, toast — so a frame we cannot attribute must not be published at all.
		// Failing closed here is the whole point: the previous behaviour was to publish anyway,
		// unowned, which the hub reads as "route to every authenticated subscriber".
		owner, err := h.ownerOfTodo(ctx, t)
		// With no store there is no tenancy to enforce: humans, endpoints and todos all live in
		// the database, so a storeless handler has nobody to leak BETWEEN. Real transitions only
		// ever arrive from store hooks, which cannot fire without a store, so this branch is
		// reachable only from render/escaping tests. It is scoped to h.store == nil deliberately —
		// a store that is present but yields no owner still fails closed below.
		if h.store != nil && (err != nil || owner == "") {
			h.log.Warn("live todo transition: unattributable, not published",
				"todo", t.ID, "endpoint", t.EndpointID, "err", err)
			return
		}
		var payload strings.Builder
		if frag, err := h.renderFragment("lane_move", h.laneMoveForTodo(ctx, t, name)); err == nil {
			payload.WriteString(frag)
		} else {
			h.log.Error("render lane_move fragment", "todo", t.ID, "err", err)
		}
		// The Todos view table row (a different DOM element than the board card) is updated by the
		// SAME event via a second OOB swap, so an open Todos table advances its row live. The
		// reaper's re-surface (todo_resurfaced) flags the row to flash briefly. Rendering it needs
		// the enriched read model (trust mode, dedup count) — a best-effort store read; on error the
		// row swap is skipped and a reload renders truth. Governing: SPEC-0015 REQ "Todos View And
		// Drawer" (SSE row updates preserved).
		if h.store != nil {
			if it, err := h.store.GetTodoItem(ctx, owner, t.ID); err == nil {
				trow := h.todoRowFromItem(ctx, it, true, name == "todo_resurfaced")
				// Creation and re-surface may address a row the open table never rendered (a new
				// todo, or one that had left the current filter), where an id-targeted OOB swap
				// silently no-ops (#96). Those render as the lane_move idiom instead: an
				// idempotent OOB delete plus an afterbegin insertion into #sb-todos-body;
				// sb-live.js drops inserts that miss the viewer's active filter/search.
				trow.Insert = name == "todo_created" || name == "todo_resurfaced"
				if frag, err := h.renderFragment("todo_row", trow); err == nil {
					payload.WriteString(frag)
				} else {
					h.log.Error("render todo_row fragment", "todo", t.ID, "err", err)
				}
			} else {
				h.log.Warn("live todo_row lookup", "todo", t.ID, "err", err)
			}
		}
		// Background transitions surface as transient toasts announced with the todo id.
		if msg := h.toastFor(ctx, name, t); msg != nil {
			if frag, err := h.renderFragment("toast", msg); err == nil {
				payload.WriteString(frag)
			} else {
				h.log.Error("render toast fragment", "todo", t.ID, "err", err)
			}
		}
		if payload.Len() > 0 {
			h.events.Publish(Event{Name: name, Data: payload.String(), Owner: owner})
		}
		h.publishCounts(ctx)
	})
}

// ownerOfTodo resolves the human a todo belongs to, via the endpoint → agent → owner_human_id edge
// that endpoint_seen has always used. It is the routing key for every content-carrying live frame.
//
// A todo with no endpoint belongs to nobody, and the caller must treat that as "do not publish"
// rather than "publish to everyone" — an unowned row is an ingestion bug, and broadcasting it is
// the exact failure this file used to have. Returns "" with no error in that case so the caller's
// `owner == ""` guard catches it alongside a real lookup failure.
func (h *Handler) ownerOfTodo(ctx context.Context, t store.Todo) (string, error) {
	if h.store == nil || t.EndpointID == "" {
		return "", nil
	}
	return h.store.EndpointOwner(ctx, t.EndpointID)
}

// laneMoveForTodo expresses one committed todo transition as lane movement. Creation removes the
// delivery's EPHEMERAL received card (correlated by source + idempotency key — the same facts the
// ingest instrumentation stamped) and inserts the durable card into verified; every later verb
// removes the durable card by its stable id and re-inserts it into the lane its new state belongs
// to. The pair is idempotent: delete no-ops when the node is absent, insert always lands.
func (h *Handler) laneMoveForTodo(ctx context.Context, t store.Todo, event string) laneMove {
	card := h.laneCardFromTodo(ctx, t)
	card.OOB = true
	removeID := card.DomID
	if event == "todo_created" {
		removeID = ""
		if t.IdempotencyKey != "" {
			removeID = rxCardID(card.Source, t.IdempotencyKey)
		}
	}
	return laneMove{RemoveID: removeID, Card: card}
}

// PublishEndpointSeen adapts a committed endpoint last-seen stamp (store.EndpointSeenHook) into
// the endpoint_seen SSE frame: an OOB refresh of that endpoint's last-seen node. No counts refresh
// — an authentication changes no queue numbers. Never blocks the authenticating request.
// Governing: SPEC-0012 REQ "Live Updates via SSE", SPEC-0015 REQ "Live Fragment Architecture".
func (h *Handler) PublishEndpointSeen(endpointID string, seenAt time.Time) {
	h.enqueueLive(func(ctx context.Context) {
		// Scope the frame to the endpoint's owning human (endpoint → agent → owner_human_id):
		// endpoint cards are a per-owner surface, so another human's streams must not learn when
		// this credential authenticates. Fail CLOSED on a lookup miss — an unscoped broadcast
		// would leak activity across humans, while a dropped frame only stales a last-seen stamp
		// until reload. Governing: SPEC-0012 REQ "Live Updates via SSE" (stream scoped to the
		// human's own data).
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
// — the queue name is the whole notification payload, so no card can be rendered here; the card
// appears on reload (DB truth) or via this process's own hooks. Governing: SPEC-0004 REQ
// "In-Database Wakeups via LISTEN/NOTIFY" ("the web UI can be nudged when new work arrives").
func (h *Handler) PublishQueueNudge(string) {
	h.enqueueLive(func(ctx context.Context) { h.publishCounts(ctx) })
}

// sseEventNames maps store transition verbs onto the typed event taxonomy. The four A2A states
// (SPEC-0018 REQ "Task State Machine Extension") ride their own typed events so the board advances
// live on cancel/reject/interrupt just as it does on the original four transitions; an interrupt
// resolving back to `claimed` (ResumeTodo) reuses the existing todo_claimed event. Governing:
// SPEC-0015 lane semantics, SPEC-0018 REQ "Task State Machine Extension".
var sseEventNames = map[string]string{
	"created":        "todo_created",
	"claimed":        "todo_claimed",
	"done":           "todo_completed",
	"failed":         "todo_failed",
	"pending":        "todo_resurfaced", // reaper re-surface or retry back to the queue
	"canceled":       "todo_canceled",
	"rejected":       "todo_rejected",
	"input-required": "todo_input_required",
	"auth-required":  "todo_auth_required",
}

// laneForState maps a durable todo state onto its board lane: pending = verified (durable,
// unclaimed); claimed and beyond = patched through. The four A2A states (SPEC-0018) are all
// post-pending — canceled/rejected/input-required/auth-required each land in the patched lane via
// this default, which is correct: none is an unclaimed durable arrival. Governing: SPEC-0015 lane
// semantics, SPEC-0018 REQ "Task State Machine Extension".
func laneForState(state string) string {
	if state == "pending" {
		return laneVerified
	}
	return lanePatched
}

// laneStateLabel renders a card's state chip text: the durable "pending" state reads "queued" on
// the board (the design record's chip), every other state is its own label. The A2A states
// (canceled/rejected/input-required/auth-required, SPEC-0018) render under their own internal
// spelling here — the A2A wire projection (store.A2ATaskState) applies only at the A2A boundary, not
// on switchboard's own board.
func laneStateLabel(state string) string {
	if state == "pending" {
		return "queued"
	}
	return state
}

// toastFor renders the toast for a transition, or nil for transitions that only move the
// board (creation is announced by its own card appearing). It resolves the lease owner's display
// name via the store (claimed · <agent name>).
func (h *Handler) toastFor(ctx context.Context, name string, t store.Todo) *toastMsg {
	id := shortID(t.ID)
	switch name {
	case "todo_claimed":
		return &toastMsg{Kind: "claimed", Text: id + " · claimed · " + h.ownerLabel(ctx, t.Owner)}
	case "todo_completed":
		return &toastMsg{Kind: "completed", Text: id + " · completed · ack sent"}
	case "todo_failed":
		// A scheduled-backoff fail and a dead-letter both commit as 'failed'; NextRetryAt tells the
		// truthful story (SPEC-0003 scheduled backoff — design "will retry with backoff").
		if t.NextRetryAt != nil {
			return &toastMsg{Kind: "failed", Text: id + " · failed · will retry with backoff"}
		}
		return &toastMsg{Kind: "failed", Text: id + " · failed · attempts exhausted"}
	case "todo_resurfaced":
		return &toastMsg{Kind: "resurfaced", Text: id + " · re-surfaced to queue"}
	// A2A transitions (SPEC-0018 REQ "Task State Machine Extension"). Cancel/reject are terminal
	// moves worth a toast; the interrupt states announce the todo is paused waiting on the client.
	case "todo_canceled":
		return &toastMsg{Kind: "completed", Text: id + " · canceled"}
	case "todo_rejected":
		return &toastMsg{Kind: "failed", Text: id + " · rejected"}
	case "todo_input_required":
		return &toastMsg{Kind: "claimed", Text: id + " · awaiting input"}
	case "todo_auth_required":
		return &toastMsg{Kind: "claimed", Text: id + " · awaiting auth"}
	}
	return nil
}

// publishCounts re-renders the count bundle (tiles + rail count + LIVE pill + lane counts) and
// publishes it, ONCE PER SUBSCRIBED HUMAN, each frame carrying only that human's numbers. Store
// errors are logged, never published: subscribers just keep their last numbers and a reload
// renders truth.
//
// It used to render one bundle from global counts and broadcast it. Counts are a disclosure in
// their own right — a tile reading "412 today" on a board showing four rows tells the viewer a
// great deal about the other tenants — so the render moved inside the per-human loop rather than
// the publish. The loop is bounded by CONNECTED humans (single digits in practice), not by the
// humans table, and does nothing at all when nobody is watching.
func (h *Handler) publishCounts(ctx context.Context) {
	if h.store == nil {
		return
	}
	for _, human := range h.events.humans() {
		h.publishCountsFor(ctx, human)
	}
}

// publishCountsFor renders and publishes the counts bundle for exactly one human.
func (h *Handler) publishCountsFor(ctx context.Context, human string) {
	stats, err := h.store.BoardStats(ctx, human)
	if err != nil {
		h.log.Error("live counts stats", "err", err)
		return
	}
	buckets, err := h.store.EventBuckets(ctx, human, activityBuckets)
	if err != nil {
		h.log.Error("live counts buckets", "err", err)
	}
	// The Todos view filter-pill counts and the board lane-header counts ride the same counts
	// frame (OOB spans ignored on pages that don't render them).
	todos, err := h.store.TodoCounts(ctx, human)
	if err != nil {
		h.log.Error("live counts todo counts", "err", err)
	}
	frag, err := h.renderFragment("counts", countsView{
		Tiles: tilesView{Stats: stats, Bars: activityBars(buckets), OOB: true},
		Todos: todos,
		Lanes: laneCountsFrom(todos),
	})
	if err != nil {
		h.log.Error("render counts fragment", "err", err)
		return
	}
	h.events.Publish(Event{Name: "counts", Data: frag, Owner: human})
}

// laneCountsFrom derives the lane-header counts from the per-state todo counts: verified holds the
// durable-unclaimed queue, patched through everything claimed or beyond.
func laneCountsFrom(c store.TodoCounts) laneCounts {
	return laneCounts{Verified: c.Pending, Patched: c.Claimed + c.Done + c.Failed}
}

// enqueueLive appends fn to the ordered live-publish queue, starting the single worker on first
// use. Ordering matters (lane_received must precede todo_created for the same delivery so the
// in-flight card exists before the frame that removes it); a full queue drops — lossy by design.
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

// renderFragment executes one named fragment from the per-view fragment files (templates/fragments/)
// into a string ready for SSE framing (the transport strips newlines; HTML is whitespace-insensitive).
func (h *Handler) renderFragment(name string, data any) (string, error) {
	var buf bytes.Buffer
	if err := h.frags.ExecuteTemplate(&buf, name, data); err != nil {
		return "", fmt.Errorf("render fragment %q: %w", name, err)
	}
	return buf.String(), nil
}

// laneCardFromItem builds a durable lane card from the enriched Todos read model (server render:
// the same card the SSE frames later move, so reload and live cards are pixel-identical).
func laneCardFromItem(it store.TodoItem) laneCard {
	source := it.Source
	if source == "" {
		source = it.Queue
	}
	card := laneCard{
		DomID:      "sb-td-" + it.ID,
		Lane:       laneForState(it.State),
		Source:     source,
		Kind:       it.Kind,
		Title:      it.Title,
		TrustMode:  it.TrustMode,
		State:      it.State,
		OwnerLabel: ownerLabel(it.Owner),
		TodoID:     it.ID,
		Held:       it.Queue == store.QueueQuarantine,
		At:         it.CreatedAt,
	}
	card.Detail = laneCardDetail(it.Todo)
	return card
}

// laneCardFromItem (method) additionally resolves the lease owner's display name via the store.
func (h *Handler) laneCardFromItem(ctx context.Context, it store.TodoItem) laneCard {
	card := laneCardFromItem(it)
	card.OwnerLabel = h.ownerLabel(ctx, it.Owner)
	card.Detail = h.laneCardDetail(ctx, it.Todo)
	return card
}

// laneCardFromTodo builds the durable lane card for a todo transition, resolving the originating
// event for provenance (source, event type, trust chip). Event-less todos (operator pushes, dev
// seeds) fall back to the todo's own fields under the queue trust mode.
func (h *Handler) laneCardFromTodo(ctx context.Context, t store.Todo) laneCard {
	source := t.Source
	if source == "" {
		source = t.Queue
	}
	card := laneCard{
		DomID:      "sb-td-" + t.ID,
		Lane:       laneForState(t.State),
		Source:     source,
		Kind:       t.Kind,
		Title:      t.Title,
		TrustMode:  "queue",
		State:      t.State,
		OwnerLabel: h.ownerLabel(ctx, t.Owner),
		TodoID:     t.ID,
		Held:       t.Queue == store.QueueQuarantine,
		At:         t.CreatedAt,
	}
	if t.EventID != nil && h.store != nil {
		// Scoped by the todo's own owner: this is the trust-mode chip for a card that is already
		// this human's, so it can never widen what the card shows.
		owner, oerr := h.ownerOfTodo(ctx, t)
		if oerr != nil {
			owner = ""
		}
		if e, err := h.store.EventByID(ctx, owner, *t.EventID); err == nil {
			card.TrustMode = e.TrustMode
			if card.Kind == "" {
				card.Kind = e.EventType
			}
		} else {
			h.log.Warn("live lane card event lookup", "todo", t.ID, "event", *t.EventID, "err", err)
		}
	}
	card.Detail = h.laneCardDetail(ctx, t)
	return card
}

// laneCardDetail renders a durable card's foot line per state: dedup provenance for queued work,
// the lease owner for claimed work, the outcome for terminal states.
func laneCardDetail(t store.Todo) string {
	switch t.State {
	case "pending":
		if t.IdempotencyKey != "" {
			return "idem ok"
		}
		return "queued"
	case "claimed":
		return "claimed · " + ownerLabel(t.Owner)
	case "done":
		return "done ✓ · ack sent"
	case "failed":
		if t.NextRetryAt != nil {
			return "failed · will retry"
		}
		return "failed · attempts exhausted"
	// A2A states (SPEC-0018 REQ "Task State Machine Extension"). Terminal canceled/rejected read
	// their own outcome; the interrupt states surface who they're still owned by, since the lease is
	// retained and the todo returns to that owner when the requirement is supplied.
	case "canceled":
		return "canceled"
	case "rejected":
		return "rejected · not retried"
	case "input-required":
		return "input required · " + ownerLabel(t.Owner)
	case "auth-required":
		return "auth required · " + ownerLabel(t.Owner)
	}
	return ""
}

// laneCardDetail (method) resolves the claimed/interrupted owner's display name via the store. The
// A2A interrupt states (input-required/auth-required, SPEC-0018) retain their owner just like
// `claimed`, so they get the same store-resolved owner label rather than the raw owner id.
func (h *Handler) laneCardDetail(ctx context.Context, t store.Todo) string {
	switch t.State {
	case "claimed":
		return "claimed · " + h.ownerLabel(ctx, t.Owner)
	case "input-required":
		return "input required · " + h.ownerLabel(ctx, t.Owner)
	case "auth-required":
		return "auth required · " + h.ownerLabel(ctx, t.Owner)
	}
	return laneCardDetail(t)
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
// store) degrades gracefully to the id-prefix label.
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
// 0014 owner convention), the operator claims as "op:<human id>" (the operator Claim).
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
