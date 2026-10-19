// Interrupt-lease reaping: a todo paused on the client whose holder died must come back to the queue
// instead of parking forever.
//
// Governing: ADR-0046 (proposed), SPEC-0018 REQ "Task State Machine Extension" (scenario "Interrupt
// states return to claimed"), ADR-0002 (reaper crash safety), SPEC-0023 REQ-3 (lifecycle counters).
//
// @joestump-agent 10/19/2026 - Added for stump.wtf/switchboard#594.

package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// strandInterrupt parks a claimed todo in an interrupt state and then rewinds its wait start, which is
// what "the holder died a while ago" looks like from inside the database.
func strandInterrupt(t *testing.T, s *Store, ctx context.Context, id, state string, ago time.Duration) {
	t.Helper()
	if _, err := s.InterruptTodo(ctx, id, "w", state, []byte(`{"need":"an answer"}`)); err != nil {
		t.Fatalf("interrupt(%s): %v", state, err)
	}
	// Rewind created_at with it: the wait started after the row was enqueued, and todos' CHECK says
	// so. This is what "that happened yesterday" looks like from inside the database.
	if _, err := s.pool.Exec(ctx, `UPDATE todos
		SET created_at = now() - $2::interval, interrupted_at = now() - $2::interval WHERE id=$1`,
		id, ago.String()); err != nil {
		t.Fatalf("age interrupt: %v", err)
	}
}

func readState(t *testing.T, s *Store, ctx context.Context, id string) (state, owner string, interrupted bool) {
	t.Helper()
	var ownerPtr, interruptedAt *string
	if err := s.pool.QueryRow(ctx,
		`SELECT state, owner, interrupted_at::text FROM todos WHERE id=$1`, id).
		Scan(&state, &ownerPtr, &interruptedAt); err != nil {
		t.Fatalf("read todo: %v", err)
	}
	if ownerPtr != nil {
		owner = *ownerPtr
	}
	return state, owner, interruptedAt != nil
}

// TestStrandedInterruptReturnsToQueuePastTTL is the regression test for #594: a worker claims a todo,
// interrupts it, and dies. Before the interrupt TTL existed the row was invisible to the reaper (its
// predicate needs state='claimed') and unclaimable (the claim predicate covers pending, lapsed claimed
// and due failed), so it parked until someone called resume — forever. On the parent commit this test
// fails: ReapExpired reports 0 and the row is still sitting in input-required with a dead owner.
func TestStrandedInterruptReturnsToQueuePastTTL(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "a2a-strand", "q")

	for _, state := range []string{StateInputRequired, StateAuthRequired} {
		td, _, err := s.CreateTodo(ctx, CreateTodoParams{
			EndpointID: ep, Queue: "q", Title: "t", IdempotencyKey: "strand-" + state})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := s.ClaimTodo(ctx, ep, td.ID, "w", time.Hour); err != nil {
			t.Fatalf("claim: %v", err)
		}
		strandInterrupt(t, s, ctx, td.ID, state, interruptLeaseTTLDefault+time.Hour)

		n, err := s.ReapExpired(ctx)
		if err != nil {
			t.Fatalf("reap(%s): %v", state, err)
		}
		if n != 1 {
			t.Fatalf("reap(%s): reaped %d, want 1 — a stranded interrupt is invisible to crash safety", state, n)
		}
		got, owner, interruptedAt := readState(t, s, ctx, td.ID)
		if got != "pending" {
			t.Fatalf("reap(%s): state=%s want pending", state, got)
		}
		if owner != "" {
			t.Fatalf("reap(%s): owner=%q want cleared — nobody may hold a row whose holder is gone", state, owner)
		}
		if interruptedAt {
			t.Fatalf("reap(%s): interrupted_at still set; the wait is over", state)
		}
		// The point of requeueing rather than failing: somebody else can pick the work up.
		claimed, err := s.ClaimTodo(ctx, ep, td.ID, "w2", time.Hour)
		if err != nil {
			t.Fatalf("reclaim(%s): %v — a reclaimed interrupt must be claimable", state, err)
		}
		if claimed.State != "claimed" {
			t.Fatalf("reclaim(%s): state=%s want claimed", state, claimed.State)
		}
	}
}

// TestInterruptInsideTTLIsUntouched pins the other half of SPEC-0018's scenario: a wait that is still
// within its deadline keeps its owner and lease, and the same holder can still resume it.
func TestInterruptInsideTTLIsUntouched(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "a2a-waiting", "q")
	td, _, err := s.CreateTodo(ctx, CreateTodoParams{EndpointID: ep, Queue: "q", Title: "t", IdempotencyKey: "wait-1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	claimed, err := s.ClaimTodo(ctx, ep, td.ID, "w", time.Hour)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	it, err := s.InterruptTodo(ctx, td.ID, "w", StateInputRequired, []byte(`{"need":"an answer"}`))
	if err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	if it.LeaseExpiresAt == nil || !it.LeaseExpiresAt.Equal(*claimed.LeaseExpiresAt) {
		t.Fatalf("interrupt: lease=%v want the claim's %v", it.LeaseExpiresAt, claimed.LeaseExpiresAt)
	}

	if n, err := s.ReapExpired(ctx); err != nil || n != 0 {
		t.Fatalf("reap inside the TTL: n=%d err=%v; want 0 — a wait nobody has given up on is not a death", n, err)
	}
	resumed, err := s.ResumeTodo(ctx, td.ID, "w", []byte(`{"got":"the answer"}`))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resumed.State != "claimed" || resumed.Owner != "w" {
		t.Fatalf("resume: state=%s owner=%q want claimed/w", resumed.State, resumed.Owner)
	}
	if _, _, interruptedAt := readState(t, s, ctx, td.ID); interruptedAt {
		t.Fatal("resume must clear interrupted_at, or the sweep would reclaim a row somebody came back for")
	}
}

// TestStrandedInterruptDeadLettersAtCap: the attempt cap applies to an interrupt reclaim exactly as it
// does to a lapsed lease, and the dead-letter says why it died, the way the 0017 backfill's rows do.
func TestStrandedInterruptDeadLettersAtCap(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "a2a-strand-cap", "q")
	td, _, err := s.CreateTodo(ctx, CreateTodoParams{EndpointID: ep, Queue: "q", Title: "t", IdempotencyKey: "strand-cap"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.ClaimTodo(ctx, ep, td.ID, "w", time.Hour); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE todos SET attempt = max_attempts WHERE id=$1`, td.ID); err != nil {
		t.Fatalf("spend attempts: %v", err)
	}
	strandInterrupt(t, s, ctx, td.ID, StateAuthRequired, interruptLeaseTTLDefault+time.Hour)

	if n, err := s.ReapExpired(ctx); err != nil || n != 1 {
		t.Fatalf("reap: n=%d err=%v want 1", n, err)
	}
	var state, result string
	if err := s.pool.QueryRow(ctx, `SELECT state, result::text FROM todos WHERE id=$1`, td.ID).
		Scan(&state, &result); err != nil {
		t.Fatalf("read: %v", err)
	}
	if state != "failed" {
		t.Fatalf("state=%s want failed at the attempt cap", state)
	}
	if !strings.Contains(result, "interrupt_expired") {
		t.Fatalf("result=%s want it to name interrupt_expired", result)
	}
}

// TestInterruptTTLSettingCanDisableTheSweep: SPEC-0018 lists "leaves it interrupted indefinitely" as an
// acceptable answer, so it must be reachable without a code change.
func TestInterruptTTLSettingCanDisableTheSweep(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "a2a-strand-off", "q")
	// settings is instance-global, so the row is removed on the way in and on the way out: leaving it
	// would silently switch the sweep off for every later test in this package.
	if _, err := s.pool.Exec(ctx, `DELETE FROM settings WHERE key=$1`, settingInterruptLeaseTTLSeconds); err != nil {
		t.Fatalf("clear setting: %v", err)
	}
	t.Cleanup(func() {
		if _, err := s.pool.Exec(context.Background(), `DELETE FROM settings WHERE key=$1`,
			settingInterruptLeaseTTLSeconds); err != nil {
			t.Errorf("cleanup setting: %v", err)
		}
	})
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO settings (key, value) VALUES ($1, '0')`, settingInterruptLeaseTTLSeconds); err != nil {
		t.Fatalf("setting: %v", err)
	}
	td, _, err := s.CreateTodo(ctx, CreateTodoParams{EndpointID: ep, Queue: "q", Title: "t", IdempotencyKey: "strand-off"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.ClaimTodo(ctx, ep, td.ID, "w", time.Hour); err != nil {
		t.Fatalf("claim: %v", err)
	}
	strandInterrupt(t, s, ctx, td.ID, StateInputRequired, 365*24*time.Hour)

	if n, err := s.ReapExpired(ctx); err != nil || n != 0 {
		t.Fatalf("reap with the TTL off: n=%d err=%v want 0", n, err)
	}
	if state, _, _ := readState(t, s, ctx, td.ID); state != StateInputRequired {
		t.Fatalf("state=%s want it left interrupted, as the operator asked", state)
	}
}

// TestInterruptReclaimCountsLeaseAndCause: every interrupt reclaim is also a lease expiry, because the
// attempt it closes is a `reaped` close and SPEC-0034 REQ-14 reconciles those two families; the cause
// counter is what says this expiry was a wait that outlived its holder.
func TestInterruptReclaimCountsLeaseAndCause(t *testing.T) {
	s, ctx := testStore(t)
	m := newRecMetrics()
	s.SetMetrics(m)
	defer s.SetMetrics(nil)
	ep := seedEndpoint(t, s, ctx, "a2a-strand-count", "q")
	td, _, err := s.CreateTodo(ctx, CreateTodoParams{EndpointID: ep, Queue: "q", Title: "t", IdempotencyKey: "strand-count"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.ClaimTodo(ctx, ep, td.ID, "w", time.Hour); err != nil {
		t.Fatalf("claim: %v", err)
	}
	strandInterrupt(t, s, ctx, td.ID, StateInputRequired, interruptLeaseTTLDefault+time.Hour)

	if n, err := s.ReapExpired(ctx); err != nil || n != 1 {
		t.Fatalf("reap: n=%d err=%v want 1", n, err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.expired["q"] != 1 {
		t.Fatalf("leases expired=%v want one q increment", m.expired)
	}
	if m.interrupted["q"] != 1 {
		t.Fatalf("interrupt reclaims=%v want one q increment", m.interrupted)
	}
	if m.closed["q/reaped"] != 1 {
		t.Fatalf("attempt closes=%v want one q/reaped", m.closed)
	}
}
