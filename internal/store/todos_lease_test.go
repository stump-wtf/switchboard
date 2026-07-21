package store

import (
	"errors"
	"testing"
	"time"
)

// Governing: SPEC-0003 REQ "Visibility Window, Lease, and Heartbeat".
func TestVisibilityLeaseAndHeartbeat(t *testing.T) {
	s, ctx := testStore(t)

	ep := seedEndpoint(t, s, ctx, "visibility-lease-and-heartbeat")

	td, _, err := s.CreateTodo(ctx, CreateTodoParams{EndpointID: ep, Queue: "q", Title: "t", IdempotencyKey: "lease1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	claimed, err := s.ClaimTodo(ctx, ep, td.ID, "owner-1", time.Hour)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	firstLease := *claimed.LeaseExpiresAt

	// Invisible to other consumers while the lease is live.
	if _, err := s.ClaimNext(ctx, ep, []string{"q"}, "owner-2", time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("live-leased todo must be invisible to others, got %v", err)
	}

	// Only the owner may heartbeat; a non-owner heartbeat is a conflict and does not extend the lease.
	if _, err := s.HeartbeatTodo(ctx, ep, td.ID, "owner-2", 2*time.Hour); !errors.Is(err, ErrConflict) {
		t.Fatalf("non-owner heartbeat should conflict, got %v", err)
	}
	// Owner heartbeat extends the lease without consuming an attempt.
	hb, err := s.HeartbeatTodo(ctx, ep, td.ID, "owner-1", 3*time.Hour)
	if err != nil {
		t.Fatalf("owner heartbeat: %v", err)
	}
	if !hb.LeaseExpiresAt.After(firstLease) {
		t.Fatalf("heartbeat must extend the lease: %v !> %v", hb.LeaseExpiresAt, firstLease)
	}
	if hb.Attempt != claimed.Attempt {
		t.Fatalf("heartbeat must not consume an attempt: %d != %d", hb.Attempt, claimed.Attempt)
	}

	// Only the owner may complete or fail (row exists but not owned → ErrConflict).
	if _, err := s.CompleteTodo(ctx, ep, td.ID, "owner-2", nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("non-owner complete should conflict, got %v", err)
	}
	if _, err := s.FailTodo(ctx, ep, td.ID, "owner-2", nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("non-owner fail should conflict, got %v", err)
	}

	// Heartbeat on an absent todo is ErrNotFound.
	if _, err := s.HeartbeatTodo(ctx, ep, "td_missing", "owner-1", time.Hour); !errors.Is(err, ErrNotFound) {
		t.Fatalf("heartbeat absent should be not found, got %v", err)
	}
}

// Governing: SPEC-0003 REQ "Lease-Expiry Requeue and Reaper (Crash Safety)".
func TestReaperRequeuesAndDeadLetters(t *testing.T) {
	s, ctx := testStore(t)

	ep := seedEndpoint(t, s, ctx, "reaper-requeues-and-dead-letters")

	// Below the attempt cap: an expired lease is requeued to pending with owner/lease cleared.
	a, _, err := s.CreateTodo(ctx, CreateTodoParams{EndpointID: ep, Queue: "q", Title: "a", IdempotencyKey: "reap-a"})
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	if _, err := s.ClaimTodo(ctx, ep, a.ID, "w", time.Hour); err != nil {
		t.Fatalf("claim a: %v", err)
	}
	// At the cap: an expired lease is dead-lettered to failed.
	b, _, err := s.CreateTodo(ctx, CreateTodoParams{EndpointID: ep, Queue: "q", Title: "b", IdempotencyKey: "reap-b"})
	if err != nil {
		t.Fatalf("create b: %v", err)
	}
	if _, err := s.ClaimTodo(ctx, ep, b.ID, "w", time.Hour); err != nil {
		t.Fatalf("claim b: %v", err)
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE todos SET lease_expires_at = now() - interval '1 minute' WHERE id = ANY($1)`,
		[]string{a.ID, b.ID}); err != nil {
		t.Fatalf("expire leases: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE todos SET attempt = max_attempts WHERE id=$1`, b.ID); err != nil {
		t.Fatalf("exhaust b: %v", err)
	}

	n, err := s.ReapExpired(ctx)
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if n != 2 {
		t.Fatalf("reaped %d, want 2", n)
	}
	ga, _ := s.GetTodo(ctx, ep, a.ID)
	if ga.State != "pending" || ga.Owner != "" || ga.LeaseExpiresAt != nil {
		t.Fatalf("below-cap todo should requeue clean: %+v", ga)
	}
	gb, _ := s.GetTodo(ctx, ep, b.ID)
	if gb.State != "failed" {
		t.Fatalf("at-cap todo should dead-letter: state=%s", gb.State)
	}
}
