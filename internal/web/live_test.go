package web

// Typed SSE taxonomy coverage for the patch-panel board (SPEC-0015 REQ "Patch Panel Board", REQ
// "Live Fragment Architecture"): lane cards render standalone, lane movement is an OOB removal +
// insertion pair targeting the lane lists by id, the ephemeral received-lane events (ingest
// instrumentation) correlate with the committed todo_created frame that advances the card, and
// the ordered publish queue preserves the lane_received → todo_created lifecycle order for a
// single delivery.

import (
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/switchboard/internal/store"
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

// recvFrame pulls the next SSE frame off a subscription or fails the test.
func recvFrame(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case e := <-ch:
		return e
	case <-time.After(2 * time.Second):
		t.Fatal("no SSE frame delivered")
		return Event{}
	}
}

// TestLaneCardFragmentStates renders the card per state: the ephemeral verifying card (pulse +
// TTL stamp), the queued card (Claim action, hx-swap none — the response is pure OOB movement),
// the claimed/done cards, and the OOB insertion wiring into each lane list.
func TestLaneCardFragmentStates(t *testing.T) {
	h := newTestHandler(t)

	verifying := inflightCard("github", "push", "signed", "gh-delivery-1", time.Now())
	out := renderFrag(t, h, "lane_card", verifying)
	for _, want := range []string{
		`id="` + rxCardID("github", "gh-delivery-1") + `"`,
		`data-sb-lane-card="received"`,
		`data-sb-ephemeral="60000"`,
		"sb-lcard__pulse", "verifying",
		"sb-badge--signed", `sb-icon-wrap sb-icon-wrap--signed`,
		"/static/icons/brands/github.svg", // brand SVG, not the two-letter GH tag
		"checking signature",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("verifying card: missing %q in %q", want, out)
		}
	}
	if strings.Contains(out, "hx-swap-oob") {
		t.Error("non-OOB card must not carry hx-swap-oob")
	}

	queued := laneCard{DomID: "sb-td-td_1", Lane: laneVerified, Source: "github", Kind: "push",
		Title: "PR #4127 opened", TrustMode: "signed", State: "pending", TodoID: "td_1",
		Detail: "idem ok", At: time.Now(), OOB: true}
	out = renderFrag(t, h, "lane_card", queued)
	for _, want := range []string{
		`id="sb-td-td_1"`,
		`hx-swap-oob="afterbegin:#sb-lane-verified-cards"`,
		">queued<", // pending renders as the design's queued chip
		"PR #4127 opened",
		`hx-post="/todos/td_1/claim"`, `hx-swap="none"`,
		"idem ok",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("queued card: missing %q in %q", want, out)
		}
	}
	if strings.Contains(out, "data-sb-ephemeral") {
		t.Error("durable card must not carry an ephemeral TTL")
	}

	claimed := queued
	claimed.State, claimed.Lane, claimed.OwnerLabel, claimed.Detail = "claimed", lanePatched, "operator", "claimed · operator"
	out = renderFrag(t, h, "lane_card", claimed)
	for _, want := range []string{`hx-swap-oob="afterbegin:#sb-lane-patched-cards"`, ">claimed<", "claimed · operator"} {
		if !strings.Contains(out, want) {
			t.Errorf("claimed card: missing %q in %q", want, out)
		}
	}

	done := claimed
	done.State, done.Detail = "done", "done ✓ · ack sent"
	out = renderFrag(t, h, "lane_card", done)
	if !strings.Contains(out, ">done<") {
		t.Errorf("done card: missing state chip in %q", out)
	}
	if strings.Contains(out, "/claim") {
		t.Errorf("done card must not offer Claim: %q", out)
	}
}

// TestLaneMoveFragmentCarriesRemovalAndInsertion pins the SPEC-0015 "Live Fragment Architecture"
// wire shape: one lane movement = an OOB delete of the previous node by id + an OOB afterbegin
// insertion into the destination lane. With no RemoveID the payload is a pure insertion.
func TestLaneMoveFragmentCarriesRemovalAndInsertion(t *testing.T) {
	h := newTestHandler(t)
	card := laneCard{DomID: "sb-td-td_9", Lane: lanePatched, Source: "github", State: "claimed",
		TrustMode: "signed", At: time.Now(), OOB: true}
	out := renderFrag(t, h, "lane_move", laneMove{RemoveID: "sb-td-td_9", Card: card})
	if !strings.Contains(out, `<li id="sb-td-td_9" hx-swap-oob="delete"></li>`) {
		t.Errorf("lane_move: missing OOB removal in %q", out)
	}
	if !strings.Contains(out, `hx-swap-oob="afterbegin:#sb-lane-patched-cards"`) {
		t.Errorf("lane_move: missing OOB insertion in %q", out)
	}

	pure := renderFrag(t, h, "lane_move", laneMove{Card: card})
	if strings.Contains(pure, `hx-swap-oob="delete"`) {
		t.Errorf("lane_move without RemoveID must be a pure insertion: %q", pure)
	}
}

// TestDeliveryReceivedPublishesEphemeralCard: the ingest arrival instrument becomes a lane_received
// frame inserting the in-flight card into the received lane — ephemeral (client TTL), SSE-only.
func TestDeliveryReceivedPublishesEphemeralCard(t *testing.T) {
	h := newTestHandler(t)
	ch, cancel, err := h.events.subscribe("h1", "s")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer cancel()

	h.DeliveryReceived("github", "push", "signed", "gh-d-1")
	e := recvFrame(t, ch)
	if e.Name != "lane_received" {
		t.Fatalf("event name = %q, want lane_received", e.Name)
	}
	for _, want := range []string{
		`id="` + rxCardID("github", "gh-d-1") + `"`,
		`hx-swap-oob="afterbegin:#sb-lane-received-cards"`,
		"data-sb-ephemeral",
		"checking signature",
	} {
		if !strings.Contains(e.Data, want) {
			t.Errorf("lane_received payload missing %q: %q", want, e.Data)
		}
	}
}

// TestDeliveryRejectedPublishesRedactedResolution: a failed verification resolves the in-flight
// card — OOB removal of the SAME node the arrival inserted plus a transient, redacted rejection
// card. Only the client-safe reason travels; no signature, token, or payload detail exists to leak
// (SPEC-0001 rejection doctrine unchanged; SPEC-0015 scenario "Rejected caller").
func TestDeliveryRejectedPublishesRedactedResolution(t *testing.T) {
	h := newTestHandler(t)
	ch, cancel, err := h.events.subscribe("h1", "s")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer cancel()

	h.DeliveryRejected("github", "push", "signed", "gh-d-2", "signature verification failed")
	e := recvFrame(t, ch)
	if e.Name != "lane_rejected" {
		t.Fatalf("event name = %q, want lane_rejected", e.Name)
	}
	id := rxCardID("github", "gh-d-2")
	for _, want := range []string{
		`<li id="` + id + `" hx-swap-oob="delete"></li>`,   // removal of the in-flight card
		`hx-swap-oob="afterbegin:#sb-lane-received-cards"`, // transient rejection surface, same lane
		">rejected<",
		"signature verification failed",
		`data-sb-ephemeral="8000"`, // transient by design
	} {
		if !strings.Contains(e.Data, want) {
			t.Errorf("lane_rejected payload missing %q: %q", want, e.Data)
		}
	}
}

// TestDeliveryDedupedPublishesTransientResolution: an idempotent redelivery resolves the in-flight
// card without a lane advance — the durable card it collapsed onto already sits in its lane.
func TestDeliveryDedupedPublishesTransientResolution(t *testing.T) {
	h := newTestHandler(t)
	ch, cancel, err := h.events.subscribe("h1", "s")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer cancel()

	h.DeliveryDeduped("stripe", "invoice.paid", "signed", "evt_1")
	e := recvFrame(t, ch)
	if e.Name != "lane_deduped" {
		t.Fatalf("event name = %q, want lane_deduped", e.Name)
	}
	for _, want := range []string{">deduped<", "collapsed onto an existing todo", `data-sb-ephemeral="8000"`} {
		if !strings.Contains(e.Data, want) {
			t.Errorf("lane_deduped payload missing %q: %q", want, e.Data)
		}
	}
}

// TestSignedWebhookCrossesTheBoard walks the SPEC-0015 scenario "A signed webhook crosses the
// board" at the typed-event layer: arrival inserts the received card; the committed todo_created
// frame REMOVES that exact node (correlated by source + idempotency key) and inserts the durable
// card into verified; the claim frame moves it verified → patched through. Ordering holds because
// the single live worker drains in enqueue order.
func TestSignedWebhookCrossesTheBoard(t *testing.T) {
	h := newTestHandler(t)
	ch, cancel, err := h.events.subscribe("h1", "s")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer cancel()

	const key = "gh-delivery-42"
	h.DeliveryReceived("github", "push", "signed", key)
	h.PublishTodoTransition("created", store.Todo{
		ID: "td_42", Source: "github", Kind: "push", Title: "github push in joestump/switchboard",
		State: "pending", IdempotencyKey: key, CreatedAt: time.Now(),
	})
	h.PublishTodoTransition("claimed", store.Todo{
		ID: "td_42", Source: "github", Kind: "push", State: "claimed", Owner: "op:h1",
		IdempotencyKey: key, CreatedAt: time.Now(),
	})

	rx := rxCardID("github", key)

	received := recvFrame(t, ch)
	if received.Name != "lane_received" {
		t.Fatalf("frame 1 = %q, want lane_received", received.Name)
	}
	if !strings.Contains(received.Data, `id="`+rx+`"`) {
		t.Fatalf("received card id mismatch: %q", received.Data)
	}

	created := recvFrame(t, ch)
	if created.Name != "todo_created" {
		t.Fatalf("frame 2 = %q, want todo_created", created.Name)
	}
	for _, want := range []string{
		`<li id="` + rx + `" hx-swap-oob="delete"></li>`, // the in-flight card leaves received
		`id="sb-td-td_42"`,
		`hx-swap-oob="afterbegin:#sb-lane-verified-cards"`, // …and the durable card enters verified
		">queued<",
	} {
		if !strings.Contains(created.Data, want) {
			t.Errorf("todo_created payload missing %q: %q", want, created.Data)
		}
	}

	claimed := recvFrame(t, ch)
	if claimed.Name != "todo_claimed" {
		t.Fatalf("frame 3 = %q, want todo_claimed", claimed.Name)
	}
	for _, want := range []string{
		`<li id="sb-td-td_42" hx-swap-oob="delete"></li>`, // leaves verified…
		`hx-swap-oob="afterbegin:#sb-lane-patched-cards"`, // …and enters patched through
		"claimed · operator",
		"td_42 · claimed · operator", // background-transition toast
	} {
		if !strings.Contains(claimed.Data, want) {
			t.Errorf("todo_claimed payload missing %q: %q", want, claimed.Data)
		}
	}
}

// TestTodoResurfacedReturnsCardToVerified: the reaper's re-surface moves the card back from
// patched through into verified (claimable again) and announces itself with a toast.
func TestTodoResurfacedReturnsCardToVerified(t *testing.T) {
	h := newTestHandler(t)
	ch, cancel, err := h.events.subscribe("h1", "s")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer cancel()

	h.PublishTodoTransition("pending", store.Todo{ID: "td_8f2a99", Source: "stripe", Kind: "invoice", State: "pending", CreatedAt: time.Now()})
	e := recvFrame(t, ch)
	if e.Name != "todo_resurfaced" {
		t.Fatalf("event name = %q, want todo_resurfaced", e.Name)
	}
	for _, want := range []string{
		`<li id="sb-td-td_8f2a99" hx-swap-oob="delete"></li>`,
		`hx-swap-oob="afterbegin:#sb-lane-verified-cards"`,
		">queued<",
		"re-surfaced to queue", shortID("td_8f2a99"),
	} {
		if !strings.Contains(e.Data, want) {
			t.Errorf("todo_resurfaced payload missing %q: %q", want, e.Data)
		}
	}
}

// TestEventlessTodoCardFallsBackToQueueTrust: a todo without an originating event (an operator
// push or a dev seed) still renders a coherent card keyed by its todo id under the queue trust mode.
func TestEventlessTodoCardFallsBackToQueueTrust(t *testing.T) {
	h := newTestHandler(t)
	card := h.laneCardFromTodo(t.Context(), store.Todo{
		ID: "td_dev", Source: "ops", Kind: "job", State: "pending", CreatedAt: time.Now(),
	})
	if card.DomID != "sb-td-td_dev" || card.TrustMode != "queue" || card.Lane != laneVerified {
		t.Fatalf("eventless card = %+v", card)
	}
	out := renderFrag(t, h, "lane_card", card)
	if !strings.Contains(out, "sb-badge--queue") || !strings.Contains(out, `id="sb-td-td_dev"`) {
		t.Errorf("eventless todo card wrong: %q", out)
	}
}

func TestCountsFragmentCarriesOOBBundle(t *testing.T) {
	h := newTestHandler(t)
	todos := store.TodoCounts{All: 9, Pending: 5, Claimed: 2, Done: 1, Failed: 1}
	out := renderFrag(t, h, "counts", countsView{
		Tiles: tilesView{
			Stats: store.BoardStats{InFlight: 2, AwaitingClaim: 5, VerifiedPct: 80, EventsPerMin: 3},
			Bars:  activityBars([]int{1, 0, 4}),
			OOB:   true,
		},
		Todos: todos,
		Lanes: laneCountsFrom(todos),
	})
	for _, want := range []string{
		`id="sb-tiles"`, `id="sb-todo-count"`, `id="sb-live"`, // Board swap targets
		`id="sb-tc-all"`, `id="sb-tc-pending"`, `id="sb-tc-failed"`, // Todos view pill-count targets
		// Lane-header counts ride the same frame (SPEC-0015 "lane headers SHALL carry live counts").
		`id="sb-lane-count-verified" class="sb-lane__n" hx-swap-oob="true">5<`,
		`id="sb-lane-count-patched" class="sb-lane__n" hx-swap-oob="true">4<`,
		"sb-tile--alert", // awaiting-claim emphasis travels with the fragment
		"LIVE · 3/min",   // pill rate
		`id="sb-todo-count" class="sb-rail__count" aria-live="polite" hx-swap-oob="true">9<`, // rail badge = TOTAL todos
		"sb-bars__bar--now",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("counts: missing %q in %q", want, out)
		}
	}
	// tiles + rail count + LIVE pill + 5 Todos pill counts + 2 lane counts = 10 OOB swaps.
	if got := strings.Count(out, `hx-swap-oob="true"`); got != 10 {
		t.Errorf("counts: %d oob swaps, want 10 (tiles + count + pill + 5 filter counts + 2 lane counts):\n%q", got, out)
	}
}

func TestToastFragmentTargetsToastRegion(t *testing.T) {
	h := newTestHandler(t)
	out := renderFrag(t, h, "toast", toastMsg{Kind: "resurfaced", Text: "td_8f2a · lease expired, re-surfaced to queue"})
	for _, want := range []string{`hx-swap-oob="afterbegin:#sb-toasts"`, `sb-toast--resurfaced`, `role="status"`, "re-surfaced", "sb-toast__icon"} {
		if !strings.Contains(out, want) {
			t.Errorf("toast: missing %q in %q", want, out)
		}
	}
}

// endpoint_seen completes the typed taxonomy: a committed last-seen stamp becomes a named frame
// carrying the OOB last-seen refresh for that endpoint's swap target.
func TestPublishEndpointSeenCarriesOOBStamp(t *testing.T) {
	h := newTestHandler(t)
	ch, cancel, err := h.events.subscribe("h1", "s")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer cancel()

	h.PublishEndpointSeen("e1", time.Now())
	e := recvFrame(t, ch)
	if e.Name != "endpoint_seen" {
		t.Fatalf("event name = %q, want endpoint_seen", e.Name)
	}
	for _, want := range []string{`id="sb-ep-seen-e1"`, `hx-swap-oob="true"`, "seen just now"} {
		if !strings.Contains(e.Data, want) {
			t.Fatalf("endpoint_seen payload missing %q: %q", want, e.Data)
		}
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
	ch, cancel, err := h.events.subscribe("h1", "s")
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

	// Lane semantics (SPEC-0015): pending = verified (durable, unclaimed); everything claimed or
	// beyond = patched through.
	if laneForState("pending") != laneVerified {
		t.Error("pending todos live in the verified lane")
	}
	for _, s := range []string{"claimed", "done", "failed"} {
		if laneForState(s) != lanePatched {
			t.Errorf("%s todos live in the patched-through lane", s)
		}
	}
	if laneStateLabel("pending") != "queued" || laneStateLabel("claimed") != "claimed" {
		t.Error("state chip labels: pending reads queued, others read themselves")
	}
	if c := laneCountsFrom(store.TodoCounts{Pending: 5, Claimed: 2, Done: 1, Failed: 1}); c.Verified != 5 || c.Patched != 4 {
		t.Errorf("laneCountsFrom = %+v", c)
	}

	// rxCardID is deterministic (arrival and resolution frames must target the same node) and
	// distinct across providers sharing a key.
	first, second := rxCardID("github", "k1"), rxCardID("github", "k1")
	if first != second {
		t.Error("rxCardID must be deterministic")
	}
	if rxCardID("github", "k1") == rxCardID("stripe", "k1") {
		t.Error("rxCardID must scope the key to its provider")
	}
	if !strings.HasPrefix(rxCardID("github", "k1"), "sb-rx-") {
		t.Errorf("rxCardID prefix: %q", rxCardID("github", "k1"))
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
