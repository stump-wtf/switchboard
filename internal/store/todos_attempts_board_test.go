package store

// Board Attempt Read Tests
//
// TodoAttemptsOperatorOwned is the todo drawer's history read. These tests pin its scope against
// real Postgres: the owning human reads every attempt, and a second human, a never-minted id and a
// malformed human id all get the same ErrNotFound, never an empty list that would confirm the id.
//
// Governing: SPEC-0034 REQ-10 "Tenant Isolation" (scenario "Second human on the Board"), REQ-13.
//
// @joestump-agent 09/25/2026 - Added for #329 (epic #313).

import (
	"errors"
	"testing"
	"time"
)

func TestTodoAttemptsOperatorOwnedScopesToOwningHuman(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "board-attempts-h1")
	other := seedEndpoint(t, s, ctx, "board-attempts-h2")
	h1, h2 := ownerOf(t, s, ctx, ep), ownerOf(t, s, ctx, other)

	id := seedPending(t, s, ctx, ep, "q", "board attempts")
	if _, _, err := s.ClaimTodoWith(ctx, ep, id, "agent:a1", ClaimOpts{TTL: time.Hour, Claimant: "fixer/run-1"}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := s.FailTodoWith(ctx, ep, id, "agent:a1", Report{Summary: "tests still red"}); err != nil {
		t.Fatalf("fail: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE todos SET next_retry_at = now() WHERE id = $1`, id); err != nil {
		t.Fatalf("expire backoff: %v", err)
	}
	if _, _, err := s.ClaimTodoWith(ctx, ep, id, "agent:a1", ClaimOpts{TTL: time.Hour, Claimant: "fixer/run-2"}); err != nil {
		t.Fatalf("second claim: %v", err)
	}

	// HAPPY: the owning human reads both attempts, newest (open) first.
	as, total, pruned, err := s.TodoAttemptsOperatorOwned(ctx, h1, id, 20)
	if err != nil {
		t.Fatalf("owner read: %v", err)
	}
	if len(as) != 2 || total != 2 || pruned != 0 {
		t.Fatalf("owner read = %d attempts, total %d, pruned %d; want 2, 2, 0", len(as), total, pruned)
	}
	if as[0].Seq != 2 || as[0].EndedAt != nil || as[0].Claimant != "fixer/run-2" {
		t.Errorf("newest attempt = %+v, want the open seq 2 by fixer/run-2", as[0])
	}
	if as[1].Seq != 1 || as[1].Outcome != "failed" || as[1].Summary != "tests still red" {
		t.Errorf("older attempt = %+v, want seq 1 failed with its summary", as[1])
	}

	// A todo with no attempts yet is an empty history for its owner, not a miss.
	fresh := seedPending(t, s, ctx, ep, "q", "never claimed")
	if as, total, _, err := s.TodoAttemptsOperatorOwned(ctx, h1, fresh, 20); err != nil || len(as) != 0 || total != 0 {
		t.Fatalf("owner read of an unclaimed todo = %d attempts, total %d, err %v; want empty", len(as), total, err)
	}

	// UNHAPPY: every foreign or malformed read is the same ErrNotFound as a never-minted id.
	for name, read := range map[string]func() error{
		"second human":          func() error { _, _, _, err := s.TodoAttemptsOperatorOwned(ctx, h2, id, 20); return err },
		"second human, no hist": func() error { _, _, _, err := s.TodoAttemptsOperatorOwned(ctx, h2, fresh, 20); return err },
		"never-minted id":       func() error { _, _, _, err := s.TodoAttemptsOperatorOwned(ctx, h1, "td_never_minted", 20); return err },
		"empty human":           func() error { _, _, _, err := s.TodoAttemptsOperatorOwned(ctx, "", id, 20); return err },
		"malformed human":       func() error { _, _, _, err := s.TodoAttemptsOperatorOwned(ctx, "not-a-uuid", id, 20); return err },
	} {
		if err := read(); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: err = %v, want ErrNotFound", name, err)
		}
	}
	assertAttemptInvariant(t, s, ctx)
}
