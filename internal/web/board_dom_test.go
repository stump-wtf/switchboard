package web

// Companion coverage for the Board view (issue #102) that the feature story's bundled tests do not
// assert directly: that BOTH live regions exist in the initial DOM before any SSE frame, that trust
// badges carry human-readable TEXT (not just a modifier class), that the Board exposes the exact
// DOM hooks the presentation-JS layer (static/sb.js) queries, and that every verb in the typed SSE
// taxonomy has a matching sse-swap subscription on the rendered page.
// Governing: SPEC-0013 REQ "Board View — Live Incoming Lines", REQ "Live Updates and Toasts",
// REQ "Information Architecture and Navigation".

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/joestump/switchboard/internal/store"
)

// boardBody renders the Board with a connected shell and one seeded pending row — enough to force
// the feed, tiles, toast region, and SSE sink to all render.
func boardBody(t *testing.T) string {
	t.Helper()
	h := newTestHandler(t)
	return renderPage(t, h, "board", view{
		Title: "The Board", Human: testHuman(), CSRF: "tok",
		Shell: shell{Active: "board", TodoCount: 1, LiveRate: 4, DBConnected: true, Initials: "JS"},
		Tiles: tilesView{Stats: store.BoardStats{AwaitingClaim: 1, VerifiedPct: 50, EventsPerMin: 4}, Bars: activityBars([]int{1, 2})},
		Rows: []feedRow{feedRowFromEvent(store.EventSummary{
			ID: 1, Source: "github", EventType: "push", TrustMode: "signed",
			ReceivedAt: time.Now().Add(-1 * time.Minute), TodoID: "td_1", TodoState: "pending",
		}, false)},
	})
}

// The feed and the toast region are both aria-live regions that MUST exist in the DOM before the
// first SSE frame — htmx swaps rows/toasts into them and screen readers announce the changes. A
// live region created only on first event would silently swallow the first announcement.
func TestBoardLiveRegionsPresentInInitialDOM(t *testing.T) {
	body := boardBody(t)
	// Feed live region.
	if !regexp.MustCompile(`<ul id="sb-feed"[^>]*aria-live="polite"`).MatchString(body) {
		t.Error("feed region (#sb-feed) must carry aria-live=\"polite\" in the initial DOM")
	}
	// Toast live region.
	if !regexp.MustCompile(`<div id="sb-toasts"[^>]*aria-live="polite"`).MatchString(body) {
		t.Error("toast region (#sb-toasts) must carry aria-live=\"polite\" in the initial DOM")
	}
	// Count-pill live regions (SPEC-0013 "Dynamic Content Regions": count pills too, #186): the
	// stat band and the rail's Todos badge update via SSE counts frames, so they must be polite
	// live regions in the initial DOM as well.
	if !regexp.MustCompile(`<section id="sb-tiles"[^>]*aria-live="polite"`).MatchString(body) {
		t.Error("stat band (#sb-tiles) must carry aria-live=\"polite\" in the initial DOM")
	}
	if !regexp.MustCompile(`<span id="sb-todo-count"[^>]*aria-live="polite"`).MatchString(body) {
		t.Error("rail badge (#sb-todo-count) must carry aria-live=\"polite\" in the initial DOM")
	}
	// Exactly the four aria-live regions the design calls for (feed + toasts + the two count
	// regions above) — no more, no fewer, so a stray live region cannot start double-announcing.
	if n := strings.Count(body, `aria-live="polite"`); n != 4 {
		t.Errorf("board has %d aria-live regions, want exactly 4 (feed + toasts + tiles + rail count)", n)
	}
}

// Trust badges must carry the trust mode as readable TEXT, not merely a color-coding class — the
// design record's legend and every feed row name the mode in words so trust is legible without
// relying on color alone.
func TestTrustBadgesCarryText(t *testing.T) {
	h := newTestHandler(t)
	for _, mode := range []string{"signed", "token", "open", "queue"} {
		row := renderFrag(t, h, "feed_row", feedRowFromEvent(store.EventSummary{
			ID: 7, Source: "stripe", EventType: "invoice.paid", TrustMode: mode, ReceivedAt: time.Now(),
		}, false))
		// Both the class modifier AND the word, in the same badge span.
		wantBadge := regexp.MustCompile(`<span class="sb-badge sb-badge--` + mode + `">` + mode + `</span>`)
		if !wantBadge.MatchString(row) {
			t.Errorf("trust mode %q: badge must carry both the class and the text, got %q", mode, row)
		}
	}
	// The Board's trust legend also spells out every mode, prefixed with the design record's ●
	// dot — aria-hidden so AT still reads just the mode word (color/glyph never carry alone).
	body := boardBody(t)
	for _, mode := range []string{"signed", "token", "open", "queue"} {
		if !strings.Contains(body, `sb-badge--`+mode+`"><span aria-hidden="true">● </span>`+mode+`<`) {
			t.Errorf("trust legend missing ● dot prefix + readable text for %q", mode)
		}
	}
}

// TestBoardHeaderCarriesReaperPillAndFeedHint pins the #179 board-panel polish: the amber reaper
// pill renders in the Board header chrome (the Board summarizes the same durable queue the reaper
// re-surfaces into), and the Incoming-lines section head carries the design record's right-aligned
// 'newest first · live' hint. Governing: SPEC-0013 REQ "Todos View — Durable Queue" ("the view
// MUST surface that the reaper is active"), REQ "Board View — Live Incoming Lines".
func TestBoardHeaderCarriesReaperPillAndFeedHint(t *testing.T) {
	body := boardBody(t)
	for _, want := range []string{
		`class="sb-reaper"`,
		`class="sb-reaper__dot" aria-hidden="true"`, // pulsing dot is decorative — AT reads the copy
		"lease reaper active · re-surfaces abandoned work",
		`class="sb-section-hint">newest first · live<`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("board header/feed hint: missing %q", want)
		}
	}
}

// TestTilesCarryDesignSublabels pins the #179 stat-tile sublabels: the three count tiles read
// 'in flight · being worked', 'awaiting claim', and 'verified sources' under the bare value, per
// the design record's board panel (the throughput tile keeps its events/min unit).
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

// The presentation-JS layer (static/sb.js) is a thin, cosmetic layer that queries specific DOM
// hooks: it trims the feed to [data-sb-feed-cap], toggles [data-sb-feed-empty], and expires toasts
// under #sb-toasts. There is no browser in CI, so we guard the template↔JS contract here: if these
// hooks are renamed or dropped, the JS silently no-ops and this test fails instead.
// Governing: SPEC-0013 REQ "Live Updates and Toasts" (design.md "thin vanilla-JS layer").
func TestBoardExposesPresentationJSContract(t *testing.T) {
	body := boardBody(t)
	// Feed cap hook, with a positive integer value sb.js can parse.
	m := regexp.MustCompile(`data-sb-feed-cap="(\d+)"`).FindStringSubmatch(body)
	if m == nil {
		t.Error("feed missing data-sb-feed-cap hook that sb.js trims against")
	} else if m[1] == "0" {
		t.Errorf("data-sb-feed-cap must be a positive cap, got %q", m[1])
	}
	if !strings.Contains(body, "data-sb-feed-empty") {
		t.Error("empty-state element missing data-sb-feed-empty hook sb.js toggles")
	}
	if !strings.Contains(body, `id="sb-toasts"`) {
		t.Error("toast region #sb-toasts missing — sb.js schedules TTL expiry on its children")
	}
	// The layout links the helper so the hooks are actually driven.
	if !strings.Contains(body, "/static/sb.js") {
		t.Error("layout must link /static/sb.js")
	}
}

// Every verb in the typed SSE taxonomy (live.go sseEventNames) plus the two non-transition frames
// (event_received, counts) must have a matching sse-swap subscription in the rendered Board, or a
// published frame would arrive with no swap target. Deriving the expectation from the map (rather
// than a hardcoded string) makes this fail closed: adding a verb to the taxonomy without wiring its
// subscription breaks the test.
func TestSSETaxonomyHasSwapTargetsOnBoard(t *testing.T) {
	body := boardBody(t)
	// Collect every event name subscribed via sse-swap="a,b,c" attributes on the page.
	subscribed := map[string]bool{}
	for _, m := range regexp.MustCompile(`sse-swap="([^"]+)"`).FindAllStringSubmatch(body, -1) {
		for _, name := range strings.Split(m[1], ",") {
			subscribed[strings.TrimSpace(name)] = true
		}
	}
	want := []string{"event_received", "counts"}
	for _, v := range sseEventNames {
		want = append(want, v)
	}
	for _, name := range want {
		if !subscribed[name] {
			t.Errorf("SSE event %q has no sse-swap subscription on the Board (subscribed: %v)", name, subscribed)
		}
	}
}
