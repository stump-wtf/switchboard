package store

import (
	"testing"
	"time"
)

// Governing: SPEC-0003 REQ "Idempotent Enqueue and Dedup".
func TestIdempotentEnqueue(t *testing.T) {
	s, ctx := testStore(t)

	// A null (empty) idempotency key never participates in dedup — each enqueue is a distinct row.
	a, createdA, err := s.CreateTodo(ctx, CreateTodoParams{Queue: "q", Title: "no-key-1"})
	if err != nil || !createdA {
		t.Fatalf("create a: created=%v err=%v", createdA, err)
	}
	b, createdB, err := s.CreateTodo(ctx, CreateTodoParams{Queue: "q", Title: "no-key-2"})
	if err != nil || !createdB {
		t.Fatalf("create b: created=%v err=%v", createdB, err)
	}
	if a.ID == b.ID {
		t.Fatal("null idempotency_key must not dedup")
	}

	// A keyed enqueue dedups against the live non-terminal row.
	k1, created1, err := s.CreateTodo(ctx, CreateTodoParams{Queue: "q", Title: "keyed", IdempotencyKey: "dk"})
	if err != nil || !created1 {
		t.Fatalf("create k1: %v", err)
	}
	dup, createdDup, err := s.CreateTodo(ctx, CreateTodoParams{Queue: "q", Title: "keyed-again", IdempotencyKey: "dk"})
	if err != nil {
		t.Fatalf("dedup enqueue: %v", err)
	}
	if createdDup || dup.ID != k1.ID {
		t.Fatalf("expected dedup to existing row: created=%v id=%s want %s", createdDup, dup.ID, k1.ID)
	}

	// Once the keyed todo reaches a terminal state, the same key may enqueue a NEW pending todo — the
	// partial unique index only covers non-terminal rows.
	if _, err := s.ClaimTodo(ctx, k1.ID, "w", time.Hour); err != nil {
		t.Fatalf("claim k1: %v", err)
	}
	if _, err := s.CompleteTodo(ctx, k1.ID, "w", nil); err != nil {
		t.Fatalf("complete k1: %v", err)
	}
	k2, created2, err := s.CreateTodo(ctx, CreateTodoParams{Queue: "q", Title: "keyed-after-done", IdempotencyKey: "dk"})
	if err != nil || !created2 {
		t.Fatalf("re-enqueue after terminal: created=%v err=%v", created2, err)
	}
	if k2.ID == k1.ID {
		t.Fatal("a terminal todo must not block a new pending todo with the same key")
	}
}

// Governing: SPEC-0003 REQ "Bounded Retries via max_attempts".
func TestBoundedRetries(t *testing.T) {
	s, ctx := testStore(t)

	td, _, err := s.CreateTodo(ctx, CreateTodoParams{Queue: "q", Title: "retryme", IdempotencyKey: "br"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if td.MaxAttempts != 5 {
		t.Fatalf("default max_attempts = %d, want 5", td.MaxAttempts)
	}

	// Fail repeatedly: below the cap it returns to pending; at the cap it dead-letters. It must never
	// loop unbounded — after max_attempts failures the state is terminal 'failed'.
	var last Todo
	for i := 1; i <= td.MaxAttempts; i++ {
		c, err := s.ClaimTodo(ctx, td.ID, "w", time.Hour)
		if err != nil {
			t.Fatalf("claim #%d: %v", i, err)
		}
		if c.Attempt != i {
			t.Fatalf("attempt after claim #%d = %d, want %d", i, c.Attempt, i)
		}
		last, err = s.FailTodo(ctx, td.ID, "w", nil)
		if err != nil {
			t.Fatalf("fail #%d: %v", i, err)
		}
	}
	if last.State != "failed" {
		t.Fatalf("after %d failures state=%s, want failed", td.MaxAttempts, last.State)
	}
}
