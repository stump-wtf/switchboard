// Doorbell heartbeat: re-ringing pending todos nobody claimed.
//
// The push is a hint and the queue is the ledger, but under push-only delivery that was only half
// true: a doorbell that arrived while every worker was mid-turn, or was dropped by a transport
// fault, was never repeated, so the todo sat pending forever. Fifty rows accumulated that way.
// Skipped without SWITCHBOARD_TEST_DATABASE_URL. Governing: ADR-0013; SPEC-0011.
package store

import (
	"context"
	"testing"
	"time"
)

// age backdates a todo's created_at and its ring bookkeeping so a sweep sees it as due.
func age(t *testing.T, s *Store, ctx context.Context, id string, createdAgo time.Duration, attempts int, ringedAgo *time.Duration) {
	t.Helper()
	if ringedAgo == nil {
		_, err := s.pool.Exec(ctx, `UPDATE todos SET created_at = now() - $2::interval,
			ring_attempts = $3, last_ringed_at = NULL WHERE id = $1`, id, createdAgo, attempts)
		if err != nil {
			t.Fatalf("age: %v", err)
		}
		return
	}
	_, err := s.pool.Exec(ctx, `UPDATE todos SET created_at = now() - $2::interval,
		ring_attempts = $3, last_ringed_at = now() - $4::interval WHERE id = $1`,
		id, createdAgo, attempts, *ringedAgo)
	if err != nil {
		t.Fatalf("age: %v", err)
	}
}

func TestRingUnclaimedRepeatsAnUnpickedDoorbell(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "ring", "forge")

	td, _, err := s.CreateTodo(ctx, CreateTodoParams{EndpointID: ep, Queue: "forge", Title: "unpicked", Source: "test"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Freshly created: still inside the first backoff, so it must NOT be rung — a todo whose
	// original doorbell may still be in flight must not be immediately doubled.
	if got, err := s.RingUnclaimed(ctx); err != nil || len(got) != 0 {
		t.Fatalf("fresh todo rung too early: %d rows, err %v", len(got), err)
	}

	age(t, s, ctx, td.ID, 10*time.Minute, 0, nil)
	got, err := s.RingUnclaimed(ctx)
	if err != nil {
		t.Fatalf("ring: %v", err)
	}
	if len(got) != 1 || got[0].ID != td.ID {
		t.Fatalf("expected the aged todo to be rung, got %d rows", len(got))
	}

	// Marked in the same statement, so a second sweep immediately after does not re-select it.
	if again, err := s.RingUnclaimed(ctx); err != nil || len(again) != 0 {
		t.Fatalf("same todo rung twice in a row: %d rows, err %v", len(again), err)
	}
}

// A todo nobody wants must stop costing model turns. Five doorbells over ~7h is the budget.
func TestRingUnclaimedStopsAtTheAttemptCap(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "ring", "forge")
	td, _, err := s.CreateTodo(ctx, CreateTodoParams{EndpointID: ep, Queue: "forge", Title: "nobody wants this", Source: "test"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	long := 24 * time.Hour
	age(t, s, ctx, td.ID, 48*time.Hour, ringMaxAttempts, &long)

	if got, err := s.RingUnclaimed(ctx); err != nil || len(got) != 0 {
		t.Fatalf("a todo at the attempt cap was rung again: %d rows, err %v", len(got), err)
	}
}

// Only pending rows. A claimed todo has a lease and a working owner; ringing it would wake a second
// worker for work already in hand.
func TestRingUnclaimedIgnoresClaimedAndDoneTodos(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "ring", "forge")
	long := 24 * time.Hour

	claimed, _, _ := s.CreateTodo(ctx, CreateTodoParams{EndpointID: ep, Queue: "forge", Title: "in hand", Source: "test"})
	age(t, s, ctx, claimed.ID, 48*time.Hour, 0, &long)
	if _, err := s.ClaimTodo(ctx, ep, claimed.ID, "agent:x", time.Hour); err != nil {
		t.Fatalf("claim: %v", err)
	}

	done, _, _ := s.CreateTodo(ctx, CreateTodoParams{EndpointID: ep, Queue: "forge", Title: "finished", Source: "test"})
	age(t, s, ctx, done.ID, 48*time.Hour, 0, &long)
	if _, err := s.ClaimTodo(ctx, ep, done.ID, "agent:x", time.Hour); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := s.CompleteTodo(ctx, ep, done.ID, "agent:x", nil); err != nil {
		t.Fatalf("complete: %v", err)
	}

	got, err := s.RingUnclaimed(ctx)
	if err != nil {
		t.Fatalf("ring: %v", err)
	}
	for _, r := range got {
		if r.ID == claimed.ID || r.ID == done.ID {
			t.Fatalf("rang a %s todo: %s", r.State, r.ID)
		}
	}
}

// A backlog is exactly when this matters: waking every worker for fifty rows at once is a token
// bomb, since each push costs a model turn.
func TestRingUnclaimedIsCappedPerSweep(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "ring", "forge")
	long := 24 * time.Hour
	for i := 0; i < ringSweepLimit*3; i++ {
		td, _, err := s.CreateTodo(ctx, CreateTodoParams{EndpointID: ep, Queue: "forge",
			Title: "backlog", Source: "test"})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		age(t, s, ctx, td.ID, 48*time.Hour, 1, &long)
	}
	got, err := s.RingUnclaimed(ctx)
	if err != nil {
		t.Fatalf("ring: %v", err)
	}
	if len(got) != ringSweepLimit {
		t.Fatalf("swept %d todos, want the cap of %d — an unbounded sweep is a token bomb", len(got), ringSweepLimit)
	}
}
