package web

// Typed SSE taxonomy coverage (SPEC-0013 REQ "Board View — Live Incoming Lines", REQ "Live
// Updates and Toasts"): fragments render standalone, carry the hx-swap-oob wiring that lets one
// named event update row + pills + toast atomically, and the ordered publish queue preserves the
// event_received → todo_created lifecycle order for a single delivery.

import (
	"strings"
	"testing"
	"time"

	"github.com/joestump/switchboard/internal/store"
)

// renderFrag renders one fragment from the standalone fragment set (the same path live.go uses
// for SSE payloads — no page around it).
func renderFrag(t *testing.T, h *Handler, name string, data any) string {
	t.Helper()
	out, err := h.renderFragment(name, data)
	if err != nil {
		t.Fatalf("render fragment %s: %v", name, err)
	}
	return out
}

func TestFeedRowFragmentStages(t *testing.T) {
	h := newTestHandler(t)
	base := store.EventSummary{ID: 9, Source: "github", EventType: "push", TrustMode: "signed", ReceivedAt: time.Now()}

	verifying := renderFrag(t, h, "feed_row", feedRowFromEvent(base, false))
	for _, want := range []string{`id="sb-ev-9"`, "verifying…", "sb-feed__pulse", "sb-badge--signed", ">GH<"} {
		if !strings.Contains(verifying, want) {
			t.Errorf("verifying row: missing %q in %q", want, verifying)
		}
	}
	if strings.Contains(verifying, "hx-swap-oob") {
		t.Error("non-OOB row must not carry hx-swap-oob")
	}

	pending := base
	pending.TodoID, pending.TodoState = "td_1", "pending"
	row := renderFrag(t, h, "feed_row", feedRowFromEvent(pending, true))
	for _, want := range []string{`hx-swap-oob="true"`, "patched → todo", `hx-post="/todos/td_1/claim"`, `hx-target="closest li"`} {
		if !strings.Contains(row, want) {
			t.Errorf("pending row: missing %q in %q", want, row)
		}
	}

	claimed := pending
	claimed.TodoState, claimed.TodoOwner = "claimed", "op:h1"
	if got := renderFrag(t, h, "feed_row", feedRowFromEvent(claimed, true)); !strings.Contains(got, "claimed · operator") {
		t.Errorf("claimed row: missing operator stage in %q", got)
	}

	done := pending
	done.TodoState = "done"
	got := renderFrag(t, h, "feed_row", feedRowFromEvent(done, true))
	if !strings.Contains(got, "done ✓") {
		t.Errorf("done row: missing done stage in %q", got)
	}
	if strings.Contains(got, "/claim") {
		t.Errorf("done row must not offer Claim: %q", got)
	}
}

// A todo without an originating event (queue adapter / dev seed) still renders a coherent row
// keyed by its todo id under the queue trust mode.
func TestFeedRowFromEventlessTodo(t *testing.T) {
	h := newTestHandler(t)
	row := h.feedRowFromTodo(t.Context(), store.Todo{
		ID: "td_dev", Source: "redis", Kind: "job", State: "pending", CreatedAt: time.Now(),
	}, true)
	if row.RowID != "sb-td-td_dev" || row.TrustMode != "queue" {
		t.Fatalf("eventless row = %+v", row)
	}
	out := renderFrag(t, h, "feed_row", row)
	if !strings.Contains(out, "sb-badge--queue") || !strings.Contains(out, `id="sb-td-td_dev"`) {
		t.Errorf("eventless todo row wrong: %q", out)
	}
}

func TestCountsFragmentCarriesOOBBundle(t *testing.T) {
	h := newTestHandler(t)
	out := renderFrag(t, h, "counts", countsView{Tiles: tilesView{
		Stats: store.BoardStats{InFlight: 2, AwaitingClaim: 5, VerifiedPct: 80, EventsPerMin: 3},
		Bars:  activityBars([]int{1, 0, 4}),
		OOB:   true,
	}})
	for _, want := range []string{
		`id="sb-tiles"`, `id="sb-todo-count"`, `id="sb-live"`, // the three swap targets
		"sb-tile--alert", // awaiting-claim emphasis travels with the fragment
		"LIVE · 3/min",   // pill rate
		">5</span>",      // rail count
		"sb-bars__bar--now",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("counts: missing %q in %q", want, out)
		}
	}
	if got := strings.Count(out, `hx-swap-oob="true"`); got != 3 {
		t.Errorf("counts: %d oob swaps, want 3 (tiles + count + pill):\n%q", got, out)
	}
}

func TestToastFragmentTargetsToastRegion(t *testing.T) {
	h := newTestHandler(t)
	out := renderFrag(t, h, "toast", "td_8f2a · lease expired, re-surfaced to queue")
	for _, want := range []string{`hx-swap-oob="afterbegin:#sb-toasts"`, `class="sb-toast"`, `role="status"`, "re-surfaced"} {
		if !strings.Contains(out, want) {
			t.Errorf("toast: missing %q in %q", want, out)
		}
	}
}

// Ordering: for one delivery the event hook fires before the todo hook (store contract) and the
// single live worker must preserve that order on the wire, or a stage update could target a feed
// row that does not exist yet.
func TestLivePublishPreservesLifecycleOrder(t *testing.T) {
	h := newTestHandler(t)
	ch, cancel, err := h.events.subscribe("s")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer cancel()

	evID := int64(41)
	h.PublishEventReceived(store.EventSummary{ID: evID, Source: "github", EventType: "push", TrustMode: "signed", ReceivedAt: time.Now()})
	h.PublishTodoTransition("created", store.Todo{ID: "td_41", Source: "github", Kind: "push", State: "pending", EventID: &evID, CreatedAt: time.Now()})

	var names []string
	timeout := time.After(2 * time.Second)
	for len(names) < 2 {
		select {
		case e := <-ch:
			names = append(names, e.Name)
		case <-timeout:
			t.Fatalf("timed out; got %v", names)
		}
	}
	if names[0] != "event_received" || names[1] != "todo_created" {
		t.Fatalf("order = %v, want [event_received todo_created]", names)
	}
}

func TestTodoResurfacedCarriesToast(t *testing.T) {
	h := newTestHandler(t)
	ch, cancel, err := h.events.subscribe("s")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer cancel()

	h.PublishTodoTransition("pending", store.Todo{ID: "td_8f2a99", Source: "stripe", Kind: "invoice", State: "pending", CreatedAt: time.Now()})
	select {
	case e := <-ch:
		if e.Name != "todo_resurfaced" {
			t.Fatalf("event name = %q, want todo_resurfaced", e.Name)
		}
		if !strings.Contains(e.Data, "re-surfaced to queue") || !strings.Contains(e.Data, shortID("td_8f2a99")) {
			t.Fatalf("resurface toast missing from payload: %q", e.Data)
		}
		if !strings.Contains(e.Data, "patched → todo") {
			t.Fatalf("resurfaced row must return to the claimable stage: %q", e.Data)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no frame delivered")
	}
}

// endpoint_seen completes the SPEC-0013 typed taxonomy: a committed last-seen stamp becomes a
// named frame carrying the OOB last-seen refresh for that endpoint's swap target.
func TestPublishEndpointSeenCarriesOOBStamp(t *testing.T) {
	h := newTestHandler(t)
	ch, cancel, err := h.events.subscribe("s")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer cancel()

	h.PublishEndpointSeen("e1", time.Now())
	select {
	case e := <-ch:
		if e.Name != "endpoint_seen" {
			t.Fatalf("event name = %q, want endpoint_seen", e.Name)
		}
		for _, want := range []string{`id="sb-ep-seen-e1"`, `hx-swap-oob="true"`, "seen just now"} {
			if !strings.Contains(e.Data, want) {
				t.Fatalf("endpoint_seen payload missing %q: %q", want, e.Data)
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no endpoint_seen frame delivered")
	}
}

func TestEndpointSeenFragmentInlineVariant(t *testing.T) {
	h := newTestHandler(t)
	out := renderFrag(t, h, "endpoint_seen", endpointSeenView{ID: "e2", SeenAt: time.Now().Add(-5 * time.Minute)})
	if !strings.Contains(out, `id="sb-ep-seen-e2"`) || !strings.Contains(out, "seen 5m ago") {
		t.Errorf("inline endpoint_seen wrong: %q", out)
	}
	if strings.Contains(out, "hx-swap-oob") {
		t.Errorf("inline (server-rendered) variant must not carry hx-swap-oob: %q", out)
	}
}

func TestUnknownVerbIsDropped(t *testing.T) {
	h := newTestHandler(t)
	ch, cancel, err := h.events.subscribe("s")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer cancel()
	h.PublishTodoTransition("exploded", store.Todo{ID: "td_x"})
	select {
	case e := <-ch:
		t.Fatalf("unexpected frame for unknown verb: %+v", e)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestLiveHelpers(t *testing.T) {
	if got := ownerLabel("op:1234"); got != "operator" {
		t.Errorf("ownerLabel(op:) = %q", got)
	}
	if got := ownerLabel("agent:0a1b2c3d-4e5f"); got != "agent · 0a1b2c3" {
		t.Errorf("ownerLabel(agent:) = %q", got)
	}
	if got := ownerLabel(""); got != "" {
		t.Errorf("ownerLabel(empty) = %q", got)
	}
	if got := shortID("td_8f2a99aa"); got != "td_8f2a" {
		t.Errorf("shortID = %q", got)
	}

	bars := activityBars([]int{0, 5, 10})
	if len(bars) != 3 {
		t.Fatalf("bars len = %d", len(bars))
	}
	if bars[0].Pct != 4 { // floor keeps quiet minutes visible
		t.Errorf("empty bucket pct = %d, want floor 4", bars[0].Pct)
	}
	if bars[1].Pct != 50 || bars[2].Pct != 100 {
		t.Errorf("scaled pcts = %d, %d", bars[1].Pct, bars[2].Pct)
	}
	if !bars[2].Current || bars[0].Current {
		t.Error("only the last bucket is current")
	}
	if activityBars(nil) != nil {
		t.Error("no buckets → no bars")
	}
}
