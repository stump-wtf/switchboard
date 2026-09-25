package server

// The endpoint card's notify-hook controls through the real router and a live session: the owner
// sees the redacted hook and can disable, re-enable and (through the confirm page) delete it;
// another human gets a plain 404 for every control and changes nothing; a POST without the CSRF
// token is refused on every control; an operator disable is counted once.
//
// Governing: SPEC-0024 REQ-10 "Operator Web UI" (scenarios "Human disables a noisy hook" and
// "Foreign human"), REQ-11 (the operator-disable series), Security Requirements "CSRF Protection"
// and "Redirect Validation"; SPEC-0015 REQ "Wizard Interaction Pattern" (delete confirms).

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stump-wtf/switchboard/internal/metrics"
	"github.com/stump-wtf/switchboard/internal/store"
)

var hookActions = []string{"/disable", "/enable", "/delete"}

func TestNotifyHookCardControls(t *testing.T) {
	f := newTenancyFixture(t)
	hook, err := f.st.CreateNotifyHook(f.ctx, f.epA.ID, "https://dispatch.example.com/alice?token=alice-query-secret",
		[]string{"reviews"}, false, "whsec_alice_secret_value", 5)
	if err != nil {
		t.Fatalf("create hook: %v", err)
	}
	base := "/endpoints/" + f.epA.ID + "/hooks/" + hook.ID
	unchanged := func(what string) {
		t.Helper()
		if got, err := f.st.GetNotifyHook(f.ctx, hook.ID, f.epA.ID); err != nil || !got.Enabled {
			t.Fatalf("%s changed alice's hook: %+v, %v", what, got, err)
		}
	}

	// The owner's card shows the hook, redacted, with its controls; delete is a link to its
	// confirm page, never a one-click POST.
	page := getAs(t, f.r, f.aliceTok, "/endpoints")
	body := page.Body.String()
	for _, want := range []string{"https://dispatch.example.com/alice?redacted", `action="` + base + `/disable"`, `href="` + base + `/delete"`} {
		if !strings.Contains(body, want) {
			t.Errorf("owner's card is missing %q", want)
		}
	}
	if strings.Contains(body, `action="`+base+`/delete"`) {
		t.Error("the card POSTs delete directly instead of routing through the confirm page")
	}
	for _, leak := range []string{"alice-query-secret", "whsec_alice_secret_value"} {
		if strings.Contains(body, leak) {
			t.Errorf("endpoints page leaks %q", leak)
		}
	}
	// Scenario "Foreign human": bob sees nothing of it, and every control is a 404 that changes
	// nothing, whether he names alice's endpoint or pairs the hook with his own endpoint id.
	assertNotIn(t, "bob's endpoints page", getAs(t, f.r, f.bobTok, "/endpoints").Body.String(), hook.ID, "dispatch.example.com/alice")
	for _, epID := range []string{f.epA.ID, f.epB.ID} {
		bobBase := "/endpoints/" + epID + "/hooks/" + hook.ID
		for _, action := range hookActions {
			if rec := f.bobPost(t, bobBase+action); rec.Code != http.StatusNotFound {
				t.Errorf("bob POST %s%s = %d, want 404", bobBase, action, rec.Code)
			}
			unchanged("bob POST " + bobBase + action)
		}
		if rec := f.bobGet(t, bobBase+"/delete"); rec.Code != http.StatusNotFound {
			t.Errorf("bob GET %s/delete (confirm) = %d, want 404", bobBase, rec.Code)
		}
	}

	// Every control refuses a POST without the CSRF token, or with a forged one, and changes nothing.
	for _, action := range hookActions {
		if rec := postNoCSRF(t, f.r, f.aliceTok, base+action); rec.Code < 400 {
			t.Errorf("POST %s without CSRF = %d, want a refusal", action, rec.Code)
		}
		unchanged("a CSRF-less POST " + action)
		if rec := postForgedCSRF(t, f.r, f.aliceTok, base+action); rec.Code < 400 {
			t.Errorf("POST %s with a forged CSRF token = %d, want a refusal", action, rec.Code)
		}
		unchanged("a forged-CSRF POST " + action)
	}

	// Scenario "Human disables a noisy hook", as the no-JS form submits it: 303 back to the card,
	// disabled_reason = operator, and the card says so.
	csrf := scrapeCSRF(t, body)
	rec := postForm(t, f.r, f.aliceTok, base+"/disable", url.Values{"csrf_token": {csrf}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/endpoints#sb-ep-"+f.epA.ID {
		t.Fatalf("disable = %d → %q", rec.Code, rec.Header().Get("Location"))
	}
	got, _ := f.st.GetNotifyHook(f.ctx, hook.ID, f.epA.ID)
	if got.Enabled || got.DisabledReason == nil || *got.DisabledReason != store.NotifyHookDisabledOperator {
		t.Fatalf("after disable = %+v", got)
	}
	if b := getAs(t, f.r, f.aliceTok, "/endpoints").Body.String(); !strings.Contains(b, "disabled · by you") || !strings.Contains(b, base+"/enable") {
		t.Error("card does not show the operator disable and the re-enable control")
	}

	// Re-enable through htmx: the response is the re-rendered hook section plus a toast into the
	// layout's aria-live #sb-toasts region, which is what announces the change.
	rec = postFormAs(t, f.r, f.aliceTok, csrf, base+"/enable", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("htmx enable = %d", rec.Code)
	}
	frag := rec.Body.String()
	for _, want := range []string{
		`id="sb-ep-hooks-` + f.epA.ID + `"`, `data-sb-hook-state="enabled"`, `id="sb-hook-toggle-` + hook.ID + `"`,
		`hx-swap-oob="afterbegin:#sb-toasts"`, "notify hook re-enabled · https://dispatch.example.com/alice?redacted",
	} {
		if !strings.Contains(frag, want) {
			t.Errorf("htmx enable response is missing %q", want)
		}
	}
	for _, leak := range []string{"alice-query-secret", "whsec_alice_secret_value"} {
		if strings.Contains(frag, leak) {
			t.Errorf("htmx enable response leaks %q", leak)
		}
	}
	if got, _ := f.st.GetNotifyHook(f.ctx, hook.ID, f.epA.ID); !got.Enabled || got.DisabledReason != nil {
		t.Fatalf("after enable = %+v", got)
	}

	// Delete confirms first: the GET is a page naming the redacted hook with the POST form, and
	// only that form's submit deletes.
	confirm := getAs(t, f.r, f.aliceTok, base+"/delete")
	if confirm.Code != http.StatusOK {
		t.Fatalf("delete confirm = %d", confirm.Code)
	}
	cb := confirm.Body.String()
	for _, want := range []string{"data-sb-hook-delete-confirm", "https://dispatch.example.com/alice?redacted", `action="` + base + `/delete"`} {
		if !strings.Contains(cb, want) {
			t.Errorf("delete confirm page is missing %q", want)
		}
	}
	if strings.Contains(cb, "alice-query-secret") {
		t.Error("delete confirm page leaks the hook's query string")
	}
	unchanged("rendering the delete confirm page")
	if rec := postForm(t, f.r, f.aliceTok, base+"/delete", url.Values{"csrf_token": {csrf}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("delete = %d", rec.Code)
	}
	if _, err := f.st.GetNotifyHook(f.ctx, hook.ID, f.epA.ID); err == nil {
		t.Fatal("hook survived delete")
	}
	if b := getAs(t, f.r, f.aliceTok, "/endpoints").Body.String(); !strings.Contains(b, "data-sb-hooks-empty") {
		t.Error("card does not render the empty state after the delete")
	}
	if rec := getAs(t, f.r, f.aliceTok, base+"/delete"); rec.Code != http.StatusNotFound {
		t.Errorf("delete confirm for a deleted hook = %d, want 404", rec.Code)
	}
}

// TestNotifyHookOperatorDisableCounts drives the counter Run installs
// (switchboard_notify_hooks_disabled_total{reason="operator"}): a real disable counts once, a
// repeated disable of an already-disabled hook does not, re-enable does not, and disabling again
// after a re-enable counts again.
func TestNotifyHookOperatorDisableCounts(t *testing.T) {
	r, st, ctx, webh := newDBRouterWeb(t)
	mtr := metrics.New(metrics.Options{})
	wireNotifyHookMetrics(webh, mtr)
	alice, tok := mintSession(t, st, ctx, "hook-counter", "Hook Counter", "hook-counter@example.com")
	ep := seedEndpoint(t, st, ctx, alice.ID, "agent-hook-counter", "hash-hook-counter", "sbk_hookctr", "reviews")
	hook, err := st.CreateNotifyHook(ctx, ep.ID, "https://dispatch.example.com/counter", nil, false, "whsec_counter_secret", 5)
	if err != nil {
		t.Fatalf("create hook: %v", err)
	}
	base := "/endpoints/" + ep.ID + "/hooks/" + hook.ID
	csrf := scrapeCSRF(t, getAs(t, r, tok, "/endpoints").Body.String())

	operatorDisables := func() float64 {
		t.Helper()
		mfs, err := mtr.Registry().Gather()
		if err != nil {
			t.Fatalf("gather: %v", err)
		}
		for _, mf := range mfs {
			if mf.GetName() != "switchboard_notify_hooks_disabled_total" {
				continue
			}
			for _, m := range mf.GetMetric() {
				for _, lp := range m.GetLabel() {
					if lp.GetName() == "reason" && lp.GetValue() == metrics.NotifyDisabledByOp {
						return m.GetCounter().GetValue()
					}
				}
			}
		}
		t.Fatal(`no switchboard_notify_hooks_disabled_total{reason="operator"} series`)
		return 0
	}
	if got := operatorDisables(); got != 0 {
		t.Fatalf("before any disable = %v, want 0", got)
	}
	for i, step := range []struct {
		action string
		want   float64
	}{
		{"/disable", 1}, // a real disable counts
		{"/disable", 1}, // a double submit changes nothing and counts nothing
		{"/enable", 1},  // re-enable is not a disable
		{"/disable", 2}, // a fresh disable after a re-enable counts again
	} {
		if rec := postFormAs(t, r, tok, csrf, base+step.action, nil); rec.Code != http.StatusOK {
			t.Fatalf("step %d POST %s = %d", i, step.action, rec.Code)
		}
		if got := operatorDisables(); got != step.want {
			t.Fatalf("step %d after %s: operator disables = %v, want %v", i, step.action, got, step.want)
		}
	}
}
