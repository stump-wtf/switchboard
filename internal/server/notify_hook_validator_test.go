package server

// The notify-hook SSRF guard as Run wires it from config: every notify-hook test in internal/mcp
// builds its own push.Validator, so none of them would notice Run dropping the allowlist, the http
// opt-in, or its own listen address. These cases exercise notifyHookValidator, the function Run calls.
// Governing: SPEC-0024 REQ-3 "Target Validation (SSRF Guard)".

import (
	"context"
	"testing"

	"github.com/stump-wtf/switchboard/internal/config"
)

func TestNotifyHookValidatorWiring(t *testing.T) {
	ctx := context.Background()
	check := func(t *testing.T, cfg config.Config, url string, wantOK bool) {
		t.Helper()
		v, _, err := notifyHookValidator(cfg)
		if err != nil {
			t.Fatalf("notifyHookValidator: %v", err)
		}
		if err := v.Validate(ctx, url); (err == nil) != wantOK {
			t.Errorf("Validate(%q) with %+v: err = %v, want ok=%v", url, cfg, err, wantOK)
		}
	}
	sameHost := config.Config{Addr: "127.0.0.1:8080", NotifyHookAllowCIDRs: "127.0.0.1/32"}

	// The allowlist reaches the guard: a same-host receiver on another port passes...
	check(t, sameHost, "https://127.0.0.1:9443/hook", true)
	// ...while switchboard's own listen address and port stay refused.
	check(t, sameHost, "https://127.0.0.1:8080/hook", false)
	// With no allowlist, loopback is refused outright.
	check(t, config.Config{Addr: "127.0.0.1:8080"}, "https://127.0.0.1:9443/hook", false)
	// The http opt-in reaches the guard, and is off by default.
	check(t, sameHost, "http://127.0.0.1:9443/hook", false)
	sameHost.PushAllowHTTP = true
	check(t, sameHost, "http://127.0.0.1:9443/hook", true)

	if _, _, err := notifyHookValidator(config.Config{NotifyHookAllowCIDRs: "10.0.0.0/33"}); err == nil {
		t.Error("a malformed allowlist entry must be an error")
	}
}
