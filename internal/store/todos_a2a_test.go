package store

import (
	"errors"
	"testing"
	"time"
)

// Governing: SPEC-0018 REQ "Task State Machine Extension", scenario "Interrupt states return to
// claimed" — a claimed todo entering input-required/auth-required stays owned+leased and returns to
// 'claimed' (not 'pending') when the requirement is supplied, keeping the same owner and lease.
func TestInterruptStatesRetainOwnerAndLease(t *testing.T) {
	s, ctx := testStore(t)

	td, _, err := s.CreateTodo(ctx, CreateTodoParams{Queue: "q", Title: "t", IdempotencyKey: "a2a-int"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	claimed, err := s.ClaimTodo(ctx, td.ID, "w", time.Hour)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed.Owner != "w" || claimed.LeaseExpiresAt == nil {
		t.Fatalf("post-claim: owner=%q lease=%v; want owner w with a lease", claimed.Owner, claimed.LeaseExpiresAt)
	}
	lease := *claimed.LeaseExpiresAt
	attempt := claimed.Attempt

	for _, state := range []string{StateInputRequired, StateAuthRequired} {
		// Enter the interrupt state: owner and lease are RETAINED, no attempt consumed.
		it, err := s.InterruptTodo(ctx, td.ID, "w", state, []byte(`{"need":"x"}`))
		if err != nil {
			t.Fatalf("interrupt(%s): %v", state, err)
		}
		if it.State != state {
			t.Fatalf("interrupt(%s): state=%s", state, it.State)
		}
		if it.Owner != "w" {
			t.Fatalf("interrupt(%s): owner=%q want w (retained)", state, it.Owner)
		}
		if it.LeaseExpiresAt == nil || !it.LeaseExpiresAt.Equal(lease) {
			t.Fatalf("interrupt(%s): lease=%v want retained %v", state, it.LeaseExpiresAt, lease)
		}
		if it.Attempt != attempt {
			t.Fatalf("interrupt(%s): attempt=%d want %d (no attempt consumed)", state, it.Attempt, attempt)
		}

		// A non-owner must not be able to interrupt or resume it.
		if _, err := s.ResumeTodo(ctx, td.ID, "other", nil); !errors.Is(err, ErrConflict) {
			t.Fatalf("resume(%s) by non-owner should conflict, got %v", state, err)
		}

		// Supplying the requirement returns it to 'claimed' — NOT 'pending' — same owner and lease.
		res, err := s.ResumeTodo(ctx, td.ID, "w", []byte(`{"answer":"y"}`))
		if err != nil {
			t.Fatalf("resume(%s): %v", state, err)
		}
		if res.State != StateClaimed {
			t.Fatalf("resume(%s): state=%s want claimed", state, res.State)
		}
		if res.Owner != "w" {
			t.Fatalf("resume(%s): owner=%q want w (retained)", state, res.Owner)
		}
		if res.LeaseExpiresAt == nil || !res.LeaseExpiresAt.Equal(lease) {
			t.Fatalf("resume(%s): lease=%v want retained %v", state, res.LeaseExpiresAt, lease)
		}
		if res.Attempt != attempt {
			t.Fatalf("resume(%s): attempt=%d want %d", state, res.Attempt, attempt)
		}
	}
}

// Governing: SPEC-0018 REQ "Task State Machine Extension" — InterruptTodo requires a live claim owned
// by the caller and only accepts an interrupt state.
func TestInterruptTodoGuards(t *testing.T) {
	s, ctx := testStore(t)

	td, _, err := s.CreateTodo(ctx, CreateTodoParams{Queue: "q", Title: "t"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// A pending (never-claimed) todo cannot be interrupted.
	if _, err := s.InterruptTodo(ctx, td.ID, "w", StateInputRequired, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("interrupt pending should conflict, got %v", err)
	}
	if _, err := s.ClaimTodo(ctx, td.ID, "w", time.Hour); err != nil {
		t.Fatalf("claim: %v", err)
	}
	// Another owner cannot interrupt someone else's claim.
	if _, err := s.InterruptTodo(ctx, td.ID, "other", StateInputRequired, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("interrupt by non-owner should conflict, got %v", err)
	}
	// A non-interrupt target state is rejected before touching the DB (no ErrConflict/ErrNotFound).
	if _, err := s.InterruptTodo(ctx, td.ID, "w", StateDone, nil); err == nil ||
		errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) {
		t.Fatalf("interrupt to a non-interrupt state should be a validation error, got %v", err)
	}
	// A missing todo is ErrNotFound.
	if _, err := s.InterruptTodo(ctx, "td_missing", "w", StateInputRequired, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("interrupt missing should be ErrNotFound, got %v", err)
	}
}

// Governing: SPEC-0018 REQ "Task State Machine Extension", scenario "Rejected is distinct from
// failed" — a todo rejected before ever being claimed goes to 'rejected', not 'failed', and is NOT
// eligible for retry/backoff.
func TestRejectedIsDistinctFromFailed(t *testing.T) {
	s, ctx := testStore(t)

	td, _, err := s.CreateTodo(ctx, CreateTodoParams{Queue: "q", Title: "t", IdempotencyKey: "a2a-rej"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	rej, err := s.RejectTodo(ctx, td.ID, []byte(`{"reason":"policy"}`))
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	if rej.State != StateRejected {
		t.Fatalf("reject: state=%s want rejected", rej.State)
	}
	// Distinct from failed: no scheduled retry window is ever set.
	if rej.NextRetryAt != nil {
		t.Fatalf("reject: NextRetryAt=%v want nil (never retried)", rej.NextRetryAt)
	}
	// Not eligible for the retry/backoff behavior 'failed' gets: the retry-due scheduler ignores it…
	n, err := s.RequeueDueRetries(ctx)
	if err != nil {
		t.Fatalf("requeue: %v", err)
	}
	if n != 0 {
		t.Fatalf("requeue picked up a rejected todo (n=%d); rejected must not be retried", n)
	}
	// …the claim scan never re-surfaces it…
	if _, err := s.ClaimTodo(ctx, td.ID, "w", time.Hour); !errors.Is(err, ErrConflict) {
		t.Fatalf("claim rejected should conflict, got %v", err)
	}
	// …and the explicit operator RetryTodo (which only re-enqueues 'failed') refuses it.
	if _, err := s.RetryTodo(ctx, td.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("RetryTodo on rejected should conflict (only failed is retryable), got %v", err)
	}
	// Re-fetch confirms it is still 'rejected' — nothing moved it.
	got, err := s.GetTodo(ctx, td.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != StateRejected {
		t.Fatalf("post-retry-attempts: state=%s want rejected", got.State)
	}

	// Reject only applies before a claim: a claimed todo cannot be rejected (it would be a cancel).
	td2, _, err := s.CreateTodo(ctx, CreateTodoParams{Queue: "q", Title: "t2", IdempotencyKey: "a2a-rej2"})
	if err != nil {
		t.Fatalf("create2: %v", err)
	}
	if _, err := s.ClaimTodo(ctx, td2.ID, "w", time.Hour); err != nil {
		t.Fatalf("claim2: %v", err)
	}
	if _, err := s.RejectTodo(ctx, td2.ID, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("reject claimed should conflict, got %v", err)
	}
}

// Governing: SPEC-0018 REQ "CancelTask Is Idempotent" / "Task State Machine Extension" — cancel moves
// a live todo to the terminal 'canceled' state (distinct from failed: never retried), and a second
// cancel of an already-canceled todo is reported as a conflict for the handler to translate into an
// idempotent success.
func TestCancelTerminalAndDistinctFromFailed(t *testing.T) {
	s, ctx := testStore(t)

	// Cancel a pending todo.
	pend, _, err := s.CreateTodo(ctx, CreateTodoParams{Queue: "q", Title: "t", IdempotencyKey: "a2a-can1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	c, err := s.CancelTodo(ctx, pend.ID, []byte(`{"reason":"caller canceled"}`))
	if err != nil {
		t.Fatalf("cancel pending: %v", err)
	}
	if c.State != StateCanceled {
		t.Fatalf("cancel: state=%s want canceled", c.State)
	}
	if c.Owner != "" || c.LeaseExpiresAt != nil || c.NextRetryAt != nil {
		t.Fatalf("cancel: owner=%q lease=%v retry=%v; want all cleared", c.Owner, c.LeaseExpiresAt, c.NextRetryAt)
	}
	// Terminal and distinct from failed: not retryable, not re-claimable, not requeued.
	if _, err := s.RetryTodo(ctx, pend.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("RetryTodo on canceled should conflict, got %v", err)
	}
	if _, err := s.ClaimTodo(ctx, pend.ID, "w", time.Hour); !errors.Is(err, ErrConflict) {
		t.Fatalf("claim canceled should conflict, got %v", err)
	}
	// Idempotency signal: a second cancel finds it already terminal → ErrConflict (handler maps to
	// a no-op success).
	if _, err := s.CancelTodo(ctx, pend.ID, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("second cancel should conflict (already terminal), got %v", err)
	}

	// Cancel a claimed todo too (a worker is mid-flight): still terminal, owner cleared.
	cl, _, err := s.CreateTodo(ctx, CreateTodoParams{Queue: "q", Title: "t2", IdempotencyKey: "a2a-can2"})
	if err != nil {
		t.Fatalf("create2: %v", err)
	}
	if _, err := s.ClaimTodo(ctx, cl.ID, "w", time.Hour); err != nil {
		t.Fatalf("claim2: %v", err)
	}
	c2, err := s.CancelTodo(ctx, cl.ID, nil)
	if err != nil {
		t.Fatalf("cancel claimed: %v", err)
	}
	if c2.State != StateCanceled || c2.Owner != "" {
		t.Fatalf("cancel claimed: state=%s owner=%q want canceled with owner cleared", c2.State, c2.Owner)
	}

	// Cancel is valid from an interrupt state too: a task paused on input/auth is in-progress work
	// and must be cancelable (SPEC-0018 REQ "CancelTask Is Idempotent" — out of claimable/in-progress).
	intr, _, err := s.CreateTodo(ctx, CreateTodoParams{Queue: "q", Title: "t3", IdempotencyKey: "a2a-can3"})
	if err != nil {
		t.Fatalf("create3: %v", err)
	}
	if _, err := s.ClaimTodo(ctx, intr.ID, "w", time.Hour); err != nil {
		t.Fatalf("claim3: %v", err)
	}
	if _, err := s.InterruptTodo(ctx, intr.ID, "w", StateInputRequired, nil); err != nil {
		t.Fatalf("interrupt3: %v", err)
	}
	c3, err := s.CancelTodo(ctx, intr.ID, nil)
	if err != nil {
		t.Fatalf("cancel interrupted: %v", err)
	}
	if c3.State != StateCanceled || c3.Owner != "" {
		t.Fatalf("cancel interrupted: state=%s owner=%q want canceled with owner cleared", c3.State, c3.Owner)
	}

	// A dead-lettered 'failed' todo is a resting state, not in-flight: cancel is not its move.
	dl, _, err := s.CreateTodo(ctx, CreateTodoParams{Queue: "q", Title: "t4", IdempotencyKey: "a2a-can4"})
	if err != nil {
		t.Fatalf("create4: %v", err)
	}
	for i := 0; i < 5; i++ {
		if _, err := s.ClaimTodo(ctx, dl.ID, "w", time.Hour); err != nil {
			t.Fatalf("claim4 %d: %v", i, err)
		}
		if _, err := s.FailTodo(ctx, dl.ID, "w", nil); err != nil {
			t.Fatalf("fail4 %d: %v", i, err)
		}
		if i < 4 {
			if _, err := s.pool.Exec(ctx, `UPDATE todos SET next_retry_at = now() - interval '1 second' WHERE id=$1`, dl.ID); err != nil {
				t.Fatalf("rewind4 %d: %v", i, err)
			}
		}
	}
	if _, err := s.CancelTodo(ctx, dl.ID, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("cancel dead-lettered failed should conflict, got %v", err)
	}

	// Missing todo → ErrNotFound.
	if _, err := s.CancelTodo(ctx, "td_missing", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cancel missing should be ErrNotFound, got %v", err)
	}
}

// Governing: SPEC-0018 REQ "Database Operation Standards", scenario "State transition and audit
// record are atomic" — each A2A transition commits its state change and its outcome (result) record
// together and fires exactly one transition hook, only after the durable commit.
func TestA2ATransitionsFireHookAfterCommit(t *testing.T) {
	s, ctx := testStore(t)

	var verbs []string
	var states []string
	s.SetTodoTransitionHook(func(verb string, td Todo) {
		verbs = append(verbs, verb)
		states = append(states, td.State)
	})
	defer s.SetTodoTransitionHook(nil)

	// Reject fires 'rejected'.
	r, _, err := s.CreateTodo(ctx, CreateTodoParams{Queue: "q", Title: "t", IdempotencyKey: "hk-rej"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	verbs, states = nil, nil
	if _, err := s.RejectTodo(ctx, r.ID, []byte(`{"reason":"x"}`)); err != nil {
		t.Fatalf("reject: %v", err)
	}
	if len(verbs) != 1 || verbs[0] != "rejected" || states[0] != StateRejected {
		t.Fatalf("reject hook = %v/%v, want [rejected]/[rejected]", verbs, states)
	}

	// Interrupt fires the interrupt state verb; resume fires 'claimed'.
	c, _, err := s.CreateTodo(ctx, CreateTodoParams{Queue: "q", Title: "t2", IdempotencyKey: "hk-int"})
	if err != nil {
		t.Fatalf("create2: %v", err)
	}
	if _, err := s.ClaimTodo(ctx, c.ID, "w", time.Hour); err != nil {
		t.Fatalf("claim: %v", err)
	}
	verbs, states = nil, nil
	if _, err := s.InterruptTodo(ctx, c.ID, "w", StateAuthRequired, nil); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	if len(verbs) != 1 || verbs[0] != StateAuthRequired {
		t.Fatalf("interrupt hook = %v, want [auth-required]", verbs)
	}
	verbs, states = nil, nil
	if _, err := s.ResumeTodo(ctx, c.ID, "w", nil); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if len(verbs) != 1 || verbs[0] != "claimed" {
		t.Fatalf("resume hook = %v, want [claimed]", verbs)
	}

	// Cancel fires 'canceled'.
	verbs, states = nil, nil
	if _, err := s.CancelTodo(ctx, c.ID, nil); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if len(verbs) != 1 || verbs[0] != "canceled" {
		t.Fatalf("cancel hook = %v, want [canceled]", verbs)
	}
}
