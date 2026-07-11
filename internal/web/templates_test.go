package web

// Template parse/render coverage for the SPEC-0013 layout shell and the restyled screens:
// landmarks, rail state, LIVE pill gating, connectivity indicator, and the .sb-* component layer
// (no Pico anywhere). Governing: SPEC-0013 REQ "Design Language Conformance", REQ "Information
// Architecture and Navigation".

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/joestump/switchboard/internal/config"
	"github.com/joestump/switchboard/internal/store"
)

func newTestHandler(t *testing.T) *Handler {
	t.Helper()
	h, err := New(nil, config.Config{BaseURL: "https://sb.example.com"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
}

func renderPage(t *testing.T, h *Handler, page string, v view) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.render(rec, page, v)
	if rec.Code != 200 {
		t.Fatalf("render %s: status %d, body %q", page, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func testHuman() *store.Human {
	return &store.Human{ID: "h1", DisplayName: "Joe Stump", Email: "joe@example.com"}
}

func TestBoardRendersShellAndTiles(t *testing.T) {
	h := newTestHandler(t)
	stats := store.BoardStats{TodosToday: 12, InFlight: 2, AwaitingClaim: 3, VerifiedPct: 83, EventsPerMin: 7}
	body := renderPage(t, h, "board", view{
		Title: "The Board", Human: testHuman(), CSRF: "tok",
		Shell: shell{Active: "board", TodoCount: 3, LiveRate: 7, DBConnected: true, Initials: "JS"},
		Tiles: tilesView{Stats: stats, Bars: activityBars([]int{0, 3, 7, 1})},
		Rows: []feedRow{
			feedRowFromEvent(store.EventSummary{ID: 1, Source: "github", EventType: "push", TrustMode: "signed",
				ReceivedAt: time.Now().Add(-2 * time.Minute), TodoID: "td_1", TodoState: "pending"}, false),
			feedRowFromEvent(store.EventSummary{ID: 2, Source: "stripe", EventType: "invoice.paid", TrustMode: "token",
				ReceivedAt: time.Now().Add(-3 * time.Hour), TodoID: "td_2", TodoState: "claimed", TodoOwner: "agent:0a1b2c3d-x"}, false),
		},
	})

	for _, want := range []string{
		"<header class=\"sb-topbar\">",                                             // banner landmark (header element)
		"<nav class=\"sb-rail\" aria-label=\"Primary\">",                           // navigation landmark
		"<main class=\"sb-main\">",                                                 // main landmark
		"aria-current=\"page\"",                                                    // active rail entry
		"LIVE · 7/min",                                                             // LIVE pill from DB rate
		"postgres · connected",                                                     // connectivity footer
		"The Board",                                                                // Zilla Slab page title
		"many lines in · each verified · patched through",                          // mono tagline
		"sb-badge--signed", "sb-badge--token", "sb-badge--open", "sb-badge--queue", // trust legend
		"sb-tile--alert", // awaiting-claim tile tinted (count > 0)
		"83%",            // verified pct tile
		">GH<", ">ST<",   // provider tags
		"2m ago", "3h ago", // relative ages
		"id=\"sb-tiles\"",                  // tiles band is the counts swap target
		"sb-bars__bar--now",                // throughput activity bars w/ current bucket
		"events/min",                       // throughput tile unit
		"patched → todo",                   // pending row lifecycle stage
		"claimed · agent · 0a1b2c3",        // claimed row stage with owner label
		"hx-post=\"/todos/td_1/claim\"",    // Claim action on the pending row
		"id=\"sb-ev-1\"", "id=\"sb-ev-2\"", // stable row ids for OOB stage updates
		"sse-connect=\"/events\"",     // one authenticated stream per page
		"sse-swap=\"event_received\"", // feed subscribes to new lines
		"hx-swap=\"afterbegin\"",      // rows enter at the top
		"sse-swap=\"todo_created,todo_claimed,todo_completed,todo_failed,todo_resurfaced,counts\"", // OOB sink
		"hx-headers='{\"X-CSRF-Token\":\"tok\"}'",                                                  // CSRF injected into HTMX requests
		"aria-live=\"polite\"",            // live regions present in DOM
		"id=\"sb-overlay\"",               // overlay slot present-but-empty
		"id=\"sb-toasts\"",                // toast region
		"id=\"sb-todo-count\"",            // rail count pill is a swap target
		"/static/sb.js",                   // toast TTL / feed cap helper
		">JS</span>",                      // avatar initials
		"/static/switchboard.css",         // component layer linked
		"aria-label=\"Switchboard mark\"", // accessible inline-SVG mark
	} {
		if !strings.Contains(body, want) {
			t.Errorf("board: missing %q", want)
		}
	}
	if strings.Contains(strings.ToLower(body), "pico") {
		t.Error("board: references Pico")
	}
}

// Carry-over decision from the wave-1 verification pass (#101): the LIVE pill is ALWAYS rendered
// so the SSE counts frame has a swap target before the first event; at zero rate it renders the
// muted idle state instead of disappearing. Governing: SPEC-0013 REQ "Information Architecture
// and Navigation" (top bar shows the live rate).
func TestLivePillIdleAtZeroRate(t *testing.T) {
	h := newTestHandler(t)
	body := renderPage(t, h, "board", view{
		Title: "The Board", Human: testHuman(),
		Shell: shell{Active: "board", DBConnected: true, Initials: "JS"},
	})
	if !strings.Contains(body, "sb-live--idle") {
		t.Error("LIVE pill should render its idle state when the rate is zero")
	}
	if !strings.Contains(body, "LIVE · 0/min") {
		t.Error("LIVE pill zero-state should still show the rate")
	}
	if !strings.Contains(body, "no lines in yet") {
		t.Error("empty feed state missing")
	}
	if !strings.Contains(body, "id=\"sb-feed\"") {
		t.Error("feed live region must exist in the DOM before the first SSE frame")
	}
}

func TestDisconnectedIndicator(t *testing.T) {
	h := newTestHandler(t)
	body := renderPage(t, h, "board", view{
		Title: "The Board", Human: testHuman(),
		Shell: shell{Active: "board", DBConnected: false, Initials: "JS"},
	})
	if !strings.Contains(body, "postgres · disconnected") {
		t.Error("disconnected state missing its distinct text")
	}
	if !strings.Contains(body, "sb-conn--down") {
		t.Error("disconnected state missing its distinct class")
	}
}

func TestLoginRendersWithoutRail(t *testing.T) {
	h := newTestHandler(t)
	body := renderPage(t, h, "login", view{Title: "Log in", OIDCConfigured: true})
	if strings.Contains(body, "sb-rail") {
		t.Error("login (unauthenticated) must not render the nav rail")
	}
	if !strings.Contains(body, "/auth/login") {
		t.Error("login missing OIDC entry point")
	}
	if strings.Contains(strings.ToLower(body), "pico") {
		t.Error("login: references Pico")
	}
}

func TestEndpointsScreensRender(t *testing.T) {
	h := newTestHandler(t)
	ag := &store.Agent{ID: "a1", Name: "reviewer-bot", Description: "reviews PRs", CreatedAt: time.Now()}
	ep := &store.Endpoint{ID: "e1", CredentialPrefix: "sbk_ab12cd", ScopeQueues: []string{"reviews"}, ScopeVerbs: []string{"claim"}, State: "active"}
	sh := shell{Active: "endpoints", DBConnected: true, Initials: "JS"}

	dash := renderPage(t, h, "dashboard", view{Title: "Endpoints", Human: testHuman(), CSRF: "tok", Shell: sh, Agents: []store.Agent{*ag}})
	if !strings.Contains(dash, "Endpoints") || !strings.Contains(dash, "reviewer-bot") {
		t.Error("dashboard missing Endpoints title or agent row")
	}
	if !strings.Contains(dash, "href=\"/agents\" aria-current=\"page\"") {
		t.Error("dashboard rail must mark Endpoints active")
	}

	agent := renderPage(t, h, "agent", view{Title: ag.Name, Human: testHuman(), CSRF: "tok", Shell: sh, Agent: ag, Endpoints: []store.Endpoint{*ep}})
	for _, want := range []string{
		"sbk_ab12cd", "sb-badge--active", "Vend endpoint",
		`id="sb-ep-seen-e1"`, ">never<", // last-seen swap target exists before the first frame
		`sse-swap="endpoint_seen"`, // page-local sink subscribes the endpoint screen
	} {
		if !strings.Contains(agent, want) {
			t.Errorf("agent: missing %q", want)
		}
	}
	seen := time.Now().Add(-2 * time.Minute)
	ep2 := *ep
	ep2.LastSeenAt = &seen
	agentSeen := renderPage(t, h, "agent", view{Title: ag.Name, Human: testHuman(), CSRF: "tok", Shell: sh, Agent: ag, Endpoints: []store.Endpoint{ep2}})
	if !strings.Contains(agentSeen, "seen 2m ago") {
		t.Error("agent: touched endpoint missing rendered last-seen stamp")
	}

	vended := renderPage(t, h, "vended", view{Title: "Vended", Human: testHuman(), CSRF: "tok", Shell: sh, Agent: ag, Endpoint: ep, Token: "sbk_secret", MCPJSON: "{}"})
	if !strings.Contains(vended, "sbk_secret") || !strings.Contains(vended, "shown only once") {
		t.Error("vended missing one-time credential display")
	}
}

func TestNoExternalOrigins(t *testing.T) {
	// SPEC-0013 "No external origins": every asset reference in every rendered page must be
	// same-origin under CSP default-src 'self'.
	h := newTestHandler(t)
	pages := map[string]view{
		"login": {Title: "Log in"},
		"board": {Title: "The Board", Human: testHuman(), Shell: shell{Active: "board"}},
	}
	for page, v := range pages {
		body := renderPage(t, h, page, v)
		for _, banned := range []string{"https://fonts.googleapis.com", "https://fonts.gstatic.com", "cdn.", "//unpkg", "//cdnjs"} {
			if strings.Contains(body, banned) {
				t.Errorf("%s: references external origin %q", page, banned)
			}
		}
	}
}

func TestHelpers(t *testing.T) {
	if got := initials(&store.Human{DisplayName: "Joe Stump"}); got != "JS" {
		t.Errorf("initials(Joe Stump) = %q", got)
	}
	if got := initials(&store.Human{Email: "ops@example.com"}); got != "O" {
		t.Errorf("initials(email only) = %q", got)
	}
	if got := initials(nil); got != "OP" {
		t.Errorf("initials(nil) = %q", got)
	}
	if got := providerTag("github"); got != "GH" {
		t.Errorf("providerTag(github) = %q", got)
	}
	if got := providerTag(""); got != "··" {
		t.Errorf("providerTag(empty) = %q", got)
	}
	if got := relTime(time.Now().Add(-30 * time.Second)); got != "just now" {
		t.Errorf("relTime(30s) = %q", got)
	}
	if got := relTime(time.Now().Add(-49 * time.Hour)); got != "2d ago" {
		t.Errorf("relTime(49h) = %q", got)
	}
}
