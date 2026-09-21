package web

// Cross-cutting render tests for the UX review epic (#83). Each assertion
// pins a specific story's fix so it cannot regress:
//
//   #85 — no user-visible "source" for the provider concept
//   #86 — Board column subtitles carry todo-state names and deep-link
//   #87 — persona prompt collapsed; published-but-unvended warns
//   #88 — lowercase heading/CTA case across all six pages
//   #89 — .sb-code carries overflow-wrap so URLs stay inside cards
//   #90 — header CTA says "+ vend"; wizard step 1 is "agent"
//
// Governing: SPEC-0015 REQ "Design Language Conformance". Individual
// story tests live alongside their story's templates; this file is the
// cross-cutting net that catches a regression introduced by a different
// story.

import (
	"strings"
	"testing"
)

// TestNoUserVisibleSourceForProvider scans every template for the
// user-visible string "source" used for the webhook-origin concept.
// Code identifiers (.Source, data-sb-source) are exempt — this is copy
// only. Story #85.
func TestNoUserVisibleSourceForProvider(t *testing.T) {
	for _, tmpl := range []string{
		"board", "todos", "endpoints", "friends",
	} {
		body := renderPage(t, newTestHandler(t), tmpl, view{
			Title: "test", Human: testHuman(), CSRF: "tok",
			Shell: shell{Active: tmpl, DBConnected: true, Initials: "JS"},
		})
		// The search placeholder must say "provider" not "source".
		if strings.Contains(body, "search id · source") {
			t.Errorf("%s: search placeholder still says 'source' instead of 'provider'", tmpl)
		}
		// The stat tile must say "verified providers" not "verified sources".
		if strings.Contains(body, "verified sources") {
			t.Errorf("%s: stat tile still says 'verified sources' instead of 'verified providers'", tmpl)
		}
	}
}

// TestBoardColumnSubtitlesNameTodoStates asserts the Board lane subtitles
// carry the todo-state vocabulary and deep-link to filtered todos. Story #86.
func TestBoardColumnSubtitlesNameTodoStates(t *testing.T) {
	h := newTestHandler(t)
	body := renderPage(t, h, "board", view{
		Title: "the board", Human: testHuman(), CSRF: "tok",
		Shell: shell{Active: "board", DBConnected: true, Initials: "JS"},
	})
	for _, want := range []string{
		"pending todos",
		"claimed, done, failed",
		`href="/todos?filter=pending"`,
		`href="/todos?filter=claimed"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("board: missing %q", want)
		}
	}
}

// TestPersonaPromptCollapsedByDefault asserts the persona card prompt is
// collapsed in a <details> element and the published-but-unvended warning
// renders when no endpoint vends a published persona. Story #87.
func TestPersonaPromptCollapsedByDefault(t *testing.T) {
	h := newTestHandler(t)
	// Published persona with no vended-as endpoints.
	body := renderFrag(t, h, "persona_card", personaCardView{
		ID:           "p1",
		Name:         "Reviewer",
		Initials:     "RE",
		Discoverable: true,
		SystemPrompt: "line one\nline two\nline three",
		PromptLines:  3,
		CSRF:         "tok",
	})
	for _, want := range []string{
		`<details class="sb-persona__prompt"`,
		`<summary class="sb-persona__prompt-summary"`,
		`<pre class="sb-persona__prompt-text"`,
		"system prompt · 3 lines",
		"published · not vended",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("persona card: missing %q", want)
		}
	}
}

// TestLowercaseHeadingsAndCTAs asserts the terminal lowercase voice is
// applied to page headings and CTAs across all six pages. Story #88.
func TestLowercaseHeadingsAndCTAs(t *testing.T) {
	h := newTestHandler(t)
	pages := []struct {
		page  string
		title string
	}{
		{"board", "the board"},
		{"todos", "todos"},
		{"endpoints", "vended mcp endpoints"},
		{"friends", "friends · agent-to-agent"},
	}
	for _, p := range pages {
		body := renderPage(t, h, p.page, view{
			Title: p.title, Human: testHuman(), CSRF: "tok",
			Shell: shell{Active: p.page, DBConnected: true, Initials: "JS"},
		})
		if !strings.Contains(body, p.title) {
			t.Errorf("%s: missing lowercase heading %q", p.page, p.title)
		}
	}
	// Personas page heading is covered by TestPersonasPageRendersCardsAndWizardEntry.
	// CTAs must be lowercase — check each on its own page.
	if body := renderPage(t, h, "endpoints", view{Title: "vended mcp endpoints", Human: testHuman(), CSRF: "tok", Shell: shell{Active: "endpoints", DBConnected: true, Initials: "JS"}}); !strings.Contains(body, "+ vend endpoint") {
		t.Error("endpoints: missing lowercase CTA '+ vend endpoint'")
	}
	// Personas page CTA ("+ new persona") is covered by TestPersonasPageRendersCardsAndWizardEntry.
	if body := renderPage(t, h, "friends", view{Title: "friends · agent-to-agent", Human: testHuman(), CSRF: "tok", Shell: shell{Active: "friends", DBConnected: true, Initials: "JS"}}); !strings.Contains(body, "+ add friend") {
		t.Error("friends: missing lowercase CTA '+ add friend'")
	}
}

// TestEndpointCardURLOverflowClass asserts the .sb-code class carries
// overflow-wrap so long URLs stay inside the card. Story #89. This is a
// CSS assertion — we check the class is present on the URL element and
// trust the CSS file (which has overflow-wrap: anywhere on .sb-code).
func TestEndpointCardURLOverflowClass(t *testing.T) {
	h := newTestHandler(t)
	body := renderPage(t, h, "endpoints", view{
		Title: "vended mcp endpoints", Human: testHuman(), CSRF: "tok",
		Shell:         shell{Active: "endpoints", DBConnected: true, Initials: "JS"},
		EndpointCards: []endpointCard{{ID: "ep1", AgentName: "bot", URL: "https://switchboard.stump.wtf/mcp/bot-very-long-slug-705ec7dc", CredPrefix: "sbk_ab12cd", State: "active"}},
	})
	if !strings.Contains(body, `class="sb-code"`) {
		t.Error("endpoints: .sb-code class missing on URL element")
	}
	if !strings.Contains(body, "data-sb-ep-url=") {
		t.Error("endpoints: data-sb-ep-url attribute missing")
	}
}

// TestHeaderCTAAndWizardStepLabels asserts the header CTA says "+ vend"
// and the wizard's first step is "agent" not "persona". Story #90.
func TestHeaderCTAAndWizardStepLabels(t *testing.T) {
	h := newTestHandler(t)
	body := renderPage(t, h, "board", view{
		Title: "the board", Human: testHuman(), CSRF: "tok",
		Shell: shell{Active: "board", DBConnected: true, Initials: "JS"},
	})
	if !strings.Contains(body, `>+ vend</a>`) {
		t.Error("header: CTA must say '+ vend' not '+ new'")
	}
	// Wizard step 1 must be "agent".
	wizBody := renderPage(t, h, "vend", view{
		Title: "Vend endpoint", Human: testHuman(), CSRF: "tok",
		Shell:           shell{Active: "endpoints", DBConnected: true, Initials: "JS"},
		PersonasEnabled: true,
		Vend:            testVendStepView("agent"),
	})
	if !strings.Contains(wizBody, `data-sb-wiz-step="agent"`) {
		t.Error("vend wizard: step 1 must be 'agent' not 'persona'")
	}
	if strings.Contains(wizBody, `data-sb-wiz-step="persona"`) {
		t.Error("vend wizard: step 1 must not be 'persona'")
	}
}
