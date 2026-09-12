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

	"github.com/stump-wtf/switchboard/internal/config"
	"github.com/stump-wtf/switchboard/internal/store"
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
	queued := laneCardFromItem(store.TodoItem{Todo: store.Todo{
		ID: "td_1", Source: "github", Kind: "push", Title: "github push", State: "pending",
		IdempotencyKey: "gh-d-1", CreatedAt: time.Now().Add(-2 * time.Minute),
	}, TrustMode: "signed"})
	claimed := laneCardFromItem(store.TodoItem{Todo: store.Todo{
		ID: "td_2", Source: "stripe", Kind: "invoice.paid", Title: "stripe invoice.paid", State: "claimed",
		Owner: "agent:0a1b2c3d-x", CreatedAt: time.Now().Add(-3 * time.Hour),
	}, TrustMode: "token"})
	body := renderPage(t, h, "board", view{
		Title: "The Board", Human: testHuman(), CSRF: "tok",
		Shell: shell{Active: "board", TodoCount: 3, LiveRate: 7, DBConnected: true, Initials: "JS"},
		Tiles: tilesView{Stats: stats, Bars: activityBars([]int{0, 3, 7, 1})},
		Lanes: lanesView{Verified: []laneCard{queued}, Patched: []laneCard{claimed},
			Counts: laneCounts{Verified: 1, Patched: 1}},
	})

	for _, want := range []string{
		"<header class=\"sb-topbar\" role=\"banner\">",                             // banner landmark (header element)
		"<nav class=\"sb-rail\" aria-label=\"Primary\">",                           // navigation landmark
		"<main class=\"sb-main\">",                                                 // main landmark
		"aria-current=\"page\"",                                                    // active rail entry
		"LIVE · 7/min",                                                             // LIVE pill from DB rate
		"postgres · connected",                                                     // connectivity footer
		"the board",                                                                // display page title
		"every inbound line, from arrival to hand-off",                             // tagline
		"sb-badge--signed", "sb-badge--token", "sb-badge--open", "sb-badge--queue", // trust legend
		"sb-tile--alert", // awaiting-claim tile tinted (count > 0)
		"83%",            // verified pct tile
		// Provider brand SVGs replace the two-letter tags wherever an icon ships (github, stripe).
		"/static/icons/brands/github.svg", "/static/icons/brands/stripe.svg",
		"2m ago", "3h ago", // relative ages
		"id=\"sb-tiles\"",   // tiles band is the counts swap target
		"sb-bars__bar--now", // throughput activity bars w/ current bucket
		"events/min",        // throughput tile unit
		// Patch panel (SPEC-0015): three lanes with insertion targets, cards with stable ids.
		"id=\"sb-lane-received-cards\"", "id=\"sb-lane-verified-cards\"", "id=\"sb-lane-patched-cards\"",
		">queued<",                               // pending card state chip
		"claimed · agent · 0a1b2c3",              // claimed card detail with owner label
		"hx-post=\"/todos/td_1/claim\"",          // Claim action on the queued card
		"id=\"sb-td-td_1\"", "id=\"sb-td-td_2\"", // stable card ids for OOB lane movement
		"sse-connect=\"/events\"",                               // one authenticated stream per page
		"sse-swap=\"lane_received,lane_rejected,lane_deduped\"", // page-local sink: ephemeral received lane
		"sse-swap=\"todo_created,todo_claimed,todo_completed,todo_failed,todo_resurfaced,todo_canceled,todo_rejected,todo_input_required,todo_auth_required,counts\"", // OOB sink (incl. the four A2A states, SPEC-0018)
		"hx-headers='{\"X-CSRF-Token\":\"tok\"}'", // CSRF injected into HTMX requests
		"aria-live=\"polite\"",                    // live regions present in DOM
		"id=\"sb-overlay\"",                       // overlay slot present-but-empty
		"id=\"sb-toasts\"",                        // toast region
		"id=\"sb-todo-count\"",                    // rail count pill is a swap target
		"/static/js/theme-boot.js",                // pre-paint theme boot (SPEC-0015 Theme Toggle)
		"/static/js/sb-live.js",                   // toast TTL / lane cap helper (split sb.js module)
		"/static/js/sb-keys.js",                   // keymap registry + key-hint footer
		">JS</span>",                              // avatar initials
		"/static/switchboard.css",                 // component layer linked
		"aria-label=\"Switchboard mark\"",         // accessible inline-SVG mark
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
	if !strings.Contains(body, "quiet · no lines in flight") {
		t.Error("empty received-lane state missing")
	}
	if !strings.Contains(body, "id=\"sb-lane-received-cards\"") {
		t.Error("received lane must exist in the DOM before the first SSE frame")
	}
}

// TestLivePillDecayContract pins the template↔sb.js contract for the #179 live-rate decay: every
// rendered pill (busy or idle, page or OOB frame) stamps data-sb-live-decay="75000" — the silence
// window (ms) after which sb.js returns the pill to its idle zero-state. The value must exceed the
// rate's trailing 1-minute DB window (store.BoardStats.EventsPerMin) so the client only zeroes the
// display once the true rate is already zero. sb.js has no JS runner in this gate; this template
// assertion is the browser-independent guard (same pattern as the countdown contracts).
func TestLivePillDecayContract(t *testing.T) {
	h := newTestHandler(t)
	for _, rate := range []int{0, 7} {
		out := renderFrag(t, h, "live_pill", map[string]any{"Rate": rate, "OOB": true})
		if !strings.Contains(out, `data-sb-live-decay="75000"`) {
			t.Errorf("live_pill (rate %d): missing data-sb-live-decay stamp (75s > the 60s rate window): %q", rate, out)
		}
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

// TestEndpointsViewRendersCards: the Endpoints view renders active + revoked cards with the full
// SPEC-0015 card set — principal, persona slot, scope chips, MCP URL, credential DISPLAY PREFIX
// only (hashed note), expiry countdown, last-seen / killed stamps, Rotate (re-vend) and Revoke
// (via confirm page) on active cards — and marks the Endpoints rail entry active.
// Governing: SPEC-0015 REQ "Endpoints View And Vend Wizard"; SPEC-0016 (countdown).
func TestEndpointsViewRendersCards(t *testing.T) {
	h := newTestHandler(t)
	sh := shell{Active: "endpoints", DBConnected: true, Initials: "JS"}
	seen := time.Now().Add(-2 * time.Minute)
	killed := time.Now().Add(-1 * time.Hour)
	expires := time.Now().Add(5*24*time.Hour + time.Hour)
	active := endpointCard{ID: "e1", AgentName: "reviewer-bot", Initials: "RE", Principal: "Joe Stump",
		Slug: "reviewer-bot-ab12cd", URL: "https://sb.example.com/mcp/reviewer-bot-ab12cd",
		CredPrefix: "sbk_ab12cd", Queues: []string{"reviews"}, Verbs: []string{"claim"}, State: "active",
		LastSeenAt: &seen, ExpiresAt: &expires}
	revoked := endpointCard{ID: "e2", AgentName: "old-bot", Initials: "OL", Principal: "Joe Stump",
		Slug: "old-bot-99", URL: "https://sb.example.com/mcp/old-bot-99",
		CredPrefix: "sbk_dead00", Queues: []string{"deploys"}, Verbs: []string{"complete"}, State: "revoked", RevokedAt: &killed}

	body := renderPage(t, h, "endpoints", view{Title: "Endpoints", Human: testHuman(), CSRF: "tok",
		Shell: sh, EndpointCards: []endpointCard{active, revoked}})

	for _, want := range []string{
		"vended mcp endpoints", // header copy per the design canvas
		"humans are the accountable principals · each endpoint is a scoped, revocable capability · revoke = kill the endpoint", // tagline
		"reviewer-bot", "old-bot",
		`class="sb-epcard__avatar" aria-hidden="true">RE<`, // two-letter initials avatar tiles
		`class="sb-epcard__avatar" aria-hidden="true">OL<`,
		`data-sb-ep-principal`, "Joe Stump · human", // the accountable principal (SPEC-0007)
		"sbk_ab12cd", "sbk_dead00", // credential display prefixes
		"hashed · shown once at vend",     // the hashed note beside the prefix
		"sb-chip--queue", "sb-chip--verb", // scope chips
		`data-sb-ep-url="https://sb.example.com/mcp/reviewer-bot-ab12cd"`, // the endpoint's MCP URL
		"sb-badge--active", "sb-badge--revoked",
		`id="sb-ep-expiry-e1"`, `data-sb-expires-at="`, ">⌛ 5d<", // expiry countdown chip + data
		"sb-epcard--revoked",                // the killed card is dimmed
		"endpoint killed",                   // + stamped with when
		`id="sb-ep-seen-e1"`, "seen 2m ago", // active card last-seen swap target + stamp
		`href="/endpoints/vend?from=e1" data-sb-ep-rotate`, // Rotate = re-vend, seeded from this card
		`href="/endpoints/e1/revoke" data-sb-ep-revoke`,    // Revoke goes via the confirm page
		`action="/endpoints/e2/delete"`,                    // Delete on the revoked card (housekeeping)
		"+ vend endpoint", `href="/endpoints/vend"`,        // the wizard launcher
		`sse-swap="endpoint_seen"`,              // page-local sink subscribes the endpoint screen
		`href="/endpoints" aria-current="page"`, // rail marks Endpoints active
	} {
		if !strings.Contains(body, want) {
			t.Errorf("endpoints view: missing %q", want)
		}
	}
	// Revocation must never fire straight off the card (irreversible steps confirm first): no POST
	// form targeting the revoke route may render here.
	if strings.Contains(body, `action="/endpoints/e1/revoke"`) {
		t.Error("card must link to the revoke confirm page, not POST the kill directly")
	}
	// The revoked card must NOT offer Revoke or Rotate, and no full credential ever appears.
	if strings.Contains(body, `href="/endpoints/e2/revoke"`) {
		t.Error("revoked card must not render a Revoke action")
	}
	if strings.Contains(body, `href="/endpoints/vend?from=e2"`) {
		t.Error("revoked card must not render a Rotate action")
	}
	// A card without an expiry renders no countdown chip.
	if strings.Contains(body, `id="sb-ep-expiry-e2"`) {
		t.Error("card without expires_at must not render a countdown chip")
	}
	// Delete is revoked-only: an active endpoint must be revoked before it can be removed, so the
	// active card must never render a Delete action (SPEC-0007 "Permanent Deletion of Revoked
	// Endpoints").
	if strings.Contains(body, `action="/endpoints/e1/delete"`) {
		t.Error("active card must not render a Delete action (revoke first)")
	}
}

// TestVendRevealShowsCredentialOnceAndHTTPWiring: the one-time reveal shows the plaintext credential
// once, the minted /mcp/{slug} URL, HTTP-only .mcp.json wiring, a shown-once warning, and the
// URL-only wiring variant for OAuth-capable clients (no embedded credential).
// Governing: SPEC-0015 REQ "Endpoints View And Vend Wizard" (one-time reveal + .mcp.json; URL-only
// variant), SPEC-0014 REQ "HTTP Wiring Is the Only Wiring", SPEC-0016.
func TestVendRevealShowsCredentialOnceAndHTTPWiring(t *testing.T) {
	h := newTestHandler(t)
	mcpjson := buildMCPJSON("https://sb.example.com", "reviewer-bot-ab12cd", "sbk_secret")
	urlOnly := buildMCPJSONURLOnly("https://sb.example.com", "reviewer-bot-ab12cd")
	body := html.UnescapeString(renderFrag(t, h, "vend_reveal", revealView{AgentName: "reviewer-bot",
		Slug: "reviewer-bot-ab12cd", Token: "sbk_secret", MCPJSON: mcpjson, MCPJSONURLOnly: urlOnly,
		URL:    mcpEndpointURL("https://sb.example.com", "reviewer-bot-ab12cd"),
		Queues: []string{"reviews"}, Verbs: []string{"claim"}, CSRF: "tok"}))
	for _, want := range []string{
		"sbk_secret",
		"Copy the credential now — it is shown once. The endpoint is the capability; revoking kills it.",
		// Standalone labeled fields per the design canvas minted state.
		"MCP endpoint URL", ">https://sb.example.com/mcp/reviewer-bot-ab12cd</pre>",
		"Credential · shown once",
		`"type": "http"`, "/mcp/reviewer-bot-ab12cd", "Bearer sbk_secret",
		`data-sb-reveal-wiring`,          // the bearer wiring block
		`data-sb-reveal-wiring-url-only`, // the OAuth-capable URL-only variant (SPEC-0016)
		"URL-only wiring",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("vend reveal: missing %q", want)
		}
	}
	// The reveal must carry exactly one copy of the plaintext credential outside the wiring's Bearer
	// header (the credential block) — i.e. it is not sprinkled across many surfaces, and the
	// URL-only variant must NOT embed it.
	if n := strings.Count(body, "sbk_secret"); n != 2 { // once in the <pre> block, once in the Bearer header
		t.Errorf("plaintext credential appears %d times, want exactly 2 (credential block + wiring)", n)
	}
	if strings.Contains(urlOnly, "sbk_secret") || strings.Contains(urlOnly, "Authorization") {
		t.Error("URL-only wiring variant must not embed a credential")
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
	// countdown: the endpoint card's compact time-remaining chip (SPEC-0016 countdown).
	for d, want := range map[time.Duration]string{
		-time.Minute:               "expired",
		30 * time.Second:           "<1m",
		30 * time.Minute:           "30m",
		5 * time.Hour:              "5h",
		5*24*time.Hour + time.Hour: "5d",
	} {
		if got := countdown(time.Now().Add(d)); got != want {
			t.Errorf("countdown(+%v) = %q, want %q", d, got, want)
		}
	}
	// cardInitials: the endpoint card's two-letter avatar tile (design canvas: first two characters
	// upper-cased; letters/digits only so hyphenated names stay legible).
	for name, want := range map[string]string{
		"reviewer-bot": "RE", "old-bot": "OL", "x": "X", "-bot": "BO", "1st-agent": "1S", "": "EP",
	} {
		if got := cardInitials(name); got != want {
			t.Errorf("cardInitials(%q) = %q, want %q", name, got, want)
		}
	}
}

// TestVendVerbOptionsEnumerateAgentToolsSurface: the vend modal's verb chips are enumerated from
// the agent-tools surface (internal/mcp) — drain verbs pre-checked, the webhook self-management
// and event-history verbs offered unchecked — so the modal can never offer a verb the MCP layer
// does not serve, and never silently defaults an endpoint into the wider surface.
// Governing: SPEC-0013 REQ "Endpoints View and Vend Modal", SPEC-0006, SPEC-0005.
func TestVendVerbOptionsEnumerateAgentToolsSurface(t *testing.T) {
	opts := vendVerbOptions()
	byName := make(map[string]bool, len(opts))
	for _, o := range opts {
		byName[o.Name] = o.Checked
	}
	for _, v := range []string{"list_todos", "claim", "complete", "fail", "heartbeat"} {
		checked, ok := byName[v]
		if !ok || !checked {
			t.Errorf("drain verb %q must be offered and pre-checked (offered=%v checked=%v)", v, ok, checked)
		}
	}
	for _, v := range []string{"create_webhook", "list_webhooks", "rotate_webhook", "delete_webhook",
		"list_webhook_events", "get_webhook_event", "replay_webhook_event", "list_providers"} {
		checked, ok := byName[v]
		if !ok {
			t.Errorf("verb %q from the agent-tools surface must be offered", v)
		}
		if checked {
			t.Errorf("verb %q must not be pre-checked", v)
		}
	}
}
