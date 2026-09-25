package store

// Lease-token fence tests
//
// Real-Postgres proofs of the fence on the agent's heartbeat, complete, fail and release: a matching
// token applies, every other token/fence combination is ErrConflict and changes nothing, the owner
// check still narrows, and the Board's paths stay exempt. Every test ends with
// assertAttemptInvariant.
//
// Governing: SPEC-0034 REQ-6 "Lease Token Fence", REQ-19 "Error Handling Standards"; ADR-0039.
//
// @joestump-agent 09/25/2026 - Added for #325 (epic #313).

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"
)

// fenceHash is the digest the MCP layer passes for a presented token.
func fenceHash(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// fencedVerb is one agent lease verb driven with a presented token hash (nil = no token).
type fencedVerb struct {
	name string
	call func(s *Store, ctx context.Context, ep, id, owner string, hash []byte) (Todo, error)
	// state and outcome are what a successful call leaves: the todo's state and, for a closing
	// verb, the attempt's outcome ("" = the attempt stays open).
	state, outcome string
}

var fencedVerbs = []fencedVerb{
	{"heartbeat", func(s *Store, ctx context.Context, ep, id, owner string, h []byte) (Todo, error) {
		return s.HeartbeatTodoWith(ctx, ep, id, owner, time.Hour, h)
	}, "claimed", ""},
	{"complete", func(s *Store, ctx context.Context, ep, id, owner string, h []byte) (Todo, error) {
		return s.CompleteTodoWith(ctx, ep, id, owner, Report{Summary: "ok", TokenHash: h})
	}, "done", "completed"},
	{"fail", func(s *Store, ctx context.Context, ep, id, owner string, h []byte) (Todo, error) {
		return s.FailTodoWith(ctx, ep, id, owner, Report{Summary: "no", TokenHash: h})
	}, "failed", "failed"},
	{"release", func(s *Store, ctx context.Context, ep, id, owner string, h []byte) (Todo, error) {
		return s.ReleaseTodoWith(ctx, ep, id, owner, Report{TokenHash: h})
	}, "pending", "released"},
}

// claimFenced claims id for owner, fenced by token when it is non-empty, and returns the claim.
func claimFenced(t *testing.T, s *Store, ctx context.Context, ep, id, owner, token string) Todo {
	t.Helper()
	o := ClaimOpts{TTL: time.Hour}
	if token != "" {
		o.TokenHash = fenceHash(token)
	}
	td, _, err := s.ClaimTodoWith(ctx, ep, id, owner, o)
	if err != nil {
		t.Fatalf("claim %s: %v", id, err)
	}
	return td
}

// assertUntouched proves a missed call changed nothing: the todo is still claimed by owner with the
// same lease, and its newest attempt is still open with no heartbeat.
func assertUntouched(t *testing.T, s *Store, ctx context.Context, ep, id, owner string, lease time.Time) {
	t.Helper()
	td, err := s.GetTodo(ctx, ep, id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	if td.State != "claimed" || td.Owner != owner || td.LeaseExpiresAt == nil || !td.LeaseExpiresAt.Equal(lease) {
		t.Fatalf("missed call moved the todo: state=%s owner=%s lease=%v (want claimed/%s/%v)",
			td.State, td.Owner, td.LeaseExpiresAt, owner, lease)
	}
	as, _, _ := attemptsOf(t, s, ctx, ep, id)
	if as[0].EndedAt != nil || as[0].LastHeartbeatAt != nil {
		t.Fatalf("missed call touched the open attempt: %+v", as[0])
	}
}

// REQ-6: the stored fence is the SHA-256 of the token, never the token, and only on a fenced claim.
func TestFenceStoresOnlyTheHash(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "fence-hash")
	fenced := seedPending(t, s, ctx, ep, "q", "fenced")
	open := seedPending(t, s, ctx, ep, "q", "open")
	claimFenced(t, s, ctx, ep, fenced, "agent:a", "tok-fenced")
	claimFenced(t, s, ctx, ep, open, "agent:a", "")
	for id, want := range map[string][]byte{fenced: fenceHash("tok-fenced"), open: nil} {
		var got []byte
		if err := s.pool.QueryRow(ctx,
			`SELECT lease_token_hash FROM todo_attempts WHERE todo_id = $1 AND ended_at IS NULL`, id).Scan(&got); err != nil {
			t.Fatalf("read fence %s: %v", id, err)
		}
		if string(got) != string(want) {
			t.Fatalf("todo %s fence = %x, want %x", id, got, want)
		}
	}
	assertAttemptInvariant(t, s, ctx)
}

// REQ-6: on a fenced attempt, the holder's token applies every verb and closes the attempt with the
// verb's outcome.
func TestFenceRightTokenApplies(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "fence-right-token")
	for _, v := range fencedVerbs {
		t.Run(v.name, func(t *testing.T) {
			id := seedPending(t, s, ctx, ep, "q", v.name)
			claimFenced(t, s, ctx, ep, id, "agent:a", "tok-"+v.name)
			td, err := v.call(s, ctx, ep, id, "agent:a", fenceHash("tok-"+v.name))
			if err != nil {
				t.Fatalf("%s with the right token: %v", v.name, err)
			}
			if td.State != v.state {
				t.Fatalf("%s left state %s, want %s", v.name, td.State, v.state)
			}
			as, _, _ := attemptsOf(t, s, ctx, ep, id)
			if v.outcome == "" {
				if as[0].EndedAt != nil || as[0].LastHeartbeatAt == nil {
					t.Fatalf("heartbeat did not stamp the open attempt: %+v", as[0])
				}
			} else if as[0].EndedAt == nil || as[0].Outcome != v.outcome {
				t.Fatalf("%s closed the attempt as %q (ended %v), want %s", v.name, as[0].Outcome, as[0].EndedAt, v.outcome)
			}
		})
	}
	assertAttemptInvariant(t, s, ctx)
}

// REQ-6 scenario "The agent cannot close its supervisor's attempt", generalized to every verb: an
// agent with the same credential and owner string, presenting no token, misses with ErrConflict and
// the todo stays claimed.
func TestFenceNoTokenConflicts(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "fence-no-token")
	for _, v := range fencedVerbs {
		t.Run(v.name, func(t *testing.T) {
			id := seedPending(t, s, ctx, ep, "q", v.name)
			claimed := claimFenced(t, s, ctx, ep, id, "agent:a", "supervisor")
			if _, err := v.call(s, ctx, ep, id, "agent:a", nil); !errors.Is(err, ErrConflict) {
				t.Fatalf("%s with no token on a fenced attempt = %v, want ErrConflict", v.name, err)
			}
			assertUntouched(t, s, ctx, ep, id, "agent:a", *claimed.LeaseExpiresAt)
		})
	}
	assertAttemptInvariant(t, s, ctx)
}

// REQ-6: a token presented for an unfenced attempt misses too: "neither" is the only token-less
// combination that applies.
func TestFenceTokenOnUnfencedConflicts(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "fence-unfenced-token")
	for _, v := range fencedVerbs {
		t.Run(v.name, func(t *testing.T) {
			id := seedPending(t, s, ctx, ep, "q", v.name)
			claimed := claimFenced(t, s, ctx, ep, id, "agent:a", "")
			if _, err := v.call(s, ctx, ep, id, "agent:a", fenceHash("made-up")); !errors.Is(err, ErrConflict) {
				t.Fatalf("%s with a token on an unfenced attempt = %v, want ErrConflict", v.name, err)
			}
			assertUntouched(t, s, ctx, ep, id, "agent:a", *claimed.LeaseExpiresAt)
		})
	}
	assertAttemptInvariant(t, s, ctx)
}

// REQ-19 scenario "Fence mismatch is conflict": a wrong token on the caller's own todo is
// ErrConflict, never a permission error, and the lease does not move.
func TestFenceWrongTokenHeartbeatConflicts(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "fence-wrong-token")
	id := seedPending(t, s, ctx, ep, "q", "hb")
	claimed := claimFenced(t, s, ctx, ep, id, "agent:a", "right")
	_, err := s.HeartbeatTodoWith(ctx, ep, id, "agent:a", 2*time.Hour, fenceHash("wrong"))
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("heartbeat with a wrong token = %v, want ErrConflict", err)
	}
	assertUntouched(t, s, ctx, ep, id, "agent:a", *claimed.LeaseExpiresAt)
	assertAttemptInvariant(t, s, ctx)
}

// REQ-6: the fence narrows the owner check and never widens it. The right token under another owner
// string is ErrConflict, and under another endpoint it is ErrNotFound, exactly as without a fence.
func TestFenceKeepsOwnerAndTenantChecks(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "fence-owner")
	other := seedEndpoint(t, s, ctx, "fence-owner-other")
	id := seedPending(t, s, ctx, ep, "q", "owned")
	claimed := claimFenced(t, s, ctx, ep, id, "agent:a", "tok")
	if _, err := s.CompleteTodoWith(ctx, ep, id, "agent:b", Report{TokenHash: fenceHash("tok")}); !errors.Is(err, ErrConflict) {
		t.Fatalf("right token, wrong owner = %v, want ErrConflict", err)
	}
	if _, err := s.CompleteTodoWith(ctx, other, id, "agent:a", Report{TokenHash: fenceHash("tok")}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("right token, foreign endpoint = %v, want ErrNotFound", err)
	}
	assertUntouched(t, s, ctx, ep, id, "agent:a", *claimed.LeaseExpiresAt)
	assertAttemptInvariant(t, s, ctx)
}

// REQ-6 scenario "A stale worker's token": attempt 1's lease lapsed and the same agent identity
// claimed attempt 2 with a fence. Attempt 1's worker presenting its old token (or none) is
// ErrConflict on every verb, and attempt 2 is untouched; attempt 2's own token still applies.
func TestFenceStaleWorkerToken(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "fence-stale-worker")
	id := seedPending(t, s, ctx, ep, "q", "stale")
	claimFenced(t, s, ctx, ep, id, "agent:a", "attempt-1")
	expireLease(t, s, ctx, id)
	second := claimFenced(t, s, ctx, ep, id, "agent:a", "attempt-2")
	if second.Attempt != 2 {
		t.Fatalf("takeover claimed attempt %d, want 2", second.Attempt)
	}
	for _, v := range fencedVerbs {
		for _, h := range [][]byte{fenceHash("attempt-1"), nil} {
			if _, err := v.call(s, ctx, ep, id, "agent:a", h); !errors.Is(err, ErrConflict) {
				t.Fatalf("stale %s (token %x) = %v, want ErrConflict", v.name, h, err)
			}
		}
	}
	assertUntouched(t, s, ctx, ep, id, "agent:a", *second.LeaseExpiresAt)
	as, _, _ := attemptsOf(t, s, ctx, ep, id)
	if len(as) != 2 || as[0].Seq != 2 || as[1].Outcome != "lease_expired" {
		t.Fatalf("attempts after the stale calls = %+v, want open seq 2 over a lease_expired seq 1", as)
	}
	if _, err := s.CompleteTodoWith(ctx, ep, id, "agent:a", Report{TokenHash: fenceHash("attempt-2")}); err != nil {
		t.Fatalf("attempt 2's holder complete: %v", err)
	}
	if got := newestClosed(t, s, ctx, ep, id); got.Seq != 2 || got.Outcome != "completed" {
		t.Fatalf("attempt 2 closed as %+v, want seq 2 completed", got)
	}
	assertAttemptInvariant(t, s, ctx)
}

// REQ-6 scenario "Unfenced client unchanged": a claim without a fence and the old token-less calls
// behave exactly as before, through both the old signatures and the ...With ones with nil hashes.
func TestFenceUnfencedClientUnchanged(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "fence-unfenced-client")
	id := seedPending(t, s, ctx, ep, "q", "old")
	if _, err := s.ClaimTodo(ctx, ep, id, "agent:a", time.Hour); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := s.HeartbeatTodo(ctx, ep, id, "agent:a", time.Hour); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if _, err := s.HeartbeatTodoWith(ctx, ep, id, "agent:a", time.Hour, nil); err != nil {
		t.Fatalf("heartbeat with nil hash: %v", err)
	}
	if _, err := s.CompleteTodo(ctx, ep, id, "agent:a", []byte(`{"ok":true}`)); err != nil {
		t.Fatalf("complete: %v", err)
	}
	for _, v := range fencedVerbs[1:] {
		id := seedPending(t, s, ctx, ep, "q", v.name)
		claimFenced(t, s, ctx, ep, id, "agent:a", "")
		if td, err := v.call(s, ctx, ep, id, "agent:a", nil); err != nil || td.State != v.state {
			t.Fatalf("unfenced %s = %s, %v; want %s", v.name, td.State, err, v.state)
		}
	}
	assertAttemptInvariant(t, s, ctx)
}

// REQ-6: the Board's actions are exempt, so the owning human can always recover a fenced attempt;
// each closes it with the action's outcome.
func TestFenceBoardActionsExempt(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "fence-board")
	human := ownerOf(t, s, ctx, ep)

	id := seedPending(t, s, ctx, ep, "q", "board-release")
	claimFenced(t, s, ctx, ep, id, "agent:a", "harness")
	if _, err := s.HeartbeatTodoOperatorOwned(ctx, human, id, "agent:a", time.Hour); err != nil {
		t.Fatalf("board heartbeat on a fenced attempt: %v", err)
	}
	td, err := s.ReleaseTodoOperatorOwned(ctx, human, id, "agent:a")
	if err != nil {
		t.Fatalf("board release on a fenced attempt: %v", err)
	}
	if td.State != "pending" {
		t.Fatalf("board release left state %s, want pending", td.State)
	}
	if got := newestClosed(t, s, ctx, ep, id); got.Outcome != "released" || got.Disposition != "requeued" {
		t.Fatalf("board release closed the attempt as %s/%s, want released/requeued", got.Outcome, got.Disposition)
	}

	id = seedPending(t, s, ctx, ep, "q", "board-fail")
	claimFenced(t, s, ctx, ep, id, "agent:a", "harness")
	if _, err := s.FailTodoOperatorOwned(ctx, human, id, "agent:a", nil); err != nil {
		t.Fatalf("board fail on a fenced attempt: %v", err)
	}
	if got := newestClosed(t, s, ctx, ep, id); got.Outcome != "failed" {
		t.Fatalf("board fail closed the attempt as %s, want failed", got.Outcome)
	}

	id = seedPending(t, s, ctx, ep, "q", "board-complete")
	claimFenced(t, s, ctx, ep, id, "agent:a", "harness")
	if _, err := s.CompleteTodoOperatorOwned(ctx, human, id, "agent:a", nil); err != nil {
		t.Fatalf("board complete on a fenced attempt: %v", err)
	}
	if got := newestClosed(t, s, ctx, ep, id); got.Outcome != "completed" {
		t.Fatalf("board complete closed the attempt as %s, want completed", got.Outcome)
	}
	assertAttemptInvariant(t, s, ctx)
}
