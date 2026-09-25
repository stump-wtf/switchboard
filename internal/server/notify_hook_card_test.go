package server

// The endpoint card's notify-hook controls through the real router and a live session: the owner
// sees the redacted hook and can disable, re-enable and delete it; another human gets a plain 404
// for every control and changes nothing; a POST without the CSRF token is refused.
//
// Governing: SPEC-0024 REQ-10 "Operator Web UI" (scenarios "Human disables a noisy hook" and
// "Foreign human"), Security Requirements "CSRF Protection" and "Redirect Validation".

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stump-wtf/switchboard/internal/store"
)

func TestNotifyHookCardControls(t *testing.T) {
	f := newTenancyFixture(t)
	hook, err := f.st.CreateNotifyHook(f.ctx, f.epA.ID, "https://dispatch.example.com/alice?token=alice-query-secret",
		[]string{"reviews"}, false, "whsec_alice_secret_value", 5)
	if err != nil {
		t.Fatalf("create hook: %v", err)
	}
	base := "/endpoints/" + f.epA.ID + "/hooks/" + hook.ID

	// The owner's card shows the hook, redacted, with its controls.
	page := getAs(t, f.r, f.aliceTok, "/endpoints")
	body := page.Body.String()
	for _, want := range []string{"https://dispatch.example.com/alice?redacted", `action="` + base + `/disable"`, `action="` + base + `/delete"`} {
		if !strings.Contains(body, want) {
			t.Errorf("owner's card is missing %q", want)
		}
	}
	for _, leak := range []string{"alice-query-secret", "whsec_alice_secret_value"} {
		if strings.Contains(body, leak) {
			t.Errorf("endpoints page leaks %q", leak)
		}
	}
	// Scenario "Foreign human": bob sees nothing of it, and every control is a 404 that changes nothing.
	assertNotIn(t, "bob's endpoints page", getAs(t, f.r, f.bobTok, "/endpoints").Body.String(), hook.ID, "dispatch.example.com/alice")
	for _, action := range []string{"/disable", "/enable", "/delete"} {
		if rec := f.bobPost(t, base+action); rec.Code != http.StatusNotFound {
			t.Errorf("bob POST %s = %d, want 404", action, rec.Code)
		}
	}
	// Bob also cannot reach it by pairing the hook with his own endpoint id.
	if rec := f.bobPost(t, "/endpoints/"+f.epB.ID+"/hooks/"+hook.ID+"/delete"); rec.Code != http.StatusNotFound {
		t.Errorf("bob POST via his own endpoint = %d, want 404", rec.Code)
	}
	if got, err := f.st.GetNotifyHook(f.ctx, hook.ID, f.epA.ID); err != nil || !got.Enabled {
		t.Fatalf("alice's hook changed under bob: %+v, %v", got, err)
	}

	// A POST without the CSRF token is refused and changes nothing.
	rec := postFormAs(t, f.r, f.aliceTok, "", base+"/disable", nil)
	if rec.Code < 400 {
		t.Errorf("POST without CSRF = %d, want a refusal", rec.Code)
	}
	if got, _ := f.st.GetNotifyHook(f.ctx, hook.ID, f.epA.ID); !got.Enabled {
		t.Fatal("a CSRF-less POST disabled the hook")
	}

	// Scenario "Human disables a noisy hook": disabled_reason = operator, and the card says so.
	csrf := scrapeCSRF(t, body)
	rec = postFormAs(t, f.r, f.aliceTok, csrf, base+"/disable", nil)
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

	// Re-enable clears the reason; delete removes it.
	if rec := postFormAs(t, f.r, f.aliceTok, csrf, base+"/enable", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("enable = %d", rec.Code)
	}
	if got, _ := f.st.GetNotifyHook(f.ctx, hook.ID, f.epA.ID); !got.Enabled || got.DisabledReason != nil {
		t.Fatalf("after enable = %+v", got)
	}
	if rec := postFormAs(t, f.r, f.aliceTok, csrf, base+"/delete", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("delete = %d", rec.Code)
	}
	if _, err := f.st.GetNotifyHook(f.ctx, hook.ID, f.epA.ID); err == nil {
		t.Fatal("hook survived delete")
	}
	if b := getAs(t, f.r, f.aliceTok, "/endpoints").Body.String(); !strings.Contains(b, "data-sb-hooks-empty") {
		t.Error("card does not render the empty state after the delete")
	}
}
