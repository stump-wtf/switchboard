package web

// DOM contract for the three-lane patch-panel board (SPEC-0015 REQ "Patch Panel Board", REQ
// "Live Fragment Architecture"): the lanes and their SSE insertion targets exist in the initial
// DOM before any frame, lane headers carry counts, trust badges carry readable TEXT, the
// presentation-JS hooks (data-sb-*) the sb-live.js module queries are all present, and every
// event in the typed SSE taxonomy has a matching sse-swap subscription on the rendered page.
// Assertions key on data-sb-* attributes and ids, never on classes alone.

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/joestump/switchboard/internal/store"
)

// boardBody renders the Board with a connected shell and one card in each persisted lane — enough
// to force the lanes, tiles, toast region, and SSE sinks to all render.
func boardBody(t *testing.T) string {
	t.Helper()
	h := newTestHandler(t)
	queued := laneCardFromItem(store.TodoItem{Todo: store.Todo{
		ID: "td_1", Queue: "reviews", Source: "github", Kind: "push", Title: "github push",
		State: "pending", IdempotencyKey: "gh-d-1", CreatedAt: time.Now().Add(-1 * time.Minute),
	}, TrustMode: "signed"})
	claimed := laneCardFromItem(store.TodoItem{Todo: store.Todo{
		ID: "td_2", Queue: "stripe", Source: "stripe", Kind: "invoice.paid", Title: "stripe invoice.paid",
		State: "claimed", Owner: "op:h1", CreatedAt: time.Now().Add(-3 * time.Minute),
	}, TrustMode: "signed"})
	return renderPage(t, h, "board", view{
		Title: "The Board", Human: testHuman(), CSRF: "tok",
		Shell: shell{Active: "board", TodoCount: 2, LiveRate: 4, DBConnected: true, Initials: "JS"},
		Tiles: tilesView{Stats: store.BoardStats{AwaitingClaim: 1, VerifiedPct: 50, EventsPerMin: 4}, Bars: activityBars([]int{1, 2})},
		Lanes: lanesView{Verified: []laneCard{queued}, Patched: []laneCard{claimed},
			Counts: laneCounts{Verified: 1, Patched: 1}},
	})
}

// TestBoardRendersThreeLanesWithInsertionTargets: all three lane card lists exist in the initial
// DOM with the exact ids the OOB lane movement targets (SPEC-0015: received/verified/patched
// through), the received lane renders EMPTY server-side (ephemeral, SSE-only), and each lane
// header carries its count hook.
func TestBoardRendersThreeLanesWithInsertionTargets(t *testing.T) {
	body := boardBody(t)
	// The three OOB insertion targets, present before any SSE frame.
	for _, id := range []string{"sb-lane-received-cards", "sb-lane-verified-cards", "sb-lane-patched-cards"} {
		if !strings.Contains(body, `id="`+id+`"`) {
			t.Errorf("lane insertion target #%s missing from the initial DOM", id)
		}
	}
	// Lane identity + JS hooks travel on data-sb-* attributes.
	for _, lane := range []string{"received", "verified", "patched"} {
		if !strings.Contains(body, `data-sb-lane-cards="`+lane+`"`) {
			t.Errorf("lane %q missing its data-sb-lane-cards hook", lane)
		}
		if !strings.Contains(body, `data-sb-lane-empty="`+lane+`"`) {
			t.Errorf("lane %q missing its data-sb-lane-empty hook", lane)
		}
	}
	// The received lane renders empty (no ephemeral card is ever server-rendered) and its count is
	// DOM-derived: the data-sb-lane-count hook starts at 0.
	if !regexp.MustCompile(`<ul id="sb-lane-received-cards"[^>]*>\s*</ul>`).MatchString(body) {
		t.Error("received lane must render EMPTY server-side — its cards are ephemeral (SSE-only)")
	}
	if !strings.Contains(body, `data-sb-lane-count="received">0<`) {
		t.Error("received lane count hook must start at 0 (DOM-derived by sb-live.js)")
	}
	// Verified/patched counts are the SSE lane_counts swap targets.
	if !strings.Contains(body, `id="sb-lane-count-verified"`) || !strings.Contains(body, `id="sb-lane-count-patched"`) {
		t.Error("verified/patched lane-header counts must render with their OOB swap-target ids")
	}
	// The durable cards render in their lanes with their stable movement ids.
	if !strings.Contains(body, `id="sb-td-td_1"`) || !strings.Contains(body, `id="sb-td-td_2"`) {
		t.Error("durable lane cards must carry their stable sb-td-<id> movement ids")
	}
	// The pending card offers Claim as a pure-OOB action (the response is lane movement).
	if !strings.Contains(body, `hx-post="/todos/td_1/claim"`) {
		t.Error("queued card missing its Claim action")
	}
}

// TestBoardLiveRegionsPresentInInitialDOM: every region SSE frames swap into must exist (and be
// politely announced) before the first frame — the three lane lists, the toast region, the stat
// band, and the rail count. A live region created only on first event would silently swallow the
// first announcement.
func TestBoardLiveRegionsPresentInInitialDOM(t *testing.T) {
	body := boardBody(t)
	for _, re := range []string{
		`<ul id="sb-lane-received-cards"[^>]*aria-live="polite"`,
		`<ul id="sb-lane-verified-cards"[^>]*aria-live="polite"`,
		`<ul id="sb-lane-patched-cards"[^>]*aria-live="polite"`,
		`<div id="sb-toasts"[^>]*aria-live="polite"`,
		`<section id="sb-tiles"[^>]*aria-live="polite"`,
		`<span id="sb-todo-count"[^>]*aria-live="polite"`,
	} {
		if !regexp.MustCompile(re).MatchString(body) {
			t.Errorf("live region missing or not polite: %s", re)
		}
	}
	// Exactly the six aria-live regions above — no stray region can start double-announcing.
	if n := strings.Count(body, `aria-live="polite"`); n != 6 {
		t.Errorf("board has %d aria-live regions, want exactly 6 (3 lanes + toasts + tiles + rail count)", n)
	}
}

// Trust badges must carry the trust mode as readable TEXT, not merely a color-coding class — the
// legend and every card name the mode in words so trust is legible without relying on color alone.
func TestTrustBadgesCarryText(t *testing.T) {
	h := newTestHandler(t)
	for _, mode := range []string{"signed", "token", "open", "queue"} {
		card := laneCardFromItem(store.TodoItem{Todo: store.Todo{
			ID: "td_7", Source: "stripe", Kind: "invoice.paid", State: "pending", CreatedAt: time.Now(),
		}, TrustMode: mode})
		out := renderFrag(t, h, "lane_card", card)
		// The badge carries the mode word (signed additionally gets its ✓ glyph, aria-hidden).
		want := regexp.MustCompile(`<span class="sb-badge sb-badge--` + mode + `">(<span aria-hidden="true">✓ </span>)?` + mode + `</span>`)
		if !want.MatchString(out) {
			t.Errorf("trust mode %q: badge must carry both the class and the text, got %q", mode, out)
		}
	}
	// The board's trust legend also spells out every mode, prefixed with the design record's ●
	// dot — aria-hidden so AT still reads just the mode word (color/glyph never carry alone).
	body := boardBody(t)
	for _, mode := range []string{"signed", "token", "open", "queue"} {
		if !strings.Contains(body, `sb-badge--`+mode+`"><span aria-hidden="true">● </span>`+mode+`<`) {
			t.Errorf("trust legend missing ● dot prefix + readable text for %q", mode)
		}
	}
}

// TestBoardHeaderCarriesReaperPill: the amber reaper pill renders in the Board header chrome (the
// board summarizes the same durable queue the reaper re-surfaces into).
func TestBoardHeaderCarriesReaperPill(t *testing.T) {
	body := boardBody(t)
	for _, want := range []string{
		`class="sb-reaper"`,
		`class="sb-reaper__dot" aria-hidden="true"`, // pulsing dot is decorative — AT reads the copy
		"lease reaper active · re-surfaces abandoned work",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("board header: missing %q", want)
		}
	}
}

// TestTilesCarryDesignSublabels pins the stat-tile sublabels: the three count tiles read
// 'in flight · being worked', 'awaiting claim', and 'verified sources' under the bare value
// (the throughput tile keeps its events/min unit).
func TestTilesCarryDesignSublabels(t *testing.T) {
	h := newTestHandler(t)
	out := renderFrag(t, h, "tiles", tilesView{Stats: store.BoardStats{InFlight: 2, AwaitingClaim: 1, VerifiedPct: 50}})
	for _, want := range []string{
		"in flight · being worked",
		"awaiting claim",
		"verified sources",
		"events/min",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("tiles: missing design sublabel %q in %q", want, out)
		}
	}
}

// The presentation-JS layer (static/js/sb-live.js) is a thin, cosmetic layer that queries specific
// DOM hooks: it trims each lane to [data-sb-lane-cap], toggles [data-sb-lane-empty], derives the
// received count into [data-sb-lane-count], expires [data-sb-ephemeral] cards, and expires toasts
// under #sb-toasts. There is no browser in CI, so we guard the template↔JS contract here: if these
// hooks are renamed or dropped, the JS silently no-ops and this test fails instead.
func TestBoardExposesPresentationJSContract(t *testing.T) {
	body := boardBody(t)
	// Per-lane cap hooks, with a positive integer value sb-live.js can parse.
	caps := regexp.MustCompile(`data-sb-lane-cap="(\d+)"`).FindAllStringSubmatch(body, -1)
	if len(caps) != 3 {
		t.Errorf("want 3 data-sb-lane-cap hooks (one per lane), got %d", len(caps))
	}
	for _, m := range caps {
		if m[1] == "0" {
			t.Errorf("data-sb-lane-cap must be a positive cap, got %q", m[1])
		}
	}
	if !strings.Contains(body, `data-sb-lane-count="received"`) {
		t.Error("received lane missing the data-sb-lane-count hook sb-live.js updates")
	}
	if !strings.Contains(body, `id="sb-toasts"`) {
		t.Error("toast region #sb-toasts missing — sb-live.js schedules TTL expiry on its children")
	}
	// Ephemeral cards stamp their TTL for sb-live.js expiry (rendered via the fragment set — the
	// page itself never server-renders one).
	h := newTestHandler(t)
	frag := renderFrag(t, h, "lane_card", inflightCard("github", "push", "signed", "k", time.Now()))
	if !regexp.MustCompile(`data-sb-ephemeral="\d+"`).MatchString(frag) {
		t.Error("ephemeral card missing the data-sb-ephemeral TTL stamp sb-live.js expires")
	}
	// The layout links the split helper modules so the hooks are actually driven.
	for _, js := range []string{"/static/js/sb-live.js", "/static/js/sb-overlay.js", "/static/js/sb-vend.js",
		"/static/js/sb-theme.js", "/static/js/sb-keys.js"} {
		if !strings.Contains(body, js) {
			t.Errorf("layout must link %s", js)
		}
	}
}

// Every verb in the typed SSE taxonomy (live.go sseEventNames) plus the ephemeral lane events and
// the counts frame must have a matching sse-swap subscription in the rendered Board, or a
// published frame would arrive with no swap processor. Deriving the todo_* expectation from the
// map (rather than a hardcoded string) makes this fail closed: adding a verb to the taxonomy
// without wiring its subscription breaks the test.
func TestSSETaxonomyHasSwapTargetsOnBoard(t *testing.T) {
	body := boardBody(t)
	// Collect every event name subscribed via sse-swap="a,b,c" attributes on the page.
	subscribed := map[string]bool{}
	for _, m := range regexp.MustCompile(`sse-swap="([^"]+)"`).FindAllStringSubmatch(body, -1) {
		for _, name := range strings.Split(m[1], ",") {
			subscribed[strings.TrimSpace(name)] = true
		}
	}
	want := []string{"lane_received", "lane_rejected", "lane_deduped", "counts"}
	for _, v := range sseEventNames {
		want = append(want, v)
	}
	for _, name := range want {
		if !subscribed[name] {
			t.Errorf("SSE event %q has no sse-swap subscription on the Board (subscribed: %v)", name, subscribed)
		}
	}
}
