package web

// DOM render contract for the one-step quick vend (templates/quickvend.html, ADR-0023): one page,
// one plain method=post form (no JS), the wizard's chip components, and the one-time reveal
// rendered inline on success. Assertions key on data-sb-* hooks, never on style classes.
// Governing: ADR-0023 (MVP basics); SPEC-0015 (components, no-JS); SPEC-0007 (one-time reveal).

import (
	"strings"
	"testing"
)

func renderQuickPage(t *testing.T, h *Handler, v *quickVendView) string {
	t.Helper()
	return renderPage(t, h, "quickvend", view{
		Title: "Quick vend", Human: testHuman(), CSRF: "tok",
		Shell: shell{Active: "endpoints", DBConnected: true, Initials: "JS"}, Quick: v,
	})
}

// TestQuickVendSingleFormRendersAllFields pins the one-step contract: a single form posting to
// /endpoints/quick carrying every required field — name, queue chips + free-text queues, verb
// chips (drain verbs pre-checked), and lifetime choices — with the CSRF token round-tripping.
func TestQuickVendSingleFormRendersAllFields(t *testing.T) {
	h := newTestHandler(t)
	page := renderQuickPage(t, h, &quickVendView{
		ActionURL:       "/endpoints/quick",
		Name:            "release-bot",
		QueueOptions:    []vendChipOption{{Name: "inbox", Checked: true}, {Name: "reviews"}},
		VerbOptions:     vendVerbOptions(),
		LifetimePresets: lifetimePresets,
	})

	if !strings.Contains(page, `action="/endpoints/quick"`) {
		t.Error("quick vend form does not post to /endpoints/quick")
	}
	if !strings.Contains(page, `name="csrf_token"`) {
		t.Error("quick vend form missing the CSRF token")
	}
	if !strings.Contains(page, `value="release-bot"`) {
		t.Error("agent name not prefilled from the view")
	}
	if !strings.Contains(page, `name="queues" value="inbox" checked`) {
		t.Error("checked queue chip did not render checked")
	}
	if !strings.Contains(page, `name="queues_extra"`) {
		t.Error("free-text queues field missing")
	}
	if !strings.Contains(page, `name="verbs" value="claim" checked`) {
		t.Error("drain verbs should start checked")
	}
	if !strings.Contains(page, `name="lifetime" value="7d"`) {
		t.Error("lifetime preset choices missing")
	}
}

// TestQuickVendValidationErrorRendersInline: a failed submission re-renders with the error inline
// and the operator's entered values preserved (SPEC-0015 value preservation, one-step edition).
func TestQuickVendValidationErrorRendersInline(t *testing.T) {
	h := newTestHandler(t)
	v := quickVendView{
		ActionURL: "/endpoints/quick",
		Name:      "release-bot",
		Error:     "at least one queue is required",
	}
	page := renderQuickPage(t, h, &v)
	if !strings.Contains(page, "at least one queue is required") {
		t.Error("validation error not rendered inline")
	}
	if !strings.Contains(page, `value="release-bot"`) {
		t.Error("entered values were not preserved on the error re-render")
	}
}

// TestQuickVendQueueFieldAdaptsToKnownQueues: with known queues the chips carry the label and the
// free-text field is for OTHER queues; on a fresh deployment (no chips) the field is simply
// "Queues" — the page never shows two competing "queues" labels.
func TestQuickVendQueueFieldAdaptsToKnownQueues(t *testing.T) {
	h := newTestHandler(t)
	withChips := renderQuickPage(t, h, &quickVendView{ActionURL: "/endpoints/quick",
		QueueOptions: []vendChipOption{{Name: "inbox"}}, VerbOptions: vendVerbOptions(), LifetimePresets: lifetimePresets})
	for _, want := range []string{`data-sb-quickvend-queues`, ">Scoped queues<", ">Other queues<"} {
		if !strings.Contains(withChips, want) {
			t.Errorf("with known queues: missing %q", want)
		}
	}
	if strings.Contains(withChips, `>Queues</label>`) {
		t.Error("with known queues the free-text field must not also be labeled plain Queues")
	}

	fresh := renderQuickPage(t, h, &quickVendView{ActionURL: "/endpoints/quick",
		VerbOptions: vendVerbOptions(), LifetimePresets: lifetimePresets})
	if !strings.Contains(fresh, `>Queues</label>`) || !strings.Contains(fresh, `name="queues_extra"`) {
		t.Error("fresh deployment: the plain Queues field is missing")
	}
	for _, banned := range []string{`data-sb-quickvend-queues`, ">Other queues<"} {
		if strings.Contains(fresh, banned) {
			t.Errorf("fresh deployment: unexpected %q", banned)
		}
	}
}

// TestQuickVendErrorPreservesChips: a rejected submission re-renders the chosen queue chips
// checked (not only the free-text extras), the verb chips as chosen, and the lifetime choice.
func TestQuickVendErrorPreservesChips(t *testing.T) {
	h := newTestHandler(t)
	verbs := vendVerbOptions()
	for i := range verbs {
		verbs[i].Checked = verbs[i].Name == "claim"
	}
	page := renderQuickPage(t, h, &quickVendView{
		ActionURL: "/endpoints/quick", Name: "release-bot", Error: "invalid lifetime",
		QueueOptions:    []vendChipOption{{Name: "inbox"}, {Name: "reviews", Checked: true}},
		ExtraQueues:     "deploys",
		VerbOptions:     verbs,
		LifetimePresets: lifetimePresets, LifetimePreset: "custom", LifetimeCustom: "banana",
	})
	for _, want := range []string{
		`data-sb-quickvend-error`, "invalid lifetime",
		`name="queues" value="reviews" checked`,
		`name="queues_extra" value="deploys"`,
		`name="verbs" value="claim" checked`,
		`name="lifetime" value="custom" checked`,
		`name="lifetime_custom" value="banana"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("error re-render: missing %q", want)
		}
	}
	if strings.Contains(page, `name="queues" value="inbox" checked`) {
		t.Error("an unchosen chip rendered checked")
	}
	if strings.Contains(page, `name="verbs" value="list_todos" checked`) {
		t.Error("an unchosen verb rendered checked — the chosen set must win over the drain default")
	}
}

// TestQuickVendRevealReplacesTheForm: after the mint the page shows the one-time reveal with its
// wiring and the next steps, and no longer offers the form (the credential appears exactly once).
func TestQuickVendRevealReplacesTheForm(t *testing.T) {
	h := newTestHandler(t)
	page := renderPage(t, h, "quickvend", view{
		Title: "Endpoint vended", Human: testHuman(), CSRF: "tok",
		Shell: shell{Active: "endpoints", DBConnected: true, Initials: "JS"},
		Reveal: &revealView{AgentName: "release-bot", Slug: "release-bot-ab12cd34", Token: "sbk_test",
			URL: "https://sb.example.com/mcp/release-bot-ab12cd34", MCPJSON: "{}", MCPJSONURLOnly: "{}",
			Queues: []string{"inbox"}, Verbs: []string{"claim"}, CSRF: "tok"},
	})
	for _, want := range []string{`data-sb-reveal`, "release-bot", "sbk_test", `data-sb-reveal-wiring`,
		`data-sb-quickvend-next`, `href="/endpoints/quick"`, `href="/endpoints"`} {
		if !strings.Contains(page, want) {
			t.Errorf("reveal page: missing %q", want)
		}
	}
	if strings.Contains(page, `data-sb-quickvend-form`) {
		t.Error("the form must not render alongside the one-time reveal")
	}
}

// TestEndpointsViewLaunchesQuickVend: the Endpoints view offers the one-step page next to the
// full wizard — the quick page is reachable without typing a URL.
func TestEndpointsViewLaunchesQuickVend(t *testing.T) {
	h := newTestHandler(t)
	page := renderPage(t, h, "endpoints", view{
		Title: "Endpoints", Human: testHuman(), CSRF: "tok",
		Shell: shell{Active: "endpoints", DBConnected: true, Initials: "JS"},
	})
	for _, want := range []string{`href="/endpoints/quick"`, `data-sb-quickvend-open`, `href="/endpoints/vend"`} {
		if !strings.Contains(page, want) {
			t.Errorf("endpoints view: missing %q", want)
		}
	}
}
