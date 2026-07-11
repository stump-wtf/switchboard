package web

// Template parse/render coverage for the SPEC-0013 layout shell and the restyled screens:
// landmarks, rail state, LIVE pill gating, connectivity indicator, and the .sb-* component layer
// (no Pico anywhere). Governing: SPEC-0013 REQ "Design Language Conformance", REQ "Information
// Architecture and Navigation".

import (
	"html"
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
		"<header class=\"sb-topbar\" role=\"banner\">",                             // banner landmark (header element)
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

// TestEndpointsViewRendersCards: the Endpoints view renders active + revoked cards with the agent
// name, scope chips, credential DISPLAY PREFIX only, last-seen / killed stamps, and Revoke on active
// cards — and marks the Endpoints rail entry active. Governing: SPEC-0013 REQ "Endpoints View and
// Vend Modal".
func TestEndpointsViewRendersCards(t *testing.T) {
	h := newTestHandler(t)
	sh := shell{Active: "endpoints", DBConnected: true, Initials: "JS"}
	seen := time.Now().Add(-2 * time.Minute)
	killed := time.Now().Add(-1 * time.Hour)
	active := endpointCard{ID: "e1", AgentName: "reviewer-bot", Slug: "reviewer-bot-ab12cd",
		CredPrefix: "sbk_ab12cd", Queues: []string{"reviews"}, Verbs: []string{"claim"}, State: "active", LastSeenAt: &seen}
	revoked := endpointCard{ID: "e2", AgentName: "old-bot", Slug: "old-bot-99",
		CredPrefix: "sbk_dead00", Queues: []string{"deploys"}, Verbs: []string{"complete"}, State: "revoked", RevokedAt: &killed}

	body := renderPage(t, h, "endpoints", view{Title: "Endpoints", Human: testHuman(), CSRF: "tok",
		Shell: sh, EndpointCards: []endpointCard{active, revoked}, VerbOptions: drainVerbs})

	for _, want := range []string{
		"reviewer-bot", "old-bot",
		"sbk_ab12cd", "sbk_dead00", // credential display prefixes
		"sb-chip--queue", "sb-chip--verb", // scope chips
		"sb-badge--active", "sb-badge--revoked",
		"sb-epcard--revoked",                // the killed card is dimmed
		"endpoint killed",                   // + stamped with when
		`id="sb-ep-seen-e1"`, "seen 2m ago", // active card last-seen swap target + stamp
		`action="/endpoints/e1/revoke"`,         // Revoke on the active card
		"+ Vend endpoint",                       // the vend trigger
		`sse-swap="endpoint_seen"`,              // page-local sink subscribes the endpoint screen
		`href="/endpoints" aria-current="page"`, // rail marks Endpoints active
	} {
		if !strings.Contains(body, want) {
			t.Errorf("endpoints view: missing %q", want)
		}
	}
	// The revoked card must NOT offer a Revoke action, and no full credential ever appears.
	if strings.Contains(body, `action="/endpoints/e2/revoke"`) {
		t.Error("revoked card must not render a Revoke action")
	}
}

// TestVendModalRendersFields: the vend modal collects a name, queue field, and verb toggle chips,
// carries the CSRF token, posts to /endpoints/vend, and wires the overlay focus/close machinery.
// Governing: SPEC-0013 REQ "Endpoints View and Vend Modal".
func TestVendModalRendersFields(t *testing.T) {
	h := newTestHandler(t)
	body := renderFrag(t, h, "vend_modal", view{CSRF: "tok", VerbOptions: drainVerbs})
	for _, want := range []string{
		`role="dialog"`, `aria-modal="true"`, `data-sb-modal`, // modal a11y + overlay hook
		`data-sb-close`,                                         // close control (Escape/scrim/return handled by sb.js)
		`action="/endpoints/vend"`, `hx-post="/endpoints/vend"`, // no-JS + HTMX submit
		`name="csrf_token" value="tok"`,    // CSRF over the POST
		`name="name"`, `data-sb-vend-name`, // agent name field
		`name="queues"`, `data-sb-vend-queues`, // queue field
		`name="verbs" value="list_todos"`, `name="verbs" value="claim"`, // verb toggle chips
		`data-sb-vend-submit`, // the submit sb.js gates on scope
	} {
		if !strings.Contains(body, want) {
			t.Errorf("vend modal: missing %q", want)
		}
	}
}

// TestVendRevealShowsCredentialOnceAndHTTPWiring: the one-time reveal shows the plaintext credential
// once, the minted /mcp/{slug} URL, HTTP-only .mcp.json wiring, and a shown-once warning.
// Governing: SPEC-0013 (credential reveal is one-time), SPEC-0014 REQ "HTTP Wiring Is the Only Wiring".
func TestVendRevealShowsCredentialOnceAndHTTPWiring(t *testing.T) {
	h := newTestHandler(t)
	mcpjson := buildMCPJSON("https://sb.example.com", "reviewer-bot-ab12cd", "sbk_secret")
	body := html.UnescapeString(renderFrag(t, h, "vend_reveal", revealView{AgentName: "reviewer-bot",
		Slug: "reviewer-bot-ab12cd", Token: "sbk_secret", MCPJSON: mcpjson,
		Queues: []string{"reviews"}, Verbs: []string{"claim"}, CSRF: "tok"}))
	for _, want := range []string{
		"sbk_secret", "shown only once",
		`"type": "http"`, "/mcp/reviewer-bot-ab12cd", "Bearer sbk_secret",
		"data-sb-close", // closing clears the overlay → the plaintext is unrecoverable
	} {
		if !strings.Contains(body, want) {
			t.Errorf("vend reveal: missing %q", want)
		}
	}
	// The reveal must carry exactly one copy of the plaintext credential outside the wiring's Bearer
	// header (the credential block) — i.e. it is not sprinkled across many surfaces.
	if n := strings.Count(body, "sbk_secret"); n != 2 { // once in the <pre> block, once in the Bearer header
		t.Errorf("plaintext credential appears %d times, want exactly 2 (credential block + wiring)", n)
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
