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
		assertCloses(t, rec, name, want, wantExpired, do)
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

// assertCloses runs do and asserts the attempts-closed counter moved by exactly want, with no other
// close counted, and the lease-expiry counter by wantExpired on each queue it names.
func assertCloses(t *testing.T, rec *recMetrics, name string, want, wantExpired map[string]int, do func()) {
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

// REQ-14 on the paths TestAttemptsClosedCounter does not drive: the Board's operator-owned claim
// (takeover), release, complete and fail, and the two other revocation cascades, endpoint expiry and
// friend-edge revocation. Each is a separate statement with its own closed flag and its own count
// call, so each needs its own planted case.
func TestAttemptsClosedCounterBoardAndCascades(t *testing.T) {
	s, ctx, rec := meteredStore(t)
	ep := seedEndpoint(t, s, ctx, "attempts-closed-board")
	human := ownerOf(t, s, ctx, ep)
	op := "op:" + human
	claim := func(queue string) string {
		t.Helper()
		id := seedPending(t, s, ctx, ep, queue, queue)
		if _, err := s.ClaimTodoOperatorOwned(ctx, human, id, op, time.Hour); err != nil {
			t.Fatalf("board claim %s: %v", queue, err)
		}
		return id
	}

	id := claim("b-takeover")
	expireLease(t, s, ctx, id)
	assertCloses(t, rec, "board takeover", map[string]int{"b-takeover/lease_expired": 1},
		map[string]int{"b-takeover": 1}, func() {
			if _, err := s.ClaimTodoOperatorOwned(ctx, human, id, op, time.Hour); err != nil {
				t.Fatalf("board takeover: %v", err)
			}
		})
	// A board claim of a pending todo closes nothing.
	fresh := seedPending(t, s, ctx, ep, "b-fresh", "fresh")
	assertCloses(t, rec, "board claim of pending", map[string]int{}, map[string]int{"b-fresh": 0}, func() {
		if _, err := s.ClaimTodoOperatorOwned(ctx, human, fresh, op, time.Hour); err != nil {
			t.Fatalf("board claim: %v", err)
		}
	})

	id = claim("b-release")
	assertCloses(t, rec, "board release", map[string]int{"b-release/released": 1}, nil, func() {
		if _, err := s.ReleaseTodoOperatorOwned(ctx, human, id, op); err != nil {
			t.Fatalf("board release: %v", err)
		}
	})
	id = claim("b-complete")
	assertCloses(t, rec, "board complete", map[string]int{"b-complete/completed": 1}, nil, func() {
		if _, err := s.CompleteTodoOperatorOwned(ctx, human, id, op, nil); err != nil {
			t.Fatalf("board complete: %v", err)
		}
	})
	id = claim("b-fail")
	assertCloses(t, rec, "board fail", map[string]int{"b-fail/failed": 1}, nil, func() {
		if _, err := s.FailTodoOperatorOwned(ctx, human, id, op, nil); err != nil {
			t.Fatalf("board fail: %v", err)
		}
	})

	// Endpoint expiry: a lapsed lifetime revokes the endpoint and closes its open attempt, counted
	// after the sweep commits. A pending todo on it closes nothing.
	exp := expiryFixture(t, s, ctx, human, "expiring-bot", "attempts-closed-expiry-hash", nil)
	expID := seedPending(t, s, ctx, exp.ID, "reviews", "expiring")
	if _, err := s.ClaimTodo(ctx, exp.ID, expID, "w", time.Hour); err != nil {
		t.Fatalf("claim on expiring endpoint: %v", err)
	}
	_ = seedPending(t, s, ctx, exp.ID, "reviews", "never claimed")
	if _, err := s.pool.Exec(ctx,
		`UPDATE endpoints SET expires_at = now() - interval '1 minute' WHERE id = $1`, exp.ID); err != nil {
		t.Fatalf("backdate expiry: %v", err)
	}
	assertCloses(t, rec, "endpoint expiry", map[string]int{"reviews/revoked": 1}, nil, func() {
		if _, err := s.ExpireEndpoints(ctx); err != nil {
			t.Fatalf("expire endpoints: %v", err)
		}
	})

	// Friend-edge revocation revokes the friend-vended endpoint through the same cascade.
	target := mustHuman(t, s, ctx, "pocket|attempts-closed-friend", "Target")
	_ = mustHuman(t, s, ctx, "pocket|attempts-closed-friend-req", "Requester")
	agent := mustAgent(t, s, ctx, target.ID, "friend-bot")
	edge := mustFriendRequest(t, s, ctx, CreateFriendRequestParams{
		FromPersona: "a@attempts", ToPersona: "b@attempts", ToHuman: target.ID,
		RequestedVerbs: []string{"create_for"},
	})
	slug, _ := MintSlug("friend-bot")
	_, fep, err := s.ApproveFriendRequest(ctx, ApproveFriendRequestParams{
		EdgeID: edge.ID, OwnerHumanID: target.ID, AgentID: agent.ID,
		CredentialHash: "attempts-closed-friend-hash", CredentialPrefix: "sbk_ac", Slug: slug,
	})
	if err != nil {
		t.Fatalf("approve friend request: %v", err)
	}
	fid := seedPending(t, s, ctx, fep.ID, "friend-q", "friend work")
	if _, err := s.ClaimTodo(ctx, fep.ID, fid, "w", time.Hour); err != nil {
		t.Fatalf("claim on friend endpoint: %v", err)
	}
	assertCloses(t, rec, "friend-edge revoke", map[string]int{"friend-q/revoked": 1}, nil, func() {
		if _, err := s.RevokeFriendEdge(ctx, edge.ID, target.ID); err != nil {
			t.Fatalf("revoke friend edge: %v", err)
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
