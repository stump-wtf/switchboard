package store

import (
	"errors"
	"testing"
	"time"
)

// Governing: SPEC-0003 REQ "Todo Lifecycle State Machine" — only defined transitions are honored and
// terminal states are final except for an explicit operator retry of a dead-lettered todo.
func TestLifecycleTransitions(t *testing.T) {
	s, ctx := testStore(t)

	td, _, err := s.CreateTodo(ctx, CreateTodoParams{Queue: "q", Title: "t", IdempotencyKey: "lc1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Completing a pending (never-claimed) todo is not a defined transition → ErrConflict.
	if _, err := s.CompleteTodo(ctx, td.ID, "w", nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("complete pending should conflict, got %v", err)
	}
	// Failing a pending todo is likewise undefined → ErrConflict.
	if _, err := s.FailTodo(ctx, td.ID, "w", nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("fail pending should conflict, got %v", err)
	}

	// Drive it to the terminal 'failed' state by exhausting attempts (max_attempts default 5).
	for i := 0; i < 5; i++ {
		if _, err := s.ClaimTodo(ctx, td.ID, "w", time.Hour); err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
		last, err := s.FailTodo(ctx, td.ID, "w", nil)
		if err != nil {
			t.Fatalf("fail %d: %v", i, err)
		}
		if i < 4 && last.State != "pending" {
			t.Fatalf("fail %d below cap: state=%s want pending", i, last.State)
		}
		if i == 4 && last.State != "failed" {
			t.Fatalf("fail at cap: state=%s want failed", last.State)
		}
	}

	// Terminal 'failed' todo is not claimable by a normal claim.
	if _, err := s.ClaimTodo(ctx, td.ID, "w", time.Hour); !errors.Is(err, ErrConflict) {
		t.Fatalf("claim failed todo should conflict, got %v", err)
	}

	// Operator retry re-enters pending with a fresh attempt budget, and it is claimable again.
	rt, err := s.RetryTodo(ctx, td.ID)
	if err != nil || rt.State != "pending" || rt.Attempt != 0 {
		t.Fatalf("retry: state=%s attempt=%d err=%v", rt.State, rt.Attempt, err)
	}
	if _, err := s.ClaimTodo(ctx, td.ID, "w", time.Hour); err != nil {
		t.Fatalf("claim after retry: %v", err)
	}

	// Retrying a non-failed (now claimed) todo is a conflict; retrying an absent todo is not found.
	if _, err := s.RetryTodo(ctx, td.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("retry non-failed should conflict, got %v", err)
	}
	if _, err := s.RetryTodo(ctx, "td_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("retry absent should be not found, got %v", err)
	}
}

// Governing: SPEC-0003 REQ "Atomic Claim …" — direct assignment is respected.
func TestClaimRespectsAssignment(t *testing.T) {
	s, ctx := testStore(t)

	td, _, err := s.CreateTodo(ctx, CreateTodoParams{Queue: "q", Title: "assigned", Assignee: "agent-1", IdempotencyKey: "as1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// A non-assignee cannot claim it — neither by id nor via the pool scan.
	if _, err := s.ClaimTodo(ctx, td.ID, "agent-2", time.Hour); !errors.Is(err, ErrConflict) {
		t.Fatalf("non-assignee ClaimTodo should conflict, got %v", err)
	}
	if _, err := s.ClaimNext(ctx, []string{"q"}, "agent-2", time.Hour); !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-assignee ClaimNext should find no work, got %v", err)
	}
	// The named assignee can.
	if _, err := s.ClaimNext(ctx, []string{"q"}, "agent-1", time.Hour); err != nil {
		t.Fatalf("assignee ClaimNext: %v", err)
	}
}
