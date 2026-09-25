package web

// Render contract for the endpoint card's notify-hook section (templates/fragments/endpoints.html
// "endpoint_hooks"). Governing: SPEC-0024 REQ-10 "Operator Web UI".

import (
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/switchboard/internal/store"
)

func cardWithHooks(state string, hooks ...store.NotifyHook) endpointCard {
	return endpointCard{ID: "ep-1", AgentName: "hook-bot", Initials: "HO", State: state, Hooks: hookRows(hooks)}
}

func TestEndpointCardRendersHooks(t *testing.T) {
	h := newTestHandler(t)
	status, reason := 503, store.NotifyHookDisabledFailures
	at := time.Now().Add(-2 * time.Minute)
	body := renderFrag(t, h, "endpoint_card", map[string]any{"CSRF": "tok", "PersonasEnabled": false, "C": cardWithHooks("active",
		store.NotifyHook{ID: "hook-on", URL: "https://dispatch.example.com/sb?token=s3cr3t", Queues: []string{"reviews"}, Enabled: true},
		store.NotifyHook{ID: "hook-off", URL: "https://relay.example.com/<script>", Enabled: false, DisabledReason: &reason,
			LastStatus: &status, LastAttemptAt: &at, ConsecutiveFailures: 10},
	)})

	for _, want := range []string{
		"https://dispatch.example.com/sb?redacted", // query redacted server-side
		`<span class="sb-chip sb-chip--queue">reviews</span>`,
		`data-sb-hook-state="enabled"`,
		"disabled · too many failures",
		"last 503",
		"10 failed in a row",
		"every scoped queue",
		`action="/endpoints/ep-1/hooks/hook-on/disable"`,
		`action="/endpoints/ep-1/hooks/hook-off/enable"`,
		// htmx upgrade of the plain form: swap the section in place; the toggle keeps one id.
		`hx-post="/endpoints/ep-1/hooks/hook-on/disable" hx-target="#sb-ep-hooks-ep-1" hx-swap="outerHTML"`,
		`id="sb-hook-toggle-hook-on"`,
		// Delete links to its confirm page (SPEC-0015 REQ "Wizard Interaction Pattern").
		`href="/endpoints/ep-1/hooks/hook-off/delete"`,
		`name="csrf_token" value="tok"`,
		`aria-label="Disable notify hook https://dispatch.example.com/sb?redacted"`,
		`aria-label="Delete notify hook https://dispatch.example.com/sb?redacted"`,
		`<div class="sb-hook__acts">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("card missing %q", want)
		}
	}
	for _, leak := range []string{"s3cr3t", "<script>"} {
		if strings.Contains(body, leak) {
			t.Errorf("card renders %q unescaped or unredacted", leak)
		}
	}
	// Delete never POSTs straight from the card.
	if strings.Contains(body, `action="/endpoints/ep-1/hooks/hook-off/delete"`) {
		t.Error("the card carries a one-click delete form")
	}
}

// A failed hooks read must not read as "no hooks": the owner would be told there is nothing to act
// on while an auto-disabled hook may be waiting for a re-enable.
func TestEndpointCardHooksUnavailable(t *testing.T) {
	h := newTestHandler(t)
	c := cardWithHooks("active")
	c.HooksUnavailable = true
	body := renderFrag(t, h, "endpoint_card", map[string]any{"CSRF": "tok", "PersonasEnabled": false, "C": c})
	if !strings.Contains(body, "data-sb-hooks-unavailable") {
		t.Error("a failed hooks read does not render the unavailable state")
	}
	if strings.Contains(body, "data-sb-hooks-empty") {
		t.Error("a failed hooks read renders the empty state")
	}
}

func TestHookDeleteConfirmPage(t *testing.T) {
	h := newTestHandler(t)
	row := hookRows([]store.NotifyHook{{ID: "hook-1", URL: "https://dispatch.example.com/sb?token=s3cr3t"}})[0]
	var buf strings.Builder
	if err := h.pages["hook_delete"].ExecuteTemplate(&buf, "layout", view{
		Title: "Delete notify hook", Human: &store.Human{DisplayName: "Op"}, CSRF: "tok",
		HookDelete: &hookDeleteView{EndpointID: "ep-1", AgentName: "hook-bot", Hook: row},
	}); err != nil {
		t.Fatalf("render hook_delete: %v", err)
	}
	body := buf.String()
	for _, want := range []string{
		"data-sb-hook-delete-confirm", "hook-bot", "https://dispatch.example.com/sb?redacted",
		`action="/endpoints/ep-1/hooks/hook-1/delete"`, `name="csrf_token" value="tok"`, `href="/endpoints#sb-ep-ep-1"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("confirm page missing %q", want)
		}
	}
	if strings.Contains(body, "s3cr3t") {
		t.Error("confirm page leaks the hook's query string")
	}
}

func TestEndpointCardHookEmptyStateAndRevoked(t *testing.T) {
	h := newTestHandler(t)
	empty := renderFrag(t, h, "endpoint_card", map[string]any{"CSRF": "tok", "PersonasEnabled": false, "C": cardWithHooks("active")})
	if !strings.Contains(empty, "data-sb-hooks-empty") || !strings.Contains(empty, "no notify hooks") {
		t.Error("an active card with no hooks does not render the empty state")
	}
	revoked := renderFrag(t, h, "endpoint_card", map[string]any{"CSRF": "tok", "PersonasEnabled": false, "C": cardWithHooks("revoked",
		store.NotifyHook{ID: "h", URL: "https://dispatch.example.com/", Enabled: true})})
	if strings.Contains(revoked, "data-sb-ep-hooks") {
		t.Error("a revoked card renders hook controls")
	}
}
