package web

// The SPEC-0024 notify-hook verbs on every grant surface: the vend wizard's verbs step and the quick
// vend page list them as their own "Outbound HTTP calls" group with warning copy, never pre-checked
// and never inside the default verb chips; the OAuth consent screen names what they grant.
// Governing: SPEC-0024 REQ-2 "Management Verbs" (grantable, grouped and labelled as outbound HTTP
// calls, in no default grant).

import (
	"strings"
	"testing"
)

const outboundWarning = "switchboard dials a host the agent chooses"

func assertOutboundGroup(t *testing.T, where, body, hook string) {
	t.Helper()
	if !strings.Contains(body, "Outbound HTTP calls") || !strings.Contains(body, hook) || !strings.Contains(body, outboundWarning) {
		t.Errorf("%s: outbound HTTP group, its %s hook or its warning copy is missing", where, hook)
	}
	for _, v := range []string{"create_notify_hook", "list_notify_hooks", "rotate_notify_hook", "delete_notify_hook"} {
		if !strings.Contains(body, `name="verbs" value="`+v+`"`) {
			t.Errorf("%s: %s is not offered", where, v)
		}
		if strings.Contains(body, `value="`+v+`" checked`) {
			t.Errorf("%s: %s is pre-checked; notify-hook verbs are never a default grant", where, v)
		}
	}
	// The group is its own chipset, after the default verb chips, not mixed into them.
	if i, j := strings.Index(body, `value="create_webhook"`), strings.Index(body, `value="create_notify_hook"`); i < 0 || j < i {
		t.Errorf("%s: notify-hook chips are not a separate group after the default verbs", where)
	}
}

func TestVendWizardVerbsStepListsOutboundGroup(t *testing.T) {
	h := newTestHandler(t)
	v := testVendStepView("verbs")
	v.VerbOptions = vendVerbOptions()
	v.OutboundVerbOptions = vendOutboundVerbOptions(nil)
	assertOutboundGroup(t, "vend wizard", renderVendPage(t, h, v, false), "data-sb-vend-outbound-verbs")

	// Back navigation keeps an operator's deliberate choice.
	v.OutboundVerbOptions = vendOutboundVerbOptions([]string{"list_notify_hooks", "list_todos"})
	body := renderVendPage(t, h, v, false)
	if !strings.Contains(body, `value="list_notify_hooks" checked`) || strings.Contains(body, `value="create_notify_hook" checked`) {
		t.Error("vend wizard: a chosen outbound verb did not stay checked alone")
	}
}

func TestQuickVendListsOutboundGroup(t *testing.T) {
	h := newTestHandler(t)
	page := renderQuickPage(t, h, &quickVendView{
		ActionURL:           "/endpoints/quick",
		VerbOptions:         vendVerbOptions(),
		OutboundVerbOptions: vendOutboundVerbOptions(nil),
		LifetimePresets:     lifetimePresets,
	})
	assertOutboundGroup(t, "quick vend", page, "data-sb-quickvend-outbound-verbs")
}

// Scenario "Not in the default grant": nothing the quick vend or the wizard pre-checks is a
// notify-hook verb.
func TestDefaultVerbChipsExcludeNotifyHooks(t *testing.T) {
	for _, o := range vendVerbOptions() {
		if strings.Contains(o.Name, "notify_hook") {
			t.Errorf("default verb chips include %s", o.Name)
		}
	}
	for _, o := range vendOutboundVerbOptions(nil) {
		if o.Checked {
			t.Errorf("%s starts checked", o.Name)
		}
	}
}

func TestConsentNamesOutboundHTTP(t *testing.T) {
	got := strings.Join(scopeBullets([]string{"inbox"}, []string{"list_todos", "create_notify_hook", "list_notify_hooks"}), "\n")
	if !strings.Contains(got, "make outbound HTTP calls") || !strings.Contains(got, "create_notify_hook · list_notify_hooks") {
		t.Fatalf("consent bullets do not name the outbound grant: %q", got)
	}
	if strings.Contains(got, "self-manage webhooks") {
		t.Fatalf("notify-hook verbs were folded into the webhook bullet: %q", got)
	}
	if none := strings.Join(scopeBullets([]string{"inbox"}, []string{"list_todos"}), "\n"); strings.Contains(none, "outbound") {
		t.Fatalf("consent advertises outbound calls the scope does not grant: %q", none)
	}
}
