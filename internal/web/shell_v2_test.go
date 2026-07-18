package web

// Charm-web shell conformance (SPEC-0015): the top bar's breadcrumb / MCP indicator / "+ new"
// launcher / theme control, the pre-paint theme boot ordering, the six-view navigation with
// providers in the IA, and the key-hint footer slot.
// Governing: ADR-0018, SPEC-0015 REQ "Application Shell And Navigation", REQ "Theme Toggle".

import (
	"strings"
	"testing"
)

// TestShellRendersCharmTopBar: the authenticated top bar carries the wordmark, the ~/operator
// breadcrumb, the MCP-connected indicator, the "+ new" launcher, and the visible theme control
// (SPEC-0015 REQ "Application Shell And Navigation", REQ "Theme Toggle").
func TestShellRendersCharmTopBar(t *testing.T) {
	h := newTestHandler(t)
	body := renderPage(t, h, "board", view{
		Title: "The Board", Human: testHuman(), CSRF: "tok",
		Shell: shell{Active: "board", DBConnected: true, Initials: "JS"},
	})
	for _, want := range []string{
		`class="sb-wordmark">switchboard</span>`, // display wordmark
		`data-sb-breadcrumb>~/operator</span>`,   // breadcrumb
		`data-sb-mcp`, "mcp connected",           // MCP indicator, connected state
		`class="sb-new" href="/endpoints/vend" data-sb-new`, // "+ new" launcher → the vend wizard (SPEC-0015)
		`data-sb-theme-toggle`,                              // visible theme control
		`id="sb-keys" data-sb-keys`,                         // key-hint footer slot
	} {
		if !strings.Contains(body, want) {
			t.Errorf("shell: missing %q", want)
		}
	}
	// Disconnected store: the MCP indicator flips to its down state.
	down := renderPage(t, h, "board", view{
		Title: "The Board", Human: testHuman(),
		Shell: shell{Active: "board", DBConnected: false, Initials: "JS"},
	})
	if !strings.Contains(down, "sb-mcp--down") || !strings.Contains(down, "mcp down") {
		t.Error("shell: MCP indicator missing its down state when the store is unreachable")
	}
}

// TestThemeBootPrecedesStylesheets: the pre-paint theme script is an EXTERNAL file (CSP
// script-src 'self') referenced BEFORE the stylesheets, so a stored night choice on a
// day-preferring OS paints night from the first frame (SPEC-0015 scenario "Returning operator").
func TestThemeBootPrecedesStylesheets(t *testing.T) {
	h := newTestHandler(t)
	body := renderPage(t, h, "login", view{Title: "Log in", OIDCConfigured: true})
	boot := strings.Index(body, `src="/static/js/theme-boot.js"`)
	css := strings.Index(body, `href="/static/tokens.css"`)
	if boot < 0 || css < 0 {
		t.Fatalf("layout missing theme boot (%d) or tokens.css (%d)", boot, css)
	}
	if boot > css {
		t.Error("theme-boot.js must load BEFORE tokens.css or the first frame can flash the wrong theme")
	}
	// The boot script must not be deferred — it has to run pre-paint.
	head := body[:css]
	if strings.Contains(head, `theme-boot.js" defer`) {
		t.Error("theme-boot.js must load synchronously (no defer): it stamps data-theme pre-paint")
	}
	if strings.Contains(body, "<script>") {
		t.Error("layout carries an inline script — script-src 'self' forbids it (SPEC-0015 Theme Toggle)")
	}
}

// TestNavOffersAllSixViews: with every capability enabled, navigation offers board, todos,
// endpoints, personas, friends, and providers — the active view indicated (SPEC-0015 scenario
// "Providers joins the IA").
func TestNavOffersAllSixViews(t *testing.T) {
	h := newTestHandler(t)
	body := renderPage(t, h, "board", view{
		Title: "The Board", Human: testHuman(),
		Shell: shell{Active: "board", DBConnected: true, Initials: "JS",
			PersonasEnabled: true, FriendsEnabled: true},
	})
	for key, href := range map[string]string{
		"b": "/", "t": "/todos", "e": "/endpoints", "p": "/personas", "f": "/friends", "r": "/providers",
	} {
		want := `data-sb-nav="` + key + `" href="` + href + `"`
		if !strings.Contains(body, want) {
			t.Errorf("nav: missing view entry %q", want)
		}
	}
	if got := strings.Count(body, `aria-current="page"`); got != 1 {
		t.Errorf("nav: aria-current count = %d, want exactly 1", got)
	}
}

// TestKeyHintFooterSlotIsEmptyServerSide: the #sb-keys footer slot renders EMPTY from the server —
// hints are drawn at runtime from the sb-keys.js keymap registry, the single source of truth, so
// no template may carry hint text that could drift from the bindings (SPEC-0015 REQ "Global
// Keyboard Map", scenario "Hints match behavior"). Governing: ADR-0018.
func TestKeyHintFooterSlotIsEmptyServerSide(t *testing.T) {
	h := newTestHandler(t)
	for _, page := range []string{"board", "login"} {
		v := view{Title: "The Board", Human: testHuman(), CSRF: "tok",
			Shell: shell{Active: "board", DBConnected: true, Initials: "JS"}}
		if page == "login" {
			v = view{Title: "Log in", OIDCConfigured: true}
		}
		body := renderPage(t, h, page, v)
		if !strings.Contains(body, `id="sb-keys" data-sb-keys aria-label="Keyboard shortcuts"></span>`) {
			t.Errorf("%s: #sb-keys slot missing or carries server-rendered content — hints must come only from the sb-keys.js registry", page)
		}
		// No hint text may be baked into any template output either ("t theme" is skipped: it
		// collides with the toggle's own "night theme" label, which is not a key hint).
		for _, hint := range []string{"g+view", "enter open", "/ filter"} {
			if strings.Contains(body, hint) {
				t.Errorf("%s: rendered page hardcodes key hint %q — a second source of truth beside the registry", page, hint)
			}
		}
	}
}

// TestProvidersPageRendersShellPlacement: the providers stub page renders with the shared chrome,
// marks its nav entry active, and explains where provider management lives until SPEC-0017.
func TestProvidersPageRendersShellPlacement(t *testing.T) {
	h := newTestHandler(t)
	body := renderPage(t, h, "providers", view{
		Title: "Providers", Human: testHuman(), CSRF: "tok",
		Shell: shell{Active: "providers", DBConnected: true, Initials: "JS"},
	})
	for _, want := range []string{
		`href="/providers" aria-current="page"`, // nav marks Providers active
		"data-sb-providers-empty",               // explanatory empty state
		"SPEC-0017",                             // names where the registry lands
	} {
		if !strings.Contains(body, want) {
			t.Errorf("providers view: missing %q", want)
		}
	}
	if got := strings.Count(body, `aria-current="page"`); got != 1 {
		t.Errorf("providers view: aria-current count = %d, want exactly 1", got)
	}
}
