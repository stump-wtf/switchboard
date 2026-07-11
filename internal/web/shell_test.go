package web

// Layout-shell conformance tests for the SPEC-0013 chrome, rendered from the embedded template FS:
// unique landmarks, single aria-current rail entry per view, present-but-empty toast/overlay
// regions, and a no-external-origin sweep over every page's rendered output.
// Governing: SPEC-0013 REQ "Information Architecture and Navigation", REQ "Design Language
// Conformance"; ADR-0001 (self-contained binary, same-origin assets).

import (
	"strings"
	"testing"
	"time"

	"github.com/joestump/switchboard/internal/store"
)

// allPages renders every registered page with representative data so sweeps cover the whole
// template surface, authenticated and not.
func allPages(t *testing.T, h *Handler) map[string]string {
	t.Helper()
	seen := time.Now().Add(-2 * time.Minute)
	card := endpointCard{ID: "e1", AgentName: "reviewer-bot", Slug: "reviewer-bot-ab12cd",
		CredPrefix: "sbk_ab12cd", Queues: []string{"reviews"}, Verbs: []string{"claim"}, State: "active", LastSeenAt: &seen}
	sh := shell{Active: "board", TodoCount: 1, LiveRate: 2, DBConnected: true, Initials: "JS"}
	views := map[string]view{
		"login": {Title: "Log in", OIDCConfigured: true},
		"board": {Title: "The Board", Human: testHuman(), CSRF: "tok", Shell: sh,
			Tiles: tilesView{Stats: store.BoardStats{TodosToday: 1, AwaitingClaim: 1, EventsPerMin: 2}, Bars: activityBars([]int{0, 1, 2, 1})},
			Rows: []feedRow{feedRowFromEvent(store.EventSummary{
				ID: 1, Source: "github", EventType: "push", TrustMode: "signed", ReceivedAt: time.Now(),
				TodoID: "td_1", TodoState: "pending"}, false)}},
		"endpoints": {Title: "Endpoints", Human: testHuman(), CSRF: "tok", Shell: shell{Active: "endpoints", Initials: "JS"},
			EndpointCards: []endpointCard{card}, VerbOptions: drainVerbs},
	}
	out := make(map[string]string, len(views))
	for page, v := range views {
		out[page] = renderPage(t, h, page, v)
	}
	return out
}

// TestLandmarksExactlyOnce: every page renders the banner (<header>) and content (<main>)
// landmarks exactly once; authenticated pages add exactly one navigation landmark, the login
// page none (SPEC-0013: the rail is chrome for signed-in operators).
func TestLandmarksExactlyOnce(t *testing.T) {
	h := newTestHandler(t)
	for page, body := range allPages(t, h) {
		wantNav := 1
		if page == "login" {
			wantNav = 0
		}
		for tag, want := range map[string]int{"<header": 1, "<main": 1, "<nav": wantNav} {
			if got := strings.Count(body, tag); got != want {
				t.Errorf("%s: %s landmark count = %d, want %d", page, tag, got, want)
			}
		}
	}
}

// TestRailMarksExactlyOneActiveItem: for each shell view, exactly one rail item carries
// aria-current="page", and it is the right one.
func TestRailMarksExactlyOneActiveItem(t *testing.T) {
	h := newTestHandler(t)
	for active, label := range map[string]string{"board": "Board", "todos": "Todos", "endpoints": "Endpoints"} {
		body := renderPage(t, h, "board", view{
			Title: "The Board", Human: testHuman(),
			Shell: shell{Active: active, DBConnected: true, Initials: "JS"},
		})
		const marker = `aria-current="page"`
		if got := strings.Count(body, marker); got != 1 {
			t.Errorf("active=%s: aria-current count = %d, want 1", active, got)
			continue
		}
		// The marked anchor must be the expected rail entry.
		item := body[strings.Index(body, marker):]
		if end := strings.Index(item, "</a>"); end >= 0 {
			item = item[:end]
		}
		if !strings.Contains(item, ">"+label+"<") {
			t.Errorf("active=%s: aria-current is not on the %q rail item: %q", active, label, item)
		}
	}
}

// TestRailPresentForCollapse: the rail renders with its stable .sb-rail hook (the class the 820px
// icons-only collapse in switchboard.css targets) whenever an operator is signed in.
func TestRailPresentForCollapse(t *testing.T) {
	h := newTestHandler(t)
	body := renderPage(t, h, "board", view{Title: "The Board", Human: testHuman(), Shell: shell{Active: "board"}})
	if !strings.Contains(body, `<nav class="sb-rail"`) {
		t.Error("authenticated shell missing the .sb-rail collapse hook")
	}
}

// TestToastAndOverlayRegionsInInitialDOM: the toast region (aria-live) and overlay slot exist,
// empty, in the initial DOM of every page, so later SSE/HTMX swaps land in announced regions
// rather than injecting them (SPEC-0012/0013).
func TestToastAndOverlayRegionsInInitialDOM(t *testing.T) {
	h := newTestHandler(t)
	for page, body := range allPages(t, h) {
		for _, want := range []string{
			`<div id="sb-overlay" class="sb-overlay" hidden></div>`,
			`<div id="sb-toasts" class="sb-toasts" aria-live="polite"></div>`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: initial DOM missing empty region %q", page, want)
			}
		}
	}
}

// TestNoExternalOriginsAnywhere sweeps the full rendered output of every page for http(s)://
// references. Only the SVG XML namespace (an identifier, not a fetch) and the configured
// same-origin base URL are permitted (SPEC-0013 "no external origins"; CSP default-src 'self').
func TestNoExternalOriginsAnywhere(t *testing.T) {
	h := newTestHandler(t)
	allowed := []string{
		`xmlns="http://www.w3.org/2000/svg"`, // namespace identifier, never fetched
		"https://sb.example.com",             // the configured BaseURL (same origin)
	}
	for page, body := range allPages(t, h) {
		scrubbed := body
		for _, a := range allowed {
			scrubbed = strings.ReplaceAll(scrubbed, a, "")
		}
		for _, scheme := range []string{"http://", "https://", "//cdn", "integrity="} {
			if idx := strings.Index(scrubbed, scheme); idx >= 0 {
				lo, hi := max(0, idx-40), min(len(scrubbed), idx+60)
				t.Errorf("%s: external origin reference %q near %q", page, scheme, scrubbed[lo:hi])
			}
		}
	}
}
