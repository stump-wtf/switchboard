package store

// DB-backed tests for the owning human's notify-hook surface (the endpoint card). Governing:
// SPEC-0024 REQ-10 "Operator Web UI" (owner scope only; the operator role is not widened).

import (
	"errors"
	"testing"
)

func TestNotifyHooksForHuman(t *testing.T) {
	s, ctx := hookStore(t)
	epA := seedEndpoint(t, s, ctx, "nh-card-a")
	epB := seedEndpoint(t, s, ctx, "nh-card-b")
	ownerA, ownerB := ownerOf(t, s, ctx, epA), ownerOf(t, s, ctx, epB)
	hA, err := s.CreateNotifyHook(ctx, epA, testHookURL, nil, false, "whsec_a", 5)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateNotifyHook(ctx, epB, testHookURL+"/b", nil, false, "whsec_b", 5); err != nil {
		t.Fatal(err)
	}

	byEp, err := s.ListNotifyHooksForHuman(ctx, ownerA)
	if err != nil || len(byEp) != 1 || len(byEp[epA]) != 1 || byEp[epA][0].ID != hA.ID {
		t.Fatalf("A's hooks = %+v, %v", byEp, err)
	}

	// Another human: not found on every control, and nothing changes.
	if _, err := s.SetNotifyHookEnabledForHuman(ctx, hA.ID, epA, ownerB, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign disable = %v", err)
	}
	if err := s.DeleteNotifyHookForHuman(ctx, hA.ID, epA, ownerB); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign delete = %v", err)
	}
	// The right owner with the wrong endpoint is not found either.
	if _, err := s.SetNotifyHookEnabledForHuman(ctx, hA.ID, epB, ownerA, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("mismatched endpoint = %v", err)
	}
	if got, _ := s.GetNotifyHook(ctx, hA.ID, epA); !got.Enabled {
		t.Fatal("a refused control changed the hook")
	}

	// Disable records the operator reason; a repeat changes nothing; re-enable clears it and the count.
	if changed, err := s.SetNotifyHookEnabledForHuman(ctx, hA.ID, epA, ownerA, false); err != nil || !changed {
		t.Fatalf("disable = %v, %v", changed, err)
	}
	got, _ := s.GetNotifyHook(ctx, hA.ID, epA)
	if got.Enabled || got.DisabledReason == nil || *got.DisabledReason != NotifyHookDisabledOperator || got.DisabledAt == nil {
		t.Fatalf("after disable = %+v", got)
	}
	if changed, err := s.SetNotifyHookEnabledForHuman(ctx, hA.ID, epA, ownerA, false); err != nil || changed {
		t.Fatalf("repeat disable = %v, %v", changed, err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE notify_hooks SET consecutive_failures = 4 WHERE id = $1`, hA.ID); err != nil {
		t.Fatal(err)
	}
	if changed, err := s.SetNotifyHookEnabledForHuman(ctx, hA.ID, epA, ownerA, true); err != nil || !changed {
		t.Fatalf("enable = %v, %v", changed, err)
	}
	got, _ = s.GetNotifyHook(ctx, hA.ID, epA)
	if !got.Enabled || got.DisabledReason != nil || got.DisabledAt != nil || got.ConsecutiveFailures != 0 {
		t.Fatalf("after enable = %+v", got)
	}

	if err := s.DeleteNotifyHookForHuman(ctx, hA.ID, epA, ownerA); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := s.DeleteNotifyHookForHuman(ctx, hA.ID, epA, ownerA); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete = %v", err)
	}
	for _, bad := range []string{"", "not-a-uuid"} {
		if _, err := s.SetNotifyHookEnabledForHuman(ctx, bad, epA, ownerA, true); !errors.Is(err, ErrNotFound) {
			t.Fatalf("malformed id %q = %v", bad, err)
		}
	}
}
