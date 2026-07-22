package store

import (
	"errors"
	"testing"
	"time"
)

// Governing: SPEC-0002/0004 REQ atomic ingestion — event and todo commit together or not at all.
func TestCreateEventTodoAtomicity(t *testing.T) {
	s, ctx := testStore(t)

	ep := seedEndpoint(t, s, ctx, "create-event-todo-atomicity")

	// Success: one delivery persists an event AND its linked todo together.
	eventID, td, created, err := s.CreateEventTodo(ctx,
		EventInput{Source: "github", Family: "webhook", EventType: "push", ExternalID: "d-ok",
			TrustMode: "signed", Verified: true, Payload: []byte("raw-bytes-are-fine")},
		CreateTodoParams{EndpointID: ep, Queue: "reviews", Source: "github", Kind: "push", Title: "push to main",
			Payload: []byte(`{"ref":"main"}`), IdempotencyKey: "d-ok"})
	if err != nil || !created || eventID == 0 {
		t.Fatalf("happy path: eventID=%d created=%v err=%v", eventID, created, err)
	}
	var linked int64
	if err := s.pool.QueryRow(ctx, `SELECT event_id FROM todos WHERE id=$1`, td.ID).Scan(&linked); err != nil {
		t.Fatalf("read todo event_id: %v", err)
	}
	if linked != eventID {
		t.Fatalf("todo.event_id=%d, want %d", linked, eventID)
	}

	// Injected failure: an invalid jsonb todo payload aborts the todo insert. The event insert that
	// ran first in the same transaction MUST roll back — no orphaned event row may survive.
	_, _, _, err = s.CreateEventTodo(ctx,
		EventInput{Source: "github", Family: "webhook", EventType: "push", ExternalID: "d-orphan",
			TrustMode: "signed", Verified: true, Payload: []byte("raw")},
		CreateTodoParams{EndpointID: ep, Queue: "reviews", Title: "bad", Payload: []byte("this is not json"),
			IdempotencyKey: "d-orphan"})
	if err == nil {
		t.Fatal("invalid jsonb todo payload should fail CreateEventTodo")
	}
	var orphans int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM events WHERE source=$1 AND external_id=$2`, "github", "d-orphan").Scan(&orphans); err != nil {
		t.Fatalf("count orphan events: %v", err)
	}
	if orphans != 0 {
		t.Fatalf("found %d orphaned event(s) after a failed todo insert, want 0", orphans)
	}
}

// Governing: SPEC-0003 lease recovery — the claim scan itself reclaims expired leases, so a stopped
// reaper can never strand a todo.
func TestClaimReclaimsExpiredLeaseWithoutReaper(t *testing.T) {
	s, ctx := testStore(t)

	ep := seedEndpoint(t, s, ctx, "claim-reclaims-expired-lease-without-reaper")

	td, _, err := s.CreateTodo(ctx, CreateTodoParams{EndpointID: ep, Queue: "reviews", Title: "work", IdempotencyKey: "k1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Claim with a live lease. While the lease is valid the todo is NOT reclaimable by anyone else.
	if _, err := s.ClaimTodo(ctx, ep, td.ID, "worker-1", time.Hour); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := s.ClaimNext(ctx, ep, []string{"reviews"}, "worker-2", time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("live-leased todo must not be claimable, got %v", err)
	}

	// Expire the lease directly (simulating a crashed owner) WITHOUT running the reaper.
	if _, err := s.pool.Exec(ctx,
		`UPDATE todos SET lease_expires_at = now() - interval '1 minute' WHERE id=$1`, td.ID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}

	// The claim scan recovers it directly: new owner, attempt incremented, fresh lease.
	reclaimed, err := s.ClaimNext(ctx, ep, []string{"reviews"}, "worker-2", time.Minute)
	if err != nil {
		t.Fatalf("expired lease should be reclaimable by the scan, got %v", err)
	}
	if reclaimed.ID != td.ID || reclaimed.Owner != "worker-2" || reclaimed.Attempt != 2 {
		t.Fatalf("reclaim wrong: id=%s owner=%s attempt=%d", reclaimed.ID, reclaimed.Owner, reclaimed.Attempt)
	}

	// An expired lease that has exhausted its attempts is left for the reaper to dead-letter, not
	// re-handed to a worker forever.
	if _, err := s.pool.Exec(ctx,
		`UPDATE todos SET lease_expires_at = now() - interval '1 minute', attempt = max_attempts WHERE id=$1`,
		td.ID); err != nil {
		t.Fatalf("exhaust attempts: %v", err)
	}
	if _, err := s.ClaimNext(ctx, ep, []string{"reviews"}, "worker-3", time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("exhausted expired lease must not be reclaimed by the scan, got %v", err)
	}
}
