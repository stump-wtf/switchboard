package store

// Prior attempts on a claim
//
// A committed claim hands back the todo's most recent closed attempts, newest first, read after the
// claim commits: the second claimer sees the first attempt's summary, a takeover sees the death it
// just recorded, the list stops at five and never holds the attempt the claim opened, and a failed
// read never fails the claim. All against Postgres.
//
// Governing: SPEC-0034 REQ-7 "Attempts on Claim Responses", REQ-4 "Died Versus Failed", REQ-10
// "Tenant Isolation", REQ-19 "Error Handling Standards".
//
// @joestump-agent 09/26/2026 - Added for #321 (epic #313).

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// elapseBackoff makes a failed todo's scheduled retry due now, so the next claim can take it.
func elapseBackoff(t *testing.T, s *Store, ctx context.Context, id string) {
	t.Helper()
	if _, err := s.pool.Exec(ctx, `UPDATE todos SET next_retry_at = now() WHERE id = $1`, id); err != nil {
		t.Fatalf("elapse backoff %s: %v", id, err)
	}
}

// REQ-7 "Second attempt sees the first", then the five-item bound across many attempts.
func TestClaimReturnsPriorAttempts(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "prior-attempts")
	id := seedPending(t, s, ctx, ep, "q", "flaky")

	_, ca, err := s.ClaimTodoWith(ctx, ep, id, "agent:a1", ClaimOpts{TTL: time.Minute, Claimant: "run-1"})
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if ca.Seq != 1 || ca.AttemptsTotal != 1 || ca.Prior != nil {
		t.Fatalf("first claim = %+v, want seq 1 of 1 and no prior attempts", ca)
	}
	if _, err := s.FailTodoWith(ctx, ep, id, "agent:a1", Report{Result: []byte(`{"error":"red"}`),
		Summary: "tests still red", Artifact: "mcp://cairn/Ab12"}); err != nil {
		t.Fatalf("fail: %v", err)
	}
	elapseBackoff(t, s, ctx, id)

	_, ca, err = s.ClaimNextWith(ctx, ep, []string{"q"}, "agent:a1", ClaimOpts{TTL: time.Minute})
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if ca.Seq != 2 || ca.AttemptsTotal != 2 || len(ca.Prior) != 1 {
		t.Fatalf("second claim = seq %d total %d prior %d, want 2/2/1", ca.Seq, ca.AttemptsTotal, len(ca.Prior))
	}
	p := ca.Prior[0]
	if p.Seq != 1 || p.Attempt != 1 || p.ClaimerKind != "endpoint" || p.Claimant != "run-1" ||
		p.Outcome != "failed" || p.Disposition != "retry_scheduled" || p.Died || p.EndedAt == nil ||
		p.Summary != "tests still red" || p.SummaryTruncated || p.Artifact != "mcp://cairn/Ab12" {
		t.Fatalf("prior[0] = %+v, want attempt 1 failed/retry_scheduled with its summary and artifact", p)
	}

	// Seven more claims, each released, give the todo nine attempts: the list stops at five, newest
	// first, and never holds the attempt the claim just opened.
	for i := 0; i < 7; i++ {
		if _, err := s.ReleaseTodoWith(ctx, ep, id, "agent:a1", Report{Summary: "stop"}); err != nil {
			t.Fatalf("release %d: %v", i, err)
		}
		if _, ca, err = s.ClaimTodoWith(ctx, ep, id, "agent:a1", ClaimOpts{TTL: time.Minute}); err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
	}
	if ca.Seq != 9 || len(ca.Prior) != PriorAttemptsMax {
		t.Fatalf("ninth claim = seq %d with %d prior, want seq 9 with %d", ca.Seq, len(ca.Prior), PriorAttemptsMax)
	}
	for i, a := range ca.Prior {
		if a.Seq != 8-i || a.EndedAt == nil || a.Outcome != "released" || a.Summary != "stop" {
			t.Fatalf("prior[%d] = %+v, want seq %d released", i, a, 8-i)
		}
	}
	assertAttemptInvariant(t, s, ctx)
}

// REQ-4 through REQ-7: the claim that takes over a lapsed lease closes the old attempt as
// lease_expired in its own statement, and the prior list, read after commit, already shows it
// closed and died with no report. A reaped attempt reads the same way.
func TestClaimPriorShowsDeaths(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "prior-deaths")

	t.Run("takeover", func(t *testing.T) {
		id := seedPending(t, s, ctx, ep, "q", "takeover")
		if _, _, err := s.ClaimTodoWith(ctx, ep, id, "w1", ClaimOpts{TTL: time.Minute}); err != nil {
			t.Fatalf("claim: %v", err)
		}
		expireLease(t, s, ctx, id)
		_, ca, err := s.ClaimTodoWith(ctx, ep, id, "w2", ClaimOpts{TTL: time.Minute})
		if err != nil {
			t.Fatalf("takeover: %v", err)
		}
		if ca.Seq != 2 || len(ca.Prior) != 1 {
			t.Fatalf("takeover = seq %d with %d prior, want 2 with 1", ca.Seq, len(ca.Prior))
		}
		if p := ca.Prior[0]; p.Outcome != "lease_expired" || !p.Died || p.EndedAt == nil || p.Summary != "" || p.Artifact != "" {
			t.Fatalf("prior[0] = %+v, want a died lease_expired attempt with no report", p)
		}
	})

	t.Run("reaped", func(t *testing.T) {
		id := seedPending(t, s, ctx, ep, "q", "reaped")
		if _, _, err := s.ClaimTodoWith(ctx, ep, id, "w1", ClaimOpts{TTL: time.Minute}); err != nil {
			t.Fatalf("claim: %v", err)
		}
		expireLease(t, s, ctx, id)
		if _, err := s.ReapExpired(ctx); err != nil {
			t.Fatalf("reap: %v", err)
		}
		_, ca, err := s.ClaimNextWith(ctx, ep, []string{"q"}, "w2", ClaimOpts{TTL: time.Minute})
		if err != nil {
			t.Fatalf("claim after reap: %v", err)
		}
		if len(ca.Prior) != 1 || ca.Prior[0].Outcome != "reaped" || !ca.Prior[0].Died || ca.Prior[0].Summary != "" {
			t.Fatalf("prior = %+v, want one died reaped attempt with no summary", ca.Prior)
		}
	})
	assertAttemptInvariant(t, s, ctx)
}

// REQ-10: the prior read is filtered on the todo's endpoint in its own statement, so another
// endpoint's id reads nothing even when asked directly.
func TestPriorAttemptsScopedToEndpoint(t *testing.T) {
	s, ctx := testStore(t)
	epA := seedEndpoint(t, s, ctx, "prior-scope-a")
	epB := seedEndpoint(t, s, ctx, "prior-scope-b")
	id := seedPending(t, s, ctx, epA, "q", "scoped")
	if _, _, err := s.ClaimTodoWith(ctx, epA, id, "w", ClaimOpts{TTL: time.Minute}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := s.FailTodoWith(ctx, epA, id, "w", Report{Summary: "a's words"}); err != nil {
		t.Fatalf("fail: %v", err)
	}
	if got, err := s.priorAttempts(ctx, epA, id, 2); err != nil || len(got) != 1 {
		t.Fatalf("owner's read = %d attempts, %v; want 1", len(got), err)
	}
	if got, err := s.priorAttempts(ctx, epB, id, 2); err != nil || len(got) != 0 {
		t.Fatalf("foreign read = %+v, %v; want nothing", got, err)
	}
}

// REQ-19: a prior read that fails leaves the committed claim alone (Prior nil) and logs the todo id
// and the error, never attempt text.
func TestFillPriorFailureIsBestEffort(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "prior-failure")
	id := seedPending(t, s, ctx, ep, "q", "unread")
	if _, _, err := s.ClaimTodoWith(ctx, ep, id, "w", ClaimOpts{TTL: time.Minute}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	const marker = "summary-marker-prior-5e21"
	if _, err := s.FailTodoWith(ctx, ep, id, "w", Report{Summary: marker}); err != nil {
		t.Fatalf("fail: %v", err)
	}
	var logs bytes.Buffer
	s.log = slog.New(slog.NewTextHandler(&logs, nil))
	t.Cleanup(func() { s.log = nil })

	dead, cancel := context.WithCancel(ctx)
	cancel()
	ca := ClaimedAttempt{Seq: 2, AttemptsTotal: 2}
	s.fillPrior(dead, ep, id, &ca)
	if ca.Prior != nil || ca.Seq != 2 {
		t.Fatalf("after a failed read ca = %+v, want seq kept and no prior", ca)
	}
	out := logs.String()
	if !strings.Contains(out, "prior attempts unread") || !strings.Contains(out, id) {
		t.Fatalf("failed read logged %q, want a warning naming the todo", out)
	}
	if strings.Contains(out, marker) {
		t.Fatalf("the warning carries attempt text: %s", out)
	}
}
