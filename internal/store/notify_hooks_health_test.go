package store

// DB-backed tests for RecordNotifyHookDelivery: the single-statement health update and its
// auto-disable. Governing: SPEC-0024 REQ-8 "Hook Health and Auto-Disable", REQ-13 (single
// statements; concurrent failures cannot both miss the threshold).

import (
	"sync"
	"testing"
)

func TestNotifyHookHealthRecordAndDisable(t *testing.T) {
	s, ctx := hookStore(t)
	ep := seedEndpoint(t, s, ctx, "nh-health")
	h, err := s.CreateNotifyHook(ctx, ep, testHookURL, nil, false, "whsec_x", 5)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	status := 503
	for i := 1; i <= 9; i++ {
		disabled, err := s.RecordNotifyHookDelivery(ctx, h.ID, false, &status, "server_error", 10)
		if err != nil || disabled {
			t.Fatalf("failure %d: disabled=%v err=%v", i, disabled, err)
		}
	}
	got, _ := s.GetNotifyHook(ctx, h.ID, ep)
	if got.ConsecutiveFailures != 9 || !got.Enabled || got.LastStatus == nil || *got.LastStatus != 503 ||
		got.LastError == nil || *got.LastError != "server_error" || got.LastAttemptAt == nil {
		t.Fatalf("after 9 failures = %+v", got)
	}

	// Scenario "Recovery resets the count".
	ok := 204
	if disabled, err := s.RecordNotifyHookDelivery(ctx, h.ID, true, &ok, "", 10); err != nil || disabled {
		t.Fatalf("delivered: %v %v", disabled, err)
	}
	got, _ = s.GetNotifyHook(ctx, h.ID, ep)
	if got.ConsecutiveFailures != 0 || got.LastError != nil || *got.LastStatus != 204 {
		t.Fatalf("after a delivery = %+v", got)
	}

	// Scenario "Dead receiver is disabled": the 10th failure in a row disables, in that statement.
	for i := 1; i <= 10; i++ {
		disabled, err := s.RecordNotifyHookDelivery(ctx, h.ID, false, nil, "timeout", 10)
		if err != nil || disabled != (i == 10) {
			t.Fatalf("failure %d: disabled=%v err=%v", i, disabled, err)
		}
	}
	got, _ = s.GetNotifyHook(ctx, h.ID, ep)
	if got.Enabled || got.DisabledReason == nil || *got.DisabledReason != NotifyHookDisabledFailures ||
		got.DisabledAt == nil || got.LastStatus != nil {
		t.Fatalf("after 10 failures = %+v", got)
	}
	// A disabled hook is left alone; and a malformed or unknown id is a no-op.
	if disabled, err := s.RecordNotifyHookDelivery(ctx, h.ID, false, nil, "timeout", 10); err != nil || disabled {
		t.Fatalf("failure on a disabled hook: %v %v", disabled, err)
	}
	if got2, _ := s.GetNotifyHook(ctx, h.ID, ep); got2.ConsecutiveFailures != 10 {
		t.Fatalf("a disabled hook's count moved: %d", got2.ConsecutiveFailures)
	}
	// Nor does a late success rewrite it: another instance's in-flight notification that answers 2xx
	// after the disable must not leave a failures-disabled hook showing 0 failures and no last_error.
	if disabled, err := s.RecordNotifyHookDelivery(ctx, h.ID, true, &ok, "", 10); err != nil || disabled {
		t.Fatalf("success on a disabled hook: %v %v", disabled, err)
	}
	if got2, _ := s.GetNotifyHook(ctx, h.ID, ep); got2.Enabled || got2.ConsecutiveFailures != 10 ||
		got2.LastError == nil || *got2.LastError != "timeout" || got2.LastStatus != nil ||
		got2.DisabledReason == nil || *got2.DisabledReason != NotifyHookDisabledFailures {
		t.Fatalf("a late success rewrote a disabled hook's health: %+v", got2)
	}
	if _, err := s.RecordNotifyHookDelivery(ctx, "not-a-uuid", false, nil, "timeout", 10); err != nil {
		t.Fatalf("malformed id: %v", err)
	}

	// rotate_notify_hook re-enables it (the store half of REQ-8's re-enable rule).
	rot, err := s.RotateNotifyHookSecret(ctx, h.ID, ep, "whsec_y", 0)
	if err != nil || !rot.Enabled || rot.ConsecutiveFailures != 0 {
		t.Fatalf("rotate after auto-disable = %+v, %v", rot, err)
	}
}

// Two instances failing the same hook at once cannot both miss the threshold.
func TestNotifyHookHealthConcurrentFailuresAtThreshold(t *testing.T) {
	s, ctx := hookStore(t)
	for round := 0; round < 5; round++ {
		ep := seedEndpoint(t, s, ctx, "nh-health-race")
		h, err := s.CreateNotifyHook(ctx, ep, testHookURL, nil, false, "whsec_x", 5)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := s.pool.Exec(ctx, `UPDATE notify_hooks SET consecutive_failures = 8 WHERE id = $1`, h.ID); err != nil {
			t.Fatalf("seed failures: %v", err)
		}
		var wg sync.WaitGroup
		results := make([]bool, 2)
		errs := make([]error, 2)
		start := make(chan struct{})
		for i := range results {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				results[i], errs[i] = s.RecordNotifyHookDelivery(ctx, h.ID, false, nil, "network", 10)
			}(i)
		}
		close(start)
		wg.Wait()
		if errs[0] != nil || errs[1] != nil {
			t.Fatalf("errors: %v %v", errs[0], errs[1])
		}
		if results[0] == results[1] {
			t.Fatalf("round %d: disabled flags %v, want exactly one statement to disable", round, results)
		}
		got, _ := s.GetNotifyHook(ctx, h.ID, ep)
		if got.Enabled || got.ConsecutiveFailures != 10 {
			t.Fatalf("round %d: hook = %+v, want disabled at 10", round, got)
		}
	}
}
