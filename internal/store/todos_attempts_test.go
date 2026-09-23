package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Attempt history store tests. Every test that drives a lifecycle path ends with
// assertAttemptInvariant, which proves that path leaked no open attempt and closed none it should
// have left open. Governing: SPEC-0034 REQ-1, 2, 3, 4, 11, 12, 17, 18, 20; ADR-0039.

// assertAttemptInvariant checks the table-wide rule the lifecycle statements maintain: a todo that
// holds a lease (claimed or an interrupt state) has exactly one open attempt, and every other todo
// has none.
func assertAttemptInvariant(t *testing.T, s *Store, ctx context.Context) {
	t.Helper()
	rows, err := s.pool.Query(ctx, `
		SELECT t.id, t.state, count(a.seq) FILTER (WHERE a.ended_at IS NULL)
		FROM todos t LEFT JOIN todo_attempts a ON a.todo_id = t.id
		GROUP BY t.id, t.state`)
	if err != nil {
		t.Fatalf("invariant query: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, state string
		var open int
		if err := rows.Scan(&id, &state, &open); err != nil {
			t.Fatalf("invariant scan: %v", err)
		}
		want := 0
		if state == "claimed" || IsInterruptState(state) {
			want = 1
		}
		if open != want {
			t.Errorf("todo %s in state %s has %d open attempts, want %d", id, state, open, want)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("invariant rows: %v", err)
	}
}

// attemptsOf reads a todo's attempts through the scoped store read, failing the test on error.
func attemptsOf(t *testing.T, s *Store, ctx context.Context, ep, id string) ([]Attempt, int, int) {
	t.Helper()
	as, total, pruned, err := s.TodoAttempts(ctx, ep, id, 50)
	if err != nil {
		t.Fatalf("TodoAttempts(%s): %v", id, err)
	}
	return as, total, pruned
}

// newestClosed returns the outcome and disposition of a todo's newest attempt, which must be closed.
func newestClosed(t *testing.T, s *Store, ctx context.Context, ep, id string) Attempt {
	t.Helper()
	as, _, _ := attemptsOf(t, s, ctx, ep, id)
	if len(as) == 0 {
		t.Fatalf("todo %s has no attempts", id)
	}
	if as[0].EndedAt == nil {
		t.Fatalf("todo %s newest attempt %d is still open", id, as[0].Seq)
	}
	return as[0]
}

func exhaust(t *testing.T, s *Store, ctx context.Context, id string) {
	t.Helper()
	if _, err := s.pool.Exec(ctx, `UPDATE todos SET max_attempts = attempt WHERE id = $1`, id); err != nil {
		t.Fatalf("exhaust: %v", err)
	}
}

// REQ-1: every field of a claim's attempt is persisted, including the provenance fields no read
// returns; and the Board's claim is recorded as the owner's, with no claimer endpoint.
func TestAttemptRecordFields(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "attempt-record-fields")
	id := seedPending(t, s, ctx, ep, "q", "fields")

	hash := make([]byte, 32)
	hash[0] = 7
	td, ca, err := s.ClaimTodoWith(ctx, ep, id, "agent:a1", ClaimOpts{
		TTL: time.Hour, Claimant: "harness/box/fixer/run-7", Session: "sess-S", TokenHash: hash,
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if ca.Seq != 1 || ca.AttemptsTotal != 1 {
		t.Fatalf("claimed attempt = %+v, want seq 1 of 1", ca)
	}
	var (
		seq, attempt                            int
		kind, session, owner, claimant          string
		claimerEP                               *string
		claimedAt, leaseExp                     time.Time
		endedAt, lastHB                         *time.Time
		outcome, disposition, summary, artifact *string
		gotHash                                 []byte
	)
	if err := s.pool.QueryRow(ctx, `
		SELECT seq, attempt, claimer_kind, claimer_endpoint_id::text, claimer_session, owner, claimant,
			claimed_at, lease_expires_at, ended_at, last_heartbeat_at, outcome, disposition, summary,
			artifact, lease_token_hash
		FROM todo_attempts WHERE todo_id = $1`, id).Scan(&seq, &attempt, &kind, &claimerEP, &session,
		&owner, &claimant, &claimedAt, &leaseExp, &endedAt, &lastHB, &outcome, &disposition, &summary,
		&artifact, &gotHash); err != nil {
		t.Fatalf("read attempt row: %v", err)
	}
	if seq != 1 || attempt != 1 || kind != "endpoint" || claimerEP == nil || *claimerEP != ep ||
		session != "sess-S" || owner != "agent:a1" || claimant != "harness/box/fixer/run-7" {
		t.Fatalf("attempt provenance = seq %d attempt %d kind %s ep %v session %s owner %s claimant %s",
			seq, attempt, kind, claimerEP, session, owner, claimant)
	}
	if !leaseExp.Equal(*td.LeaseExpiresAt) {
		t.Fatalf("attempt lease %v != todo lease %v", leaseExp, *td.LeaseExpiresAt)
	}
	if endedAt != nil || lastHB != nil || outcome != nil || disposition != nil || summary != nil || artifact != nil {
		t.Fatal("a fresh attempt must be open with no heartbeat, outcome or report")
	}
	if len(gotHash) != 32 || gotHash[0] != 7 {
		t.Fatalf("lease token hash not persisted: %x", gotHash)
	}

	// The Board's claim: claimer_kind owner, no claimer endpoint.
	id2 := seedPending(t, s, ctx, ep, "q", "board")
	if _, err := s.ClaimTodoOperatorOwned(ctx, ownerOf(t, s, ctx, ep), id2, "human:h", time.Hour); err != nil {
		t.Fatalf("board claim: %v", err)
	}
	if err := s.pool.QueryRow(ctx,
		`SELECT claimer_kind, claimer_endpoint_id::text FROM todo_attempts WHERE todo_id = $1`, id2,
	).Scan(&kind, &claimerEP); err != nil {
		t.Fatalf("read board attempt: %v", err)
	}
	if kind != "owner" || claimerEP != nil {
		t.Fatalf("board attempt kind %s endpoint %v, want owner and none", kind, claimerEP)
	}
	assertAttemptInvariant(t, s, ctx)
}

// REQ-2: a lost claim race writes nothing — exactly one attempt for one committed claim.
func TestLostClaimRaceWritesOneAttempt(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "lost-claim-race")
	id := seedPending(t, s, ctx, ep, "q", "raced")

	const n = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins, conflicts := 0, 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.ClaimTodo(ctx, ep, id, "w", time.Hour)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case errors.Is(err, ErrConflict):
				conflicts++
			default:
				t.Errorf("claim: %v", err)
			}
		}()
	}
	wg.Wait()
	if wins != 1 || conflicts != n-1 {
		t.Fatalf("wins %d conflicts %d, want 1 and %d", wins, conflicts, n-1)
	}
	if got := count(t, s, ctx, "todo_attempts", "todo_id = '"+id+"'"); got != 1 {
		t.Fatalf("%d attempts after one committed claim, want 1", got)
	}
	assertAttemptInvariant(t, s, ctx)
}

// REQ-2: interrupt and resume neither open nor close an attempt.
func TestInterruptKeepsAttemptOpen(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "interrupt-keeps-attempt")
	id := seedPending(t, s, ctx, ep, "q", "interrupted")
	if _, err := s.ClaimTodo(ctx, ep, id, "w", time.Hour); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := s.InterruptTodo(ctx, id, "w", "input-required", nil); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	assertAttemptInvariant(t, s, ctx)
	if _, err := s.ResumeTodo(ctx, id, "w", nil); err != nil {
		t.Fatalf("resume: %v", err)
	}
	as, _, _ := attemptsOf(t, s, ctx, ep, id)
	if len(as) != 1 || as[0].EndedAt != nil {
		t.Fatalf("attempts after interrupt/resume = %+v, want one open", as)
	}
	assertAttemptInvariant(t, s, ctx)
}

// REQ-2: the one-open-attempt rule is a constraint checked at COMMIT. A planted second open attempt
// is accepted by the INSERT and refused by the COMMIT.
func TestSecondOpenAttemptFailsAtCommit(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "second-open-attempt")
	id := seedPending(t, s, ctx, ep, "q", "planted")
	if _, err := s.ClaimTodo(ctx, ep, id, "w", time.Hour); err != nil {
		t.Fatalf("claim: %v", err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		INSERT INTO todo_attempts (todo_id, seq, attempt, claimer_kind, owner, lease_expires_at)
		VALUES ($1, 2, 2, 'endpoint', 'w', now())`, id); err != nil {
		t.Fatalf("the INSERT itself must pass (the constraint is deferred): %v", err)
	}
	err = tx.Commit(ctx)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23P01" || pgErr.ConstraintName != "todo_attempts_one_open" {
		t.Fatalf("commit of a second open attempt = %v, want exclusion violation on todo_attempts_one_open", err)
	}
	if got := count(t, s, ctx, "todo_attempts", "todo_id = '"+id+"'"); got != 1 {
		t.Fatalf("%d attempts after the refused commit, want 1", got)
	}
}

// REQ-3: each transition that ends a lease closes the open attempt with the tabled outcome and
// disposition, and the retry scheduler and a manual retry touch no attempt.
func TestAttemptClosesOnEveryTerminalPath(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "attempt-closes")
	human := ownerOf(t, s, ctx, ep)

	claim := func(title string) string {
		t.Helper()
		id := seedPending(t, s, ctx, ep, "q", title)
		if _, err := s.ClaimTodo(ctx, ep, id, "w", time.Hour); err != nil {
			t.Fatalf("claim %s: %v", title, err)
		}
		return id
	}
	want := func(id, outcome, disposition string) {
		t.Helper()
		a := newestClosed(t, s, ctx, ep, id)
		if a.Outcome != outcome || a.Disposition != disposition {
			t.Fatalf("todo %s closed %s/%s, want %s/%s", id, a.Outcome, a.Disposition, outcome, disposition)
		}
		if a.Died != (outcome == "lease_expired" || outcome == "reaped") {
			t.Fatalf("todo %s died = %v for outcome %s", id, a.Died, outcome)
		}
	}

	// complete, with a report
	id := claim("complete")
	if _, err := s.CompleteTodoWith(ctx, ep, id, "w", Report{Summary: "all green", Artifact: "mcp://cairn/Ab12"}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	want(id, "completed", "done")
	if a := newestClosed(t, s, ctx, ep, id); a.Summary != "all green" || a.Artifact != "mcp://cairn/Ab12" {
		t.Fatalf("complete report not stored: %+v", a)
	}

	// fail below the cap, then the retry scheduler re-queues it without touching the attempt
	id = claim("fail-below")
	if _, err := s.FailTodoWith(ctx, ep, id, "w", Report{Summary: "tests red"}); err != nil {
		t.Fatalf("fail: %v", err)
	}
	want(id, "failed", "retry_scheduled")
	if _, err := s.pool.Exec(ctx, `UPDATE todos SET next_retry_at = now() - interval '1 second' WHERE id = $1`, id); err != nil {
		t.Fatalf("elapse backoff: %v", err)
	}
	before, _, _ := attemptsOf(t, s, ctx, ep, id)
	if _, err := s.RequeueDueRetries(ctx); err != nil {
		t.Fatalf("requeue: %v", err)
	}
	if after, _, _ := attemptsOf(t, s, ctx, ep, id); len(after) != len(before) || *after[0].EndedAt != *before[0].EndedAt {
		t.Fatal("the retry scheduler must not open or close an attempt")
	}

	// fail at the cap, then a manual retry keeps history and touches no attempt
	id = claim("fail-at-cap")
	exhaust(t, s, ctx, id)
	if _, err := s.FailTodo(ctx, ep, id, "w", nil); err != nil {
		t.Fatalf("fail at cap: %v", err)
	}
	want(id, "failed", "dead_lettered")
	if _, err := s.RetryTodo(ctx, ep, id); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if as, _, _ := attemptsOf(t, s, ctx, ep, id); len(as) != 1 || as[0].Outcome != "failed" {
		t.Fatalf("a manual retry must keep the closed attempt as it was: %+v", as)
	}

	// release by the holder, and by the Board
	id = claim("release")
	if _, err := s.ReleaseTodoWith(ctx, ep, id, "w", Report{Summary: "daemon stopping"}); err != nil {
		t.Fatalf("release: %v", err)
	}
	want(id, "released", "requeued")
	id = claim("board-release")
	if _, err := s.ReleaseTodoOperatorOwned(ctx, human, id, "w"); err != nil {
		t.Fatalf("board release: %v", err)
	}
	want(id, "released", "requeued")

	// the Board's complete and fail
	id = claim("board-complete")
	if _, err := s.CompleteTodoOperatorOwned(ctx, human, id, "w", nil); err != nil {
		t.Fatalf("board complete: %v", err)
	}
	want(id, "completed", "done")
	id = claim("board-fail")
	if _, err := s.FailTodoOperatorOwned(ctx, human, id, "w", nil); err != nil {
		t.Fatalf("board fail: %v", err)
	}
	want(id, "failed", "retry_scheduled")

	// takeover of a lapsed lease: the old attempt closes lease_expired and a new one opens
	id = claim("takeover")
	expireLease(t, s, ctx, id)
	_, ca, err := s.ClaimTodoWith(ctx, ep, id, "w2", ClaimOpts{TTL: time.Hour})
	if err != nil {
		t.Fatalf("takeover claim: %v", err)
	}
	if ca.Seq != 2 {
		t.Fatalf("takeover opened seq %d, want 2", ca.Seq)
	}
	as, _, _ := attemptsOf(t, s, ctx, ep, id)
	if len(as) != 2 || as[0].EndedAt != nil || as[1].Outcome != "lease_expired" || as[1].Disposition != "requeued" || !as[1].Died {
		t.Fatalf("after takeover attempts = %+v, want seq 2 open and seq 1 lease_expired", as)
	}
	// and via claim_next
	id = claim("takeover-next")
	expireLease(t, s, ctx, id)
	if _, err := s.pool.Exec(ctx, `UPDATE todos SET queue = 'q-next' WHERE id = $1`, id); err != nil {
		t.Fatalf("isolate queue: %v", err)
	}
	if _, err := s.ClaimNext(ctx, ep, []string{"q-next"}, "w2", time.Hour); err != nil {
		t.Fatalf("takeover claim_next: %v", err)
	}
	if as, _, _ := attemptsOf(t, s, ctx, ep, id); len(as) != 2 || as[1].Outcome != "lease_expired" {
		t.Fatalf("claim_next takeover attempts = %+v", as)
	}

	// the reaper, below and at the cap
	below, atCap := claim("reap-below"), claim("reap-at-cap")
	exhaust(t, s, ctx, atCap)
	expireLease(t, s, ctx, below)
	expireLease(t, s, ctx, atCap)
	if _, err := s.ReapExpired(ctx); err != nil {
		t.Fatalf("reap: %v", err)
	}
	want(below, "reaped", "requeued")
	want(atCap, "reaped", "dead_lettered")

	// A2A cancel of a claimed todo
	id = claim("cancel")
	if _, err := s.CancelTodo(ctx, id, nil); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	want(id, "canceled", "canceled")

	assertAttemptInvariant(t, s, ctx)

	// revocation of the owning endpoint closes every open attempt revoked/dead_lettered
	claimedID := claim("revoke-claimed")
	interrupted := claim("revoke-interrupted")
	if _, err := s.InterruptTodo(ctx, interrupted, "w", "auth-required", nil); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	if err := s.RevokeEndpoint(ctx, ep, human); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	// The endpoint is gone as a scope for agent reads, so read the rows directly.
	for _, rid := range []string{claimedID, interrupted} {
		var outcome, disposition string
		if err := s.pool.QueryRow(ctx, `SELECT outcome, disposition FROM todo_attempts
			WHERE todo_id = $1 ORDER BY seq DESC LIMIT 1`, rid).Scan(&outcome, &disposition); err != nil {
			t.Fatalf("read revoked attempt: %v", err)
		}
		if outcome != "revoked" || disposition != "dead_lettered" {
			t.Fatalf("revoked todo %s closed %s/%s", rid, outcome, disposition)
		}
	}
	assertAttemptInvariant(t, s, ctx)
}

// REQ-4 (heartbeat half): each heartbeat stamps the open attempt's last_heartbeat_at and moves its
// lease_expires_at with the todo's, on the agent and the Board paths alike.
func TestHeartbeatStampsOpenAttempt(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "heartbeat-stamps")
	id := seedPending(t, s, ctx, ep, "q", "hb")
	if _, err := s.ClaimTodo(ctx, ep, id, "w", time.Minute); err != nil {
		t.Fatalf("claim: %v", err)
	}
	td, err := s.HeartbeatTodo(ctx, ep, id, "w", time.Hour)
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	as, _, _ := attemptsOf(t, s, ctx, ep, id)
	if as[0].LastHeartbeatAt == nil || !as[0].LeaseExpiresAt.Equal(*td.LeaseExpiresAt) {
		t.Fatalf("heartbeat did not stamp the attempt: %+v (todo lease %v)", as[0], td.LeaseExpiresAt)
	}
	first := *as[0].LastHeartbeatAt
	td, err = s.HeartbeatTodoOperatorOwned(ctx, ownerOf(t, s, ctx, ep), id, "w", 2*time.Hour)
	if err != nil {
		t.Fatalf("board heartbeat: %v", err)
	}
	as, _, _ = attemptsOf(t, s, ctx, ep, id)
	if !as[0].LastHeartbeatAt.After(first) || !as[0].LeaseExpiresAt.Equal(*td.LeaseExpiresAt) {
		t.Fatalf("board heartbeat did not move the attempt: %+v", as[0])
	}
	assertAttemptInvariant(t, s, ctx)
}

// REQ-11 and REQ-12: sixty claims through manual retries keep the newest fifty attempts, count ten
// pruned and sixty total, and each claim after a retry takes attempt 1 with the next seq.
func TestAttemptCapAndManualRetryHistory(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "attempt-cap")
	id := seedPending(t, s, ctx, ep, "q", "hot")
	if _, err := s.pool.Exec(ctx, `UPDATE todos SET max_attempts = 1 WHERE id = $1`, id); err != nil {
		t.Fatalf("max_attempts: %v", err)
	}
	for i := 1; i <= 60; i++ {
		td, ca, err := s.ClaimTodoWith(ctx, ep, id, "w", ClaimOpts{TTL: time.Hour})
		if err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
		if ca.Seq != i || td.Attempt != 1 {
			t.Fatalf("claim %d opened seq %d attempt %d, want seq %d attempt 1", i, ca.Seq, td.Attempt, i)
		}
		if _, err := s.FailTodo(ctx, ep, id, "w", nil); err != nil {
			t.Fatalf("fail %d: %v", i, err)
		}
		if _, err := s.RetryTodo(ctx, ep, id); err != nil {
			t.Fatalf("retry %d: %v", i, err)
		}
	}
	as, total, pruned := attemptsOf(t, s, ctx, ep, id)
	if total != 60 || pruned != 10 {
		t.Fatalf("attempts_total %d pruned %d, want 60 and 10", total, pruned)
	}
	if n := count(t, s, ctx, "todo_attempts", "todo_id = '"+id+"'"); n != 50 {
		t.Fatalf("%d attempt rows, want 50", n)
	}
	if as[0].Seq != 60 || as[len(as)-1].Seq != 11 {
		t.Fatalf("kept seqs %d..%d, want 60..11", as[0].Seq, as[len(as)-1].Seq)
	}

	// The cap is a setting, floored at 5.
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO settings (key, value) VALUES ('attempt_history_max_per_todo', '2')`); err != nil {
		t.Fatalf("set cap: %v", err)
	}
	if _, err := s.ClaimTodo(ctx, ep, id, "w", time.Hour); err != nil {
		t.Fatalf("claim under a lowered cap: %v", err)
	}
	if n := count(t, s, ctx, "todo_attempts", "todo_id = '"+id+"'"); n != 5 {
		t.Fatalf("%d attempt rows under a cap set to 2, want the floor of 5", n)
	}
	if _, total, pruned = attemptsOf(t, s, ctx, ep, id); total != 61 || pruned != 56 {
		t.Fatalf("after lowering the cap: total %d pruned %d, want 61 and 56", total, pruned)
	}
	assertAttemptInvariant(t, s, ctx)
}

// REQ-11: retention deletes a terminal todo's attempts with it.
func TestRetentionCascadesAttempts(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "retention-cascade")
	done := seedTerminal(t, s, ctx, ep, "q", "old-done", "done")
	if n := count(t, s, ctx, "todo_attempts", "todo_id = '"+done+"'"); n != 1 {
		t.Fatalf("seeded done todo has %d attempts, want 1", n)
	}
	backdate(t, s, ctx, "todos", "updated_at", "id = '"+done+"'", 400*24*time.Hour)
	if _, err := s.Prune(ctx); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n := count(t, s, ctx, "todos", "id = '"+done+"'"); n != 0 {
		t.Fatal("retention did not prune the old done todo")
	}
	if n := count(t, s, ctx, "todo_attempts", "todo_id = '"+done+"'"); n != 0 {
		t.Fatalf("%d attempts outlived their pruned todo", n)
	}
}

// REQ-10: the store read is scoped to the todo's endpoint. A foreign todo and a never-minted id are
// the same ErrNotFound.
func TestTodoAttemptsScopedToEndpoint(t *testing.T) {
	s, ctx := testStore(t)
	a := seedEndpoint(t, s, ctx, "attempts-scope-a")
	b := seedEndpoint(t, s, ctx, "attempts-scope-b")
	id := seedTerminal(t, s, ctx, a, "q", "a's work", "done")

	if _, _, _, err := s.TodoAttempts(ctx, b, id, 20); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign read = %v, want ErrNotFound", err)
	}
	if _, _, _, err := s.TodoAttempts(ctx, b, "td_never_minted", 20); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id read = %v, want ErrNotFound", err)
	}
	if _, _, _, err := s.TodoAttempts(ctx, "", id, 20); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty scope read = %v, want ErrNotFound", err)
	}
	// A todo that was never claimed reads as no attempts, not as not-found.
	fresh := seedPending(t, s, ctx, a, "q", "fresh")
	if as, total, _, err := s.TodoAttempts(ctx, a, fresh, 20); err != nil || len(as) != 0 || total != 0 {
		t.Fatalf("unclaimed todo attempts = %v, %d, %v", as, total, err)
	}
}

// REQ-17: the migration's backfill opens one attempt for a lease in flight, and the holder's
// complete after the upgrade closes it completed.
func TestMigratedAttemptClosesCompleted(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "migrated-attempt")
	id := seedPending(t, s, ctx, ep, "q", "in flight across the upgrade")
	if _, err := s.ClaimTodo(ctx, ep, id, "w", time.Hour); err != nil {
		t.Fatalf("claim: %v", err)
	}
	// Rewind to the pre-migration shape: no attempt rows, no attempt counters.
	if _, err := s.pool.Exec(ctx, `DELETE FROM todo_attempts WHERE todo_id = $1`, id); err != nil {
		t.Fatalf("rewind attempts: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE todos SET attempts_total = 0 WHERE id = $1`, id); err != nil {
		t.Fatalf("rewind counters: %v", err)
	}
	if _, err := s.pool.Exec(ctx, migrationBackfill(t)); err != nil {
		t.Fatalf("run backfill: %v", err)
	}
	as, total, _ := attemptsOf(t, s, ctx, ep, id)
	if len(as) != 1 || total != 1 || as[0].Claimant != "migrated" || as[0].EndedAt != nil {
		t.Fatalf("backfilled attempts = %+v (total %d)", as, total)
	}
	if _, err := s.CompleteTodo(ctx, ep, id, "w", nil); err != nil {
		t.Fatalf("complete after upgrade: %v", err)
	}
	if a := newestClosed(t, s, ctx, ep, id); a.Outcome != "completed" || a.Seq != 1 {
		t.Fatalf("migrated attempt closed %+v, want seq 1 completed", a)
	}
	assertAttemptInvariant(t, s, ctx)
}

// migrationBackfill returns the backfill statements of the attempt-history migration, read from
// the migration file itself so the test exercises the SQL that ships.
func migrationBackfill(t *testing.T) string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("..", "db", "migrations", "*_todo_attempts.sql"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("locate the todo_attempts migration: %v %v", paths, err)
	}
	raw, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	const marker = "UPDATE todos SET attempts_total = 1"
	i := strings.Index(string(raw), marker)
	if i < 0 {
		t.Fatal("migration backfill marker not found")
	}
	return string(raw[i:])
}

// REQ-20 with REQ-18: concurrent claim_next workers each open exactly one attempt, and the table
// invariant holds after the race. Run under -race in CI.
func TestConcurrentClaimsOpenOneAttemptEach(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "concurrent-attempts")
	const n = 12
	for i := 0; i < n; i++ {
		seedPending(t, s, ctx, ep, "race", "t")
	}
	var wg sync.WaitGroup
	seqs := make(chan int, n)
	for w := 0; w < n; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, ca, err := s.ClaimNextWith(ctx, ep, []string{"race"}, "w", ClaimOpts{TTL: time.Minute})
			if err != nil {
				t.Errorf("claim_next: %v", err)
				return
			}
			seqs <- ca.Seq
		}()
	}
	wg.Wait()
	close(seqs)
	for seq := range seqs {
		if seq != 1 {
			t.Fatalf("a first claim opened seq %d", seq)
		}
	}
	if got := count(t, s, ctx, "todo_attempts", "ended_at IS NULL"); got != n {
		t.Fatalf("%d open attempts after %d claims", got, n)
	}
	assertAttemptInvariant(t, s, ctx)
}

func TestClipSummaryAndClaimant(t *testing.T) {
	long := strings.Repeat("é", 1500) // 3000 bytes of two-byte runes
	got, cut := ClipSummary(long)
	if !cut || len(got) != 2048 || strings.ContainsRune(got, '�') {
		t.Fatalf("ClipSummary: %d bytes, cut=%v", len(got), cut)
	}
	odd := "a" + strings.Repeat("é", 1500) // forces the cut back onto a rune boundary
	if got, _ := ClipSummary(odd); len(got) != 2047 {
		t.Fatalf("ClipSummary on an odd boundary kept %d bytes, want 2047", len(got))
	}
	if got, cut := ClipSummary("short"); got != "short" || cut {
		t.Fatalf("ClipSummary(short) = %q %v", got, cut)
	}
	if got := ClipClaimant("run\x00-7\n\t" + strings.Repeat("x", 200)); len(got) != 128 || strings.ContainsAny(got, "\x00\n\t") {
		t.Fatalf("ClipClaimant = %q (%d bytes)", got, len(got))
	}
}
