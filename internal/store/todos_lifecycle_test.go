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

	// Drive it to the terminal 'failed' state by exhausting attempts (max_attempts default 5). A
	// fail below the cap parks in 'failed' with a scheduled retry window (SPEC-0003 Bounded
	// Retries, scheduled backoff) — unclaimable until due, so each loop iteration rewinds the
	// window before re-claiming.
	for i := 0; i < 5; i++ {
		if _, err := s.ClaimTodo(ctx, td.ID, "w", time.Hour); err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
		last, err := s.FailTodo(ctx, td.ID, "w", nil)
		if err != nil {
			t.Fatalf("fail %d: %v", i, err)
		}
		if last.State != "failed" {
			t.Fatalf("fail %d: state=%s want failed", i, last.State)
		}
		if i < 4 {
			if last.NextRetryAt == nil {
				t.Fatalf("fail %d below cap: NextRetryAt is nil, want a scheduled retry window", i)
			}
			// The open retry window blocks a re-claim…
			if _, err := s.ClaimTodo(ctx, td.ID, "w", time.Hour); !errors.Is(err, ErrConflict) {
				t.Fatalf("claim %d during open retry window should conflict, got %v", i, err)
			}
			// …until it elapses (rewound here rather than slept through).
			if _, err := s.pool.Exec(ctx, `UPDATE todos SET next_retry_at = now() - interval '1 second' WHERE id=$1`, td.ID); err != nil {
				t.Fatalf("rewind retry window %d: %v", i, err)
			}
		}
		if i == 4 && last.NextRetryAt != nil {
			t.Fatalf("fail at cap: NextRetryAt=%v, want nil (dead-letter)", last.NextRetryAt)
		}
	}

	// Dead-lettered 'failed' todo (no retry window) is not claimable by a normal claim.
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
