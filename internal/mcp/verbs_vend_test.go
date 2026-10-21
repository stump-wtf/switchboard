// The vend verb grant: what a vend hands out when the spec names nothing, how a spec of verb names
// and presets resolves, and which part of a grant the audit view flags. Issue #589 — a lane credential
// used to be vended with the whole webhook-admin surface it has no use for.
// Governing: ADR-0047.
package mcp

import (
	"slices"
	"strings"
	"testing"
)

func TestVendVerbGrantDefaultsToTheWorkerSurface(t *testing.T) {
	grant, err := VendVerbGrant(nil)
	if err != nil {
		t.Fatalf("VendVerbGrant(nil): %v", err)
	}
	want := append(append([]string{}, DrainVerbs()...), LeaseVerbs()...)
	if !slices.Equal(grant, want) {
		t.Errorf("default grant = %q, want the todo verbs plus the lease verbs %q", grant, want)
	}
	if GrantHasWebhookAdmin(grant) {
		t.Errorf("the default grant %q holds webhook-admin verbs; a worker lane needs none", grant)
	}
	// A blank spec is "nothing named", the same as omitting the field, so a script that always sends
	// the key cannot accidentally mint a credential with no verbs at all.
	for _, spec := range [][]string{{}, {""}, {"   "}} {
		got, err := VendVerbGrant(spec)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("VendVerbGrant(%q) = %q, %v; want the default grant", spec, got, err)
		}
	}
	// The todo preset is strictly narrower than the default: a lane that never touches its own lease
	// can ask for less than the default hands out.
	if got, err := ResolveVerbSpec([]string{"todo"}); err != nil || len(got) != len(DrainVerbs()) {
		t.Errorf(`"todo" preset = %q, %v; want the eight drain verbs`, got, err)
	}
}

func TestResolveVerbSpec(t *testing.T) {
	if got, err := ResolveVerbSpec([]string{"todo"}); err != nil || !slices.Equal(got, DrainVerbs()) {
		t.Fatalf(`"todo" preset = %q, %v; want %q`, got, err, DrainVerbs())
	}
	if got, err := ResolveVerbSpec([]string{"webhook-admin"}); err != nil || !slices.Equal(got, WebhookAdminVerbs()) {
		t.Fatalf(`"webhook-admin" preset = %q, %v; want %q`, got, err, WebhookAdminVerbs())
	}
	if got, err := ResolveVerbSpec([]string{"all"}); err != nil || !slices.Equal(got, AllVerbs()) {
		t.Fatalf(`"all" preset = %q, %v; want %q`, got, err, AllVerbs())
	}

	// Names and presets mix, and the grant comes back in canonical order however it was typed, so
	// two operators asking for the same scope store the same verb list.
	got, err := ResolveVerbSpec([]string{" complete , heartbeat ", "webhook-admin", "heartbeat"})
	if err != nil {
		t.Fatalf("mixed spec: %v", err)
	}
	want := append([]string{"complete", "heartbeat"}, WebhookAdminVerbs()...)
	if !slices.Equal(got, want) {
		t.Errorf("mixed spec = %q, want %q", got, want)
	}

	if got, err := ResolveVerbSpec([]string{"Complete"}); err != nil || !slices.Equal(got, []string{"complete"}) {
		t.Errorf("verb names are case-insensitive: got %q, %v", got, err)
	}

	// Notify-hook verbs are grantable by name but belong to no preset, and "all" must stay inside the
	// default-grantable surface (SPEC-0024 REQ-2).
	if got, err := ResolveVerbSpec([]string{"todo", "create_notify_hook"}); err != nil ||
		!slices.Equal(got, append(append([]string{}, DrainVerbs()...), "create_notify_hook")) {
		t.Errorf("notify verb by name = %q, %v; want the todo verbs plus it", got, err)
	}
	for _, preset := range VendPresets {
		if grantNamesNotifyHookVerbs(preset.Verbs) {
			t.Errorf("preset %q grants notify-hook verbs; those are opt-in by name only", preset.Name)
		}
	}

	// A name that is neither a verb nor a preset is refused, not dropped: silently vending less than
	// the operator asked for is the failure this surface must not have.
	for _, spec := range [][]string{{"claim_nextt"}, {"todo", "webhook"}, {"replay_events"}} {
		if got, err := ResolveVerbSpec(spec); err == nil {
			t.Errorf("ResolveVerbSpec(%q) = %q; want a refusal, not a narrower grant", spec, got)
		} else if !strings.Contains(err.Error(), "preset") {
			t.Errorf("ResolveVerbSpec(%q) error %q should name the presets an operator can use", spec, err)
		}
	}
	for _, spec := range [][]string{{",,,,"}} {
		if got, err := ResolveVerbSpec(spec); err == nil {
			t.Errorf("ResolveVerbSpec(%q) = %q; want a refusal — a credential with no verbs does nothing", spec, got)
		}
	}
}

func TestWebhookAdminVerbsInGrant(t *testing.T) {
	if held := WebhookAdminVerbsIn(AllVerbs()); !slices.Equal(held, WebhookAdminVerbs()) {
		t.Errorf("the full grant reports %q as admin, want %q", held, WebhookAdminVerbs())
	}
	if held := WebhookAdminVerbsIn(DrainVerbs()); len(held) != 0 {
		t.Errorf("the todo grant reports %q as admin; want none", held)
	}
	if held := WebhookAdminVerbsIn([]string{"replay_webhook_event", "claim"}); !slices.Equal(held, []string{"replay_webhook_event"}) {
		t.Errorf("mixed grant = %q, want just the admin verb", held)
	}
	if !GrantHasWebhookAdmin([]string{"claim", "LIST_WEBHOOKS"}) {
		t.Error("a grant holding list_webhooks must flag as webhook-admin whatever its case")
	}
	if GrantHasWebhookAdmin([]string{"claim", "get_default_lease"}) {
		t.Error("drain and lease verbs are not webhook administration")
	}
}

// The admin family is exactly the part of the default-grantable surface that is neither a todo verb
// nor a lease verb, so the audit flag cannot drift when a family gains a verb.
func TestWebhookAdminVerbsPartitionAllVerbs(t *testing.T) {
	all := AllVerbs()
	if got := len(DrainVerbs()) + len(WebhookAdminVerbs()) + len(LeaseVerbs()); got != len(all) {
		t.Fatalf("AllVerbs has %d verbs but the families sum to %d", len(all), got)
	}
	for _, v := range WebhookAdminVerbs() {
		if !slices.Contains(all, v) {
			t.Errorf("admin verb %q is not in AllVerbs", v)
		}
		if slices.Contains(DrainVerbs(), v) || slices.Contains(LeaseVerbs(), v) {
			t.Errorf("admin verb %q is also a todo or lease verb", v)
		}
	}
}

func grantNamesNotifyHookVerbs(grant []string) bool {
	for _, v := range grant {
		if slices.Contains(NotifyHookVerbs(), v) {
			return true
		}
	}
	return false
}
