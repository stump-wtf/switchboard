package store

// The dead-letter hook (issue #588): the notify-hook dispatcher's second trigger. These drive the
// real statements against Postgres and assert on exactly which transitions announce a dead letter:
// a fail at the attempt cap, a lease reaped at the cap, and an interrupt wait reclaimed at the cap —
// never a retryable failure, never a plain lease lapse, and never a todo whose delivery failed the
// sender gate.
//
// Governing: SPEC-0024 REQ-6 "Trigger and Sender Gate", SPEC-0034 REQ-15 "Dead-Letter Context for
// Notifications", SPEC-0003 REQ "Bounded Retries via max_attempts".

import (
	"sync"
	"testing"
	"time"
)

// deadLetterRecorder captures every dead-letter transition the store announces, in order.
type deadLetterRecorder struct {
	mu    sync.Mutex
	got   []string
	todos []Todo
}

func (r *deadLetterRecorder) hook(t Todo, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, t.ID+":"+reason)
	r.todos = append(r.todos, t)
}

func (r *deadLetterRecorder) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.got
	r.got, r.todos = nil, nil
	return out
}

// lastTodo returns the newest announced Todo for id, failing the test if there is none.
func (r *deadLetterRecorder) lastTodo(t *testing.T, id string) Todo {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.todos) - 1; i >= 0; i-- {
		if r.todos[i].ID == id {
			return r.todos[i]
		}
	}
	t.Fatalf("dead-letter hook never fired for todo %s", id)
	return Todo{}
}

func recordDeadLetters(t *testing.T, s *Store) *deadLetterRecorder {
	t.Helper()
	r := &deadLetterRecorder{}
	s.SetTodoDeadLetteredHook(r.hook)
	t.Cleanup(func() { s.SetTodoDeadLetteredHook(nil) })
	return r
}

// A fail at the cap announces exactly one dead letter; every fail below it schedules a retry and
// announces nothing, and an unverified sender's todo announces nothing at all.
func TestDeadLetterHookFiresOnlyOnDeadLetters(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "dl-hook", "inbox")
	rec := recordDeadLetters(t, s)

	below := createEventTodo(t, s, ctx, ep, "dl-below", true, "signed").ID
	atCap := createEventTodo(t, s, ctx, ep, "dl-cap", true, "signed").ID
	unverified := createEventTodo(t, s, ctx, ep, "dl-unverified", false, "signed").ID
	rec.take() // creation is not a dead letter, whatever else it announces

	for i := 1; i <= 2; i++ {
		if _, err := s.ClaimTodo(ctx, ep, below, "agent:a1", time.Hour); err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
		if _, err := s.FailTodo(ctx, ep, below, "agent:a1", []byte(`{"error":"red"}`)); err != nil {
			t.Fatalf("fail %d: %v", i, err)
		}
		requeueNow(t, s, ctx, below)
		if got := rec.take(); len(got) != 0 {
			t.Fatalf("fail %d below the cap announced %v, want nothing", i, got)
		}
	}

	exhaust(t, s, ctx, atCap)
	if _, err := s.ClaimTodo(ctx, ep, atCap, "agent:a1", time.Hour); err != nil {
		t.Fatalf("claim at cap: %v", err)
	}
	if _, err := s.FailTodo(ctx, ep, atCap, "agent:a1", []byte(`{"error":"still red"}`)); err != nil {
		t.Fatalf("fail at cap: %v", err)
	}
	got := rec.take()
	if len(got) != 1 || got[0] != atCap+":attempts_exhausted" {
		t.Fatalf("dead-letter fires = %v, want only the at-cap fail", got)
	}
	hooked := rec.lastTodo(t, atCap)
	if hooked.State != "failed" || hooked.NextRetryAt != nil {
		t.Fatalf("announced todo: state %q next_retry_at %v, want a dead letter", hooked.State, hooked.NextRetryAt)
	}
	if !hooked.DeadLetter() {
		t.Fatal("announced todo does not read as a dead letter")
	}
	if fa := hooked.FinalAttempt; fa == nil || fa.Outcome != "failed" || fa.Died {
		t.Fatalf("announced final attempt = %+v, want the attempt that just failed", fa)
	}

	exhaust(t, s, ctx, unverified)
	if _, err := s.ClaimTodo(ctx, ep, unverified, "agent:a1", time.Hour); err != nil {
		t.Fatalf("claim unverified: %v", err)
	}
	if _, err := s.FailTodo(ctx, ep, unverified, "agent:a1", nil); err != nil {
		t.Fatalf("fail unverified: %v", err)
	}
	if got := rec.take(); len(got) != 0 {
		t.Fatalf("an unverified sender's dead letter announced %v, want nothing", got)
	}
	assertAttemptInvariant(t, s, ctx)
}

// The reaper announces a reap at the cap and stays silent about a reap below it, which re-queues and
// is announced by the ready hook as a re-queue instead.
func TestDeadLetterHookOnLeaseReap(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "dl-reap", "inbox")
	rec := recordDeadLetters(t, s)
	ready := &readyRecorder{}
	s.SetTodoReadyHook(ready.hook)
	defer s.SetTodoReadyHook(nil)

	below := createEventTodo(t, s, ctx, ep, "dl-reap-below", true, "signed").ID
	atCap := createEventTodo(t, s, ctx, ep, "dl-reap-cap", true, "signed").ID
	ready.take()
	rec.take()

	for _, id := range []string{below, atCap} {
		if _, err := s.ClaimTodo(ctx, ep, id, "agent:a1", time.Hour); err != nil {
			t.Fatalf("claim %s: %v", id, err)
		}
	}
	exhaust(t, s, ctx, atCap)
	expireLease(t, s, ctx, below)
	expireLease(t, s, ctx, atCap)
	if _, err := s.ReapExpired(ctx); err != nil {
		t.Fatalf("reap: %v", err)
	}

	if got := rec.take(); len(got) != 1 || got[0] != atCap+":lease_expired" {
		t.Fatalf("dead-letter fires = %v, want only the reap at the cap", got)
	}
	if r := ready.take(); len(r) != 1 || r[0] != below+":requeued:1" {
		t.Fatalf("ready fires = %v, want the below-cap todo re-queued", r)
	}
	if hooked := rec.lastTodo(t, atCap); hooked.FinalAttempt == nil || !hooked.FinalAttempt.Died {
		t.Fatalf("announced final attempt = %+v, want the attempt the reap closed", hooked.FinalAttempt)
	}
	assertAttemptInvariant(t, s, ctx)
}

// An interrupt wait reclaimed at the cap announces a dead letter naming its own cause, so an operator
// can tell it from a worker that failed or a lease that lapsed (ADR-0046).
func TestDeadLetterHookOnInterruptReclaim(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "dl-interrupt", "inbox")
	rec := recordDeadLetters(t, s)

	id := createEventTodo(t, s, ctx, ep, "dl-interrupt-1", true, "signed").ID
	rec.take()
	if _, err := s.ClaimTodo(ctx, ep, id, "agent:a1", time.Hour); err != nil {
		t.Fatalf("claim: %v", err)
	}
	exhaust(t, s, ctx, id)
	strandInterrupt(t, s, ctx, id, StateInputRequired, interruptLeaseTTLDefault+time.Hour)
	if _, err := s.ReapExpired(ctx); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if got := rec.take(); len(got) != 1 || got[0] != id+":interrupt_expired" {
		t.Fatalf("dead-letter fires = %v, want the interrupt reclaim at the cap", got)
	}
	assertAttemptInvariant(t, s, ctx)
}

// The Board's fail is a dead letter like the agent's, since the row stops the same way; below the
// cap it schedules a retry and announces nothing.
func TestDeadLetterHookOnOperatorFail(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "dl-board", "inbox")
	human := ownerOf(t, s, ctx, ep)
	rec := recordDeadLetters(t, s)

	id := createEventTodo(t, s, ctx, ep, "dl-board-1", true, "signed").ID
	rec.take()
	if _, err := s.ClaimTodoOperatorOwned(ctx, human, id, "human:h", time.Hour); err != nil {
		t.Fatalf("board claim: %v", err)
	}
	exhaust(t, s, ctx, id)
	if _, err := s.FailTodoOperatorOwned(ctx, human, id, "human:h", nil); err != nil {
		t.Fatalf("board fail at cap: %v", err)
	}
	if got := rec.take(); len(got) != 1 || got[0] != id+":attempts_exhausted" {
		t.Fatalf("dead-letter fires = %v, want the Board's dead letter", got)
	}
	assertAttemptInvariant(t, s, ctx)
}

// A revocation dead-letters its rows inside the revocation transaction, and the revoked endpoint's
// hooks are gone with it, so nothing is announced: this pins that deliberate silence rather than
// letting a future refactor announce a dead letter to a receiver that can no longer verify it.
func TestDeadLetterHookStaysSilentOnRevocation(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "dl-revoke", "inbox")
	human := ownerOf(t, s, ctx, ep)
	rec := recordDeadLetters(t, s)

	id := createEventTodo(t, s, ctx, ep, "dl-revoke-1", true, "signed").ID
	rec.take()
	if _, err := s.ClaimTodo(ctx, ep, id, "agent:a1", time.Hour); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := s.RevokeEndpoint(ctx, ep, human); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if got := rec.take(); len(got) != 0 {
		t.Fatalf("revocation announced %v, want nothing", got)
	}
}
