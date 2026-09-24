package store

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// Attempt deaths, the attempts-closed counter, and the reaper/taker race.
// Governing: SPEC-0034 REQ-4 "Died Versus Failed", REQ-14 "Metrics", REQ-20 "Concurrency Safety".

// REQ-4: a worker that heartbeats once at T and stops dies: the reaper closes its attempt reaped,
// died, with last_heartbeat_at = T and no summary, and the next claimer reads it that way.
func TestReaperRecordsADeath(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "reaper-records-death")
	id := seedPending(t, s, ctx, ep, "q", "abandoned")
	if _, err := s.ClaimTodo(ctx, ep, id, "w1", time.Minute); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := s.HeartbeatTodo(ctx, ep, id, "w1", time.Minute); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	as, _, _ := attemptsOf(t, s, ctx, ep, id)
	hbAt := *as[0].LastHeartbeatAt
	expireLease(t, s, ctx, id)
	if _, err := s.ReapExpired(ctx); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if _, err := s.ClaimTodo(ctx, ep, id, "w2", time.Minute); err != nil {
		t.Fatalf("next claim: %v", err)
	}
	as, _, _ = attemptsOf(t, s, ctx, ep, id)
	if len(as) != 2 {
		t.Fatalf("%d attempts, want the death and the new claim", len(as))
	}
	dead := as[1]
	if dead.Outcome != "reaped" || dead.Disposition != "requeued" || !dead.Died {
		t.Fatalf("dead attempt = %s/%s died=%v, want reaped/requeued died", dead.Outcome, dead.Disposition, dead.Died)
	}
	if dead.LastHeartbeatAt == nil || !dead.LastHeartbeatAt.Equal(hbAt) {
		t.Fatalf("dead attempt last_heartbeat_at = %v, want %v", dead.LastHeartbeatAt, hbAt)
	}
	if dead.Summary != "" || dead.Artifact != "" || dead.SummaryTruncated {
		t.Fatalf("a death has no report, got summary %q artifact %q", dead.Summary, dead.Artifact)
	}
	assertAttemptInvariant(t, s, ctx)
}

// REQ-14: each committed close moves switchboard_todo_attempts_closed_total by exactly one, and each
// death (reaped, lease_expired) moves the lease-expiry counter by exactly one beside it. Every case
// is planted, so a zero cannot pass for a count.
func TestAttemptsClosedCounter(t *testing.T) {
	s, ctx, rec := meteredStore(t)
	ep := seedEndpoint(t, s, ctx, "attempts-closed-counter")
	claim := func(queue string) string {
		t.Helper()
		id := seedPending(t, s, ctx, ep, queue, queue)
		if _, err := s.ClaimTodo(ctx, ep, id, "w", time.Hour); err != nil {
			t.Fatalf("claim %s: %v", queue, err)
		}
		return id
	}
	step := func(name string, want map[string]int, wantExpired map[string]int, do func()) {
		t.Helper()
		beforeClosed := rec.closedSnapshot()
		_, _, _, beforeExpired := rec.snapshot()
		do()
		afterClosed := rec.closedSnapshot()
		_, _, _, afterExpired := rec.snapshot()
		for k, n := range want {
			if got := afterClosed[k] - beforeClosed[k]; got != n {
				t.Fatalf("%s: closed[%s] moved by %d, want %d", name, k, got, n)
			}
		}
		total := 0
		for k := range afterClosed {
			total += afterClosed[k] - beforeClosed[k]
		}
		if total != len(want) {
			t.Fatalf("%s: %d closes counted, want exactly %d", name, total, len(want))
		}
		for q, n := range wantExpired {
			if got := afterExpired[q] - beforeExpired[q]; got != n {
				t.Fatalf("%s: lease expiries on %s moved by %d, want %d", name, q, got, n)
			}
		}
	}

	id := claim("reap")
	expireLease(t, s, ctx, id)
	step("reap", map[string]int{"reap/reaped": 1}, map[string]int{"reap": 1}, func() {
		if n, err := s.ReapExpired(ctx); err != nil || n != 1 {
			t.Fatalf("reap = %d, %v", n, err)
		}
	})

	id = claim("takeover")
	expireLease(t, s, ctx, id)
	step("takeover", map[string]int{"takeover/lease_expired": 1}, map[string]int{"takeover": 1}, func() {
		if _, err := s.ClaimTodo(ctx, ep, id, "w2", time.Hour); err != nil {
			t.Fatalf("takeover: %v", err)
		}
	})

	id = claim("complete")
	step("complete", map[string]int{"complete/completed": 1}, map[string]int{"complete": 0}, func() {
		if _, err := s.CompleteTodo(ctx, ep, id, "w", nil); err != nil {
			t.Fatalf("complete: %v", err)
		}
	})

	id = claim("fail")
	step("fail", map[string]int{"fail/failed": 1}, nil, func() {
		if _, err := s.FailTodo(ctx, ep, id, "w", nil); err != nil {
			t.Fatalf("fail: %v", err)
		}
	})

	id = claim("release")
	step("release", map[string]int{"release/released": 1}, nil, func() {
		if _, err := s.ReleaseTodo(ctx, ep, id, "w"); err != nil {
			t.Fatalf("release: %v", err)
		}
	})

	id = claim("cancel")
	step("cancel", map[string]int{"cancel/canceled": 1}, nil, func() {
		if _, err := s.CancelTodo(ctx, id, nil); err != nil {
			t.Fatalf("cancel: %v", err)
		}
	})

	// A cancel of a todo nobody claimed closes nothing and counts nothing.
	pending := seedPending(t, s, ctx, ep, "cancel-pending", "never claimed")
	step("cancel-pending", map[string]int{}, nil, func() {
		if _, err := s.CancelTodo(ctx, pending, nil); err != nil {
			t.Fatalf("cancel pending: %v", err)
		}
	})

	// Revocation closes every open attempt on the endpoint: the one claimed here, and the takeover's
	// attempt, which is still open from above.
	claim("revoke")
	step("revoke", map[string]int{"revoke/revoked": 1, "takeover/revoked": 1}, nil, func() {
		if err := s.RevokeEndpoint(ctx, ep, ownerOf(t, s, ctx, ep)); err != nil {
			t.Fatalf("revoke: %v", err)
		}
	})
	assertAttemptInvariant(t, s, ctx)
}

// REQ-20: the reaper and a claim_next racing on one lapsed lease close its attempt exactly once.
// Whichever locks the row first changes it; the other's predicate no longer matches. When the reaper
// holds the lock, claim_next's SKIP LOCKED passes over the row and finds nothing, which is a
// legitimate outcome of the race. Repeated so both orders occur, and run under -race in CI.
func TestReaperAndTakerCloseOnce(t *testing.T) {
	s, ctx, rec := meteredStore(t)
	ep := seedEndpoint(t, s, ctx, "reaper-taker-race")
	const rounds = 20
	for i := 0; i < rounds; i++ {
		id := seedPending(t, s, ctx, ep, "race", "lapsed")
		if _, err := s.ClaimTodo(ctx, ep, id, "w", time.Hour); err != nil {
			t.Fatalf("round %d claim: %v", i, err)
		}
		expireLease(t, s, ctx, id)

		beforeClosed := rec.closedSnapshot()
		_, _, _, beforeExpired := rec.snapshot()
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := s.ReapExpired(ctx); err != nil {
				t.Errorf("round %d reap: %v", i, err)
			}
		}()
		var took bool
		go func() {
			defer wg.Done()
			_, err := s.ClaimNext(ctx, ep, []string{"race"}, "taker", time.Hour)
			switch {
			case err == nil:
				took = true
			case !errors.Is(err, ErrNotFound):
				t.Errorf("round %d claim_next: %v", i, err)
			}
		}()
		wg.Wait()

		afterClosed := rec.closedSnapshot()
		_, _, _, afterExpired := rec.snapshot()
		deaths := afterClosed["race/reaped"] - beforeClosed["race/reaped"] +
			afterClosed["race/lease_expired"] - beforeClosed["race/lease_expired"]
		if deaths != 1 {
			t.Fatalf("round %d: the lapsed attempt closed %d times, want exactly 1", i, deaths)
		}
		if n := afterExpired["race"] - beforeExpired["race"]; n != 1 {
			t.Fatalf("round %d: %d lease expiries counted, want 1", i, n)
		}
		as, _, _ := attemptsOf(t, s, ctx, ep, id)
		if !as[len(as)-1].Died {
			t.Fatalf("round %d: the lapsed attempt did not close as a death: %+v", i, as)
		}
		if took && (len(as) != 2 || as[0].EndedAt != nil) {
			t.Fatalf("round %d: the taker won, attempts = %+v, want the death then one open attempt", i, as)
		}
		if !took && len(as) != 1 {
			t.Fatalf("round %d: the reaper won alone, attempts = %+v, want just the death", i, as)
		}
		assertAttemptInvariant(t, s, ctx)
		// Retire the round's todo so the next round's claim_next picks the fresh one.
		if _, err := s.CancelTodo(ctx, id, nil); err != nil {
			t.Fatalf("round %d retire: %v", i, err)
		}
	}
	assertAttemptInvariant(t, s, ctx)
}
