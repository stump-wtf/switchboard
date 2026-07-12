package store

import (
	"errors"
	"testing"
	"time"
)

// Governing: SPEC-0003 REQ "Bounded Retries via max_attempts" (scheduled backoff) — a fail below
// the attempt cap parks the todo in 'failed' with next_retry_at stamped to the exponential-backoff
// schedule, the open window blocks claims, and the elapsed window re-queues (scheduler) or admits a
// direct claim.
func TestFailSchedulesBackoffRetry(t *testing.T) {
	s, ctx := testStore(t)

	td, _, err := s.CreateTodo(ctx, CreateTodoParams{Queue: "q", Title: "flaky", IdempotencyKey: "rb1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.ClaimTodo(ctx, td.ID, "w", time.Hour); err != nil {
		t.Fatalf("claim: %v", err)
	}
	before := time.Now()
	ft, err := s.FailTodo(ctx, td.ID, "w", []byte(`{"reason":"boom"}`))
	if err != nil {
		t.Fatalf("fail: %v", err)
	}
	if ft.State != "failed" {
		t.Fatalf("state after below-cap fail = %s, want failed (retry scheduled)", ft.State)
	}
	if ft.NextRetryAt == nil {
		t.Fatal("NextRetryAt is nil after a below-cap fail, want a scheduled window")
	}
	// Attempt 1 → the 30s base. Bound the stamp between now+base (relative to before the call) and
	// now+base+slack rather than asserting an exact instant.
	wantMin := before.Add(retryBackoff(1))
	wantMax := time.Now().Add(retryBackoff(1) + 5*time.Second)
	if ft.NextRetryAt.Before(wantMin) || ft.NextRetryAt.After(wantMax) {
		t.Fatalf("NextRetryAt = %v, want within [%v, %v] (attempt 1 → %v)", ft.NextRetryAt, wantMin, wantMax, retryBackoff(1))
	}

	// The open window blocks both the direct claim and the pool scan.
	if _, err := s.ClaimTodo(ctx, td.ID, "w", time.Hour); !errors.Is(err, ErrConflict) {
		t.Fatalf("claim during open retry window = %v, want ErrConflict", err)
	}
	if _, err := s.ClaimNext(ctx, []string{"q"}, "w", time.Hour); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ClaimNext during open retry window = %v, want ErrNotFound (nothing claimable)", err)
	}

	// The scheduler ignores a not-yet-due retry…
	if n, err := s.RequeueDueRetries(ctx); err != nil || n != 0 {
		t.Fatalf("RequeueDueRetries (window open) = %d, %v; want 0, nil", n, err)
	}
	// …and re-queues it once due, firing the 'pending' transition hook (the UI's re-surface).
	var verbs []string
	s.SetTodoTransitionHook(func(verb string, _ Todo) { verbs = append(verbs, verb) })
	defer s.SetTodoTransitionHook(nil)
	if _, err := s.pool.Exec(ctx, `UPDATE todos SET next_retry_at = now() - interval '1 second' WHERE id=$1`, td.ID); err != nil {
		t.Fatalf("rewind retry window: %v", err)
	}
	n, err := s.RequeueDueRetries(ctx)
	if err != nil || n != 1 {
		t.Fatalf("RequeueDueRetries (window elapsed) = %d, %v; want 1, nil", n, err)
	}
	if len(verbs) != 1 || verbs[0] != "pending" {
		t.Fatalf("hook verbs after requeue = %v, want [pending]", verbs)
	}
	rq, err := s.GetTodo(ctx, td.ID)
	if err != nil {
		t.Fatalf("get after requeue: %v", err)
	}
	if rq.State != "pending" || rq.Owner != "" || rq.LeaseExpiresAt != nil || rq.NextRetryAt != nil {
		t.Fatalf("after requeue = state:%s owner:%q lease:%v retry:%v, want clean pending",
			rq.State, rq.Owner, rq.LeaseExpiresAt, rq.NextRetryAt)
	}
	// The re-queued todo keeps its attempt count — the budget stays bounded (max_attempts).
	if rq.Attempt != 1 {
		t.Fatalf("attempt after requeue = %d, want 1 (budget preserved)", rq.Attempt)
	}
}

// A due retry is claimable directly by the claim scan — re-queue latency never depends on the
// scheduler tick (SPEC-0003 scenario "Elapsed backoff re-queues": a claim arriving after the
// window MAY take it directly).
func TestDueRetryIsClaimableDirectly(t *testing.T) {
	s, ctx := testStore(t)

	td, _, err := s.CreateTodo(ctx, CreateTodoParams{Queue: "qdirect", Title: "flaky", IdempotencyKey: "rb2"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.ClaimTodo(ctx, td.ID, "w", time.Hour); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := s.FailTodo(ctx, td.ID, "w", nil); err != nil {
		t.Fatalf("fail: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE todos SET next_retry_at = now() - interval '1 second' WHERE id=$1`, td.ID); err != nil {
		t.Fatalf("rewind retry window: %v", err)
	}
	// The pool scan picks the due retry up without any scheduler run, clearing the window.
	c, err := s.ClaimNext(ctx, []string{"qdirect"}, "w2", time.Hour)
	if err != nil {
		t.Fatalf("ClaimNext of due retry: %v", err)
	}
	if c.ID != td.ID || c.State != "claimed" || c.NextRetryAt != nil || c.Attempt != 2 {
		t.Fatalf("due-retry claim = id:%s state:%s retry:%v attempt:%d, want the todo claimed with window cleared attempt 2",
			c.ID, c.State, c.NextRetryAt, c.Attempt)
	}
}

// A dead-letter (fail at the cap) carries no retry window and moves only on the explicit manual
// retry, which also clears any window state (SPEC-0003: "Retry now" is the manual override).
func TestDeadLetterHasNoRetryWindowAndManualRetryOverrides(t *testing.T) {
	s, ctx := testStore(t)

	td, _, err := s.CreateTodo(ctx, CreateTodoParams{Queue: "qdl", Title: "doomed", IdempotencyKey: "rb3"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE todos SET max_attempts=1 WHERE id=$1`, td.ID); err != nil {
		t.Fatalf("cap attempts: %v", err)
	}
	if _, err := s.ClaimTodo(ctx, td.ID, "w", time.Hour); err != nil {
		t.Fatalf("claim: %v", err)
	}
	dl, err := s.FailTodo(ctx, td.ID, "w", nil)
	if err != nil {
		t.Fatalf("fail: %v", err)
	}
	if dl.State != "failed" || dl.NextRetryAt != nil {
		t.Fatalf("dead-letter = state:%s retry:%v, want failed with nil NextRetryAt", dl.State, dl.NextRetryAt)
	}
	// The scheduler never touches a dead-letter.
	if n, err := s.RequeueDueRetries(ctx); err != nil || n != 0 {
		t.Fatalf("RequeueDueRetries over a dead-letter = %d, %v; want 0, nil", n, err)
	}

	// Manual retry ("Retry now") is also the override of an open window: re-fail a fresh todo below
	// the cap, then retry it immediately without waiting out the backoff.
	td2, _, err := s.CreateTodo(ctx, CreateTodoParams{Queue: "qdl", Title: "impatient", IdempotencyKey: "rb4"})
	if err != nil {
		t.Fatalf("create 2: %v", err)
	}
	if _, err := s.ClaimTodo(ctx, td2.ID, "w", time.Hour); err != nil {
		t.Fatalf("claim 2: %v", err)
	}
	if _, err := s.FailTodo(ctx, td2.ID, "w", nil); err != nil {
		t.Fatalf("fail 2: %v", err)
	}
	rt, err := s.RetryTodo(ctx, td2.ID)
	if err != nil {
		t.Fatalf("manual retry during open window: %v", err)
	}
	if rt.State != "pending" || rt.NextRetryAt != nil || rt.Attempt != 0 {
		t.Fatalf("manual retry = state:%s retry:%v attempt:%d, want pending, window cleared, fresh budget",
			rt.State, rt.NextRetryAt, rt.Attempt)
	}
}
