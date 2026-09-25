package store

// Dead-Letter Context Tests
//
// A transition that dead-letters a todo hands the committed-transition hook a Todo carrying the
// attempt it closed (FinalAttempt), and every other transition leaves it nil. These drive the real
// statements against Postgres and assert on what the hook saw, since the hook payload is the
// interface SPEC-0029's notification sinks consume.
//
// Governing: SPEC-0034 REQ-15 "Dead-Letter Context for Notifications"; ADR-0039.
//
// @joestump-agent 09/25/2026 - Added for #330 (epic #313).

import (
	"context"
	"sync"
	"testing"
	"time"
)

// hookRecorder captures every committed transition the store's hook fires, in order.
type hookRecorder struct {
	mu    sync.Mutex
	verbs []string
	todos []Todo
}

func recordHook(t *testing.T, s *Store) *hookRecorder {
	t.Helper()
	r := &hookRecorder{}
	s.SetTodoTransitionHook(func(verb string, td Todo) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.verbs = append(r.verbs, verb)
		r.todos = append(r.todos, td)
	})
	t.Cleanup(func() { s.SetTodoTransitionHook(nil) })
	return r
}

// last returns the newest transition the hook saw for todo id.
func (r *hookRecorder) last(t *testing.T, id string) (string, Todo) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.todos) - 1; i >= 0; i-- {
		if r.todos[i].ID == id {
			return r.verbs[i], r.todos[i]
		}
	}
	t.Fatalf("hook never fired for todo %s", id)
	return "", Todo{}
}

func strOrNil(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// requeueNow makes a failed-below-cap todo's backoff due and runs the retry scheduler, the way a
// real retry reaches pending again.
func requeueNow(t *testing.T, s *Store, ctx context.Context, id string) {
	t.Helper()
	if _, err := s.pool.Exec(ctx,
		`UPDATE todos SET next_retry_at = now() - interval '1 second' WHERE id = $1`, id); err != nil {
		t.Fatalf("make retry due: %v", err)
	}
	if _, err := s.RequeueDueRetries(ctx); err != nil {
		t.Fatalf("requeue: %v", err)
	}
}

// REQ-15 scenario "Dead letter carries its last attempt": attempt 5 of 5 fails with a summary and
// an artifact, and the hook's payload names that attempt with attempts_total = 5. Every earlier
// fail, below the cap, carries no final-attempt context.
func TestDeadLetterHookCarriesFinalAttempt(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "final-attempt-fail")
	id := seedPending(t, s, ctx, ep, "q", "five tries")
	if _, err := s.pool.Exec(ctx, `UPDATE todos SET max_attempts = 5 WHERE id = $1`, id); err != nil {
		t.Fatalf("set max_attempts: %v", err)
	}
	rec := recordHook(t, s)

	for i := 1; i <= 4; i++ {
		if _, _, err := s.ClaimTodoWith(ctx, ep, id, "agent:a1", ClaimOpts{TTL: time.Hour}); err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
		got, err := s.FailTodoWith(ctx, ep, id, "agent:a1", Report{Summary: "red", Artifact: "mcp://cairn/Aa1"})
		if err != nil {
			t.Fatalf("fail %d: %v", i, err)
		}
		verb, hooked := rec.last(t, id)
		if verb != "failed" || hooked.NextRetryAt == nil {
			t.Fatalf("fail %d: hook verb %q next_retry_at %v, want a scheduled retry", i, verb, hooked.NextRetryAt)
		}
		if hooked.FinalAttempt != nil || got.FinalAttempt != nil {
			t.Fatalf("fail %d below the cap carried final-attempt context: %+v", i, hooked.FinalAttempt)
		}
		requeueNow(t, s, ctx, id)
	}

	if _, _, err := s.ClaimTodoWith(ctx, ep, id, "agent:a1", ClaimOpts{TTL: time.Hour}); err != nil {
		t.Fatalf("claim 5: %v", err)
	}
	got, err := s.FailTodoWith(ctx, ep, id, "agent:a1", Report{
		Result: []byte(`{"error":"tests"}`), Summary: "still red", Artifact: "mcp://cairn/Zz9",
	})
	if err != nil {
		t.Fatalf("fail 5: %v", err)
	}
	verb, hooked := rec.last(t, id)
	if verb != "failed" || hooked.State != "failed" || hooked.NextRetryAt != nil {
		t.Fatalf("fail 5: hook verb %q state %q next_retry_at %v, want a dead letter",
			verb, hooked.State, hooked.NextRetryAt)
	}
	fa := hooked.FinalAttempt
	if fa == nil {
		t.Fatal("dead-letter hook payload has no final attempt")
	}
	if fa.Seq != 5 || fa.AttemptsTotal != 5 || fa.Outcome != "failed" || fa.Died ||
		strOrNil(fa.Summary) != "still red" || strOrNil(fa.Artifact) != "mcp://cairn/Zz9" {
		t.Fatalf("final attempt = seq %d total %d outcome %q died %v summary %v artifact %v",
			fa.Seq, fa.AttemptsTotal, fa.Outcome, fa.Died, strOrNil(fa.Summary), strOrNil(fa.Artifact))
	}
	if got.FinalAttempt == nil || *got.FinalAttempt.Summary != "still red" {
		t.Fatalf("FailTodoWith's returned todo lacks the final attempt the hook saw: %+v", got.FinalAttempt)
	}
	// The payload matches the stored history, not a parallel bookkeeping of it.
	if a := newestClosed(t, s, ctx, ep, id); a.Seq != fa.Seq || a.Disposition != "dead_lettered" {
		t.Fatalf("newest attempt = seq %d %s, want seq %d dead_lettered", a.Seq, a.Disposition, fa.Seq)
	}
	assertAttemptInvariant(t, s, ctx)
}

// REQ-15: a reaper dead letter carries a died attempt with no summary, since nobody reported one.
// A reap below the cap re-queues and carries nothing.
func TestReaperDeadLetterCarriesDiedFinalAttempt(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "final-attempt-reap")
	rec := recordHook(t, s)
	claim := func(title string) string {
		id := seedPending(t, s, ctx, ep, "q", title)
		if _, _, err := s.ClaimTodoWith(ctx, ep, id, "agent:a1", ClaimOpts{TTL: time.Hour}); err != nil {
			t.Fatalf("claim %s: %v", title, err)
		}
		return id
	}
	below, atCap := claim("reap-below"), claim("reap-at-cap")
	exhaust(t, s, ctx, atCap)
	expireLease(t, s, ctx, below)
	expireLease(t, s, ctx, atCap)
	if _, err := s.ReapExpired(ctx); err != nil {
		t.Fatalf("reap: %v", err)
	}

	verb, hooked := rec.last(t, atCap)
	if verb != "failed" {
		t.Fatalf("reap at cap: hook verb %q, want failed", verb)
	}
	fa := hooked.FinalAttempt
	if fa == nil {
		t.Fatal("reaper dead letter carried no final attempt")
	}
	if !fa.Died || fa.Outcome != "reaped" || fa.Summary != nil || fa.Artifact != nil ||
		fa.Seq != 1 || fa.AttemptsTotal != 1 {
		t.Fatalf("reaper final attempt = seq %d total %d outcome %q died %v summary %v artifact %v",
			fa.Seq, fa.AttemptsTotal, fa.Outcome, fa.Died, strOrNil(fa.Summary), strOrNil(fa.Artifact))
	}

	if verb, hooked := rec.last(t, below); verb != "pending" || hooked.FinalAttempt != nil {
		t.Fatalf("reap below cap: verb %q final attempt %+v, want a re-queue with none", verb, hooked.FinalAttempt)
	}
	assertAttemptInvariant(t, s, ctx)
}

// REQ-15 on the Board: the owner's fail at the cap dead-letters with the attempt it closed, which
// has no summary or artifact because the Board reports none; below the cap it carries nothing.
func TestBoardFailFinalAttempt(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "final-attempt-board")
	human := ownerOf(t, s, ctx, ep)
	rec := recordHook(t, s)
	claim := func(title string) string {
		id := seedPending(t, s, ctx, ep, "q", title)
		if _, err := s.ClaimTodoOperatorOwned(ctx, human, id, "human:h", time.Hour); err != nil {
			t.Fatalf("board claim %s: %v", title, err)
		}
		return id
	}

	below := claim("board-below")
	if _, err := s.FailTodoOperatorOwned(ctx, human, below, "human:h", nil); err != nil {
		t.Fatalf("board fail below cap: %v", err)
	}
	if _, hooked := rec.last(t, below); hooked.NextRetryAt == nil || hooked.FinalAttempt != nil {
		t.Fatalf("board fail below cap: next_retry_at %v final attempt %+v, want a retry with none",
			hooked.NextRetryAt, hooked.FinalAttempt)
	}

	atCap := claim("board-at-cap")
	exhaust(t, s, ctx, atCap)
	if _, err := s.FailTodoOperatorOwned(ctx, human, atCap, "human:h", nil); err != nil {
		t.Fatalf("board fail at cap: %v", err)
	}
	_, hooked := rec.last(t, atCap)
	fa := hooked.FinalAttempt
	if fa == nil || fa.Outcome != "failed" || fa.Died || fa.Summary != nil || fa.Artifact != nil ||
		fa.Seq != 1 || fa.AttemptsTotal != 1 {
		t.Fatalf("board dead letter final attempt = %+v", fa)
	}
	assertAttemptInvariant(t, s, ctx)
}

// REQ-15, the revocation half, pinned as it stands: the cascade dead-letters inside the revoking
// transaction and fires no transition hook, so no final-attempt payload is emitted for it. Should
// the cascade ever start firing the hook, this test fails and the payload must be wired there too.
func TestRevocationFiresNoTransitionHook(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "final-attempt-revoke")
	id := seedPending(t, s, ctx, ep, "q", "revoked")
	if _, _, err := s.ClaimTodoWith(ctx, ep, id, "agent:a1", ClaimOpts{TTL: time.Hour}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	rec := recordHook(t, s)
	if err := s.RevokeEndpoint(ctx, ep, ownerOf(t, s, ctx, ep)); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if a := newestClosed(t, s, ctx, ep, id); a.Outcome != "revoked" || a.Disposition != "dead_lettered" {
		t.Fatalf("revoked attempt = %s/%s, want revoked/dead_lettered", a.Outcome, a.Disposition)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.verbs) != 0 {
		t.Fatalf("revocation fired the transition hook %d times (%v)", len(rec.verbs), rec.verbs)
	}
}
