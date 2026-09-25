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
		`action="/endpoints/ep-1/hooks/hook-off/delete"`,
		`name="csrf_token" value="tok"`,
		`aria-label="Disable notify hook https://dispatch.example.com/sb?redacted"`,
		`aria-live="polite"`,
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
