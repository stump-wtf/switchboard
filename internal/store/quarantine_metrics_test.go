package store

// Quarantine counters (SPEC-0026 REQ-11), driven through the real SQL paths: a newly held delivery
// counts once under its reason and a redelivery collapsing onto it does not; release, discard and
// expiry each count once, after commit, with the resolver as the row records it; and a release or
// discard that loses (conflict) counts nothing.
//
// @joestump-agent 09/25/2026 - Added for #392.

import (
	"errors"
	"testing"
)

func TestQuarantineMetrics(t *testing.T) {
	s, ctx := testStore(t)
	rec := newRecMetrics()
	s.SetMetrics(rec)
	t.Cleanup(func() { s.SetMetrics(nil) })
	owner := seedEndpoint(t, s, ctx, "qm-owner", "q", "lane-m")
	human := ownerOf(t, s, ctx, owner)

	released, key := holdOn(t, s, ctx, owner)
	// A redelivery collapses onto the held item: not a second held delivery.
	if _, out, _, err := s.CreateIntakeEventTodos(ctx, EventInput{
		Source: "github", Family: "webhook", EventType: "issues", ExternalID: key, TrustMode: "signed", Verified: true,
		Disposition: DispositionQuarantined,
	}, []string{owner}, CreateTodoParams{Title: "again", IdempotencyKey: key, QuarantineReason: "untrusted_actor"}); err != nil || out[0].New {
		t.Fatalf("redelivery = %+v (%v), want the held item back", out, err)
	}
	discarded, _ := holdOn(t, s, ctx, owner)
	expired, _ := holdOn(t, s, ctx, owner)

	held, resolved := rec.quarantineSnapshot()
	if held["untrusted_actor"] != 3 || len(held) != 1 {
		t.Fatalf("held = %v, want untrusted_actor: 3 (the redelivery is not counted)", held)
	}
	if len(resolved) != 0 {
		t.Fatalf("resolved before any resolution = %v", resolved)
	}

	plan := ReleasePlan{TodoID: released.ID, OwnerHumanID: human, By: "classifier:triage", Queue: "lane-m",
		Endpoints: []string{owner}, Trace: []byte(`{"stage":"rule"}`), Verified: true, TrustMode: "signed"}
	if _, err := s.ApplyQuarantineRelease(ctx, plan); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := s.ApplyQuarantineRelease(ctx, plan); !errors.Is(err, ErrConflict) {
		t.Fatalf("second release = %v, want conflict", err)
	}
	if _, err := s.DiscardQuarantined(ctx, human, discarded.ID, "human:"+human, "spam"); err != nil {
		t.Fatalf("discard: %v", err)
	}
	if _, err := s.DiscardQuarantined(ctx, human, discarded.ID, "human:"+human, "again"); !errors.Is(err, ErrConflict) {
		t.Fatalf("second discard = %v, want conflict", err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE todos SET created_at = now() - interval '31 days' WHERE id = $1`, expired.ID); err != nil {
		t.Fatalf("age: %v", err)
	}
	n, err := s.ExpireQuarantine(ctx)
	if err != nil || n < 1 {
		t.Fatalf("expire = %d (%v), want at least the aged item", n, err)
	}

	_, resolved = rec.quarantineSnapshot()
	want := map[string]int{
		"released/classifier:triage": 1,
		"discarded/human:" + human:   1,
		"expired/system":             int(n),
	}
	if len(resolved) != len(want) {
		t.Fatalf("resolved = %v, want %v", resolved, want)
	}
	for k, v := range want {
		if resolved[k] != v {
			t.Errorf("resolved[%s] = %d, want %d (all: %v)", k, resolved[k], v, resolved)
		}
	}
}
