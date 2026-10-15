package store

// DB-backed tests for the queue-stall nudge: a queue whose pending work nobody claims re-rings its
// consumer, and one that has been drained or is still being drained does not. Each test drives the
// sweep directly, which is what the reaper tick calls; the ledger is aged with SQL because its
// backoff is measured in minutes and hours.
//
// Skipped without SWITCHBOARD_TEST_DATABASE_URL. Governing: ADR-0045 (proposed), SPEC-0024 REQ-6.

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// stallStore seeds an endpoint with a notify hook able to receive the wake — the receiver filter
// means a queue with no hook is not a candidate, so the base fixture needs one.
func stallStore(t *testing.T, s *Store, ctx context.Context, label string, queues ...string) string {
	t.Helper()
	ep := seedEndpoint(t, s, ctx, label, queues...)
	if _, err := s.CreateNotifyHook(ctx, ep, testHookURL, nil, false, "whsec_stall", 5); err != nil {
		t.Fatalf("seed hook (%s): %v", label, err)
	}
	return ep
}

// ageTodo backdates a todo's creation so its queue counts as stalled.
func ageTodo(t *testing.T, s *Store, ctx context.Context, id string, ago time.Duration) {
	t.Helper()
	if _, err := s.pool.Exec(ctx, `UPDATE todos SET created_at = now() - $2::interval WHERE id = $1`, id, ago); err != nil {
		t.Fatalf("age todo: %v", err)
	}
}

// setLedger writes the pacing row the way a previous nudge would have left it.
func setLedger(t *testing.T, s *Store, ctx context.Context, ep, queue string, nudges int, lastNudgedAgo time.Duration) {
	t.Helper()
	_, err := s.pool.Exec(ctx, `
		INSERT INTO queue_stall_nudges (endpoint_id, queue, nudges, last_nudged_at)
		VALUES ($1, $2, $3, now() - $4::interval)
		ON CONFLICT (endpoint_id, queue) DO UPDATE SET nudges = $3, last_nudged_at = now() - $4::interval`,
		ep, queue, nudges, lastNudgedAgo)
	if err != nil {
		t.Fatalf("seed ledger: %v", err)
	}
}

func ledgerRow(t *testing.T, s *Store, ctx context.Context, ep, queue string) (int, bool) {
	t.Helper()
	var nudges int
	err := s.pool.QueryRow(ctx,
		`SELECT nudges FROM queue_stall_nudges WHERE endpoint_id = $1 AND queue = $2`, ep, queue).Scan(&nudges)
	if err != nil {
		return 0, false
	}
	return nudges, true
}

// TestStalledQueueRingsWithNoNewWebhook is the ask in #566: work sits pending, nothing arrives
// upstream to fire an edge, and the queue still wakes its consumer. The second call proves the
// cooldown: one charge, not one per tick.
func TestStalledQueueRingsWithNoNewWebhook(t *testing.T) {
	s, ctx := hookStore(t)
	ep := stallStore(t, s, ctx, "stall-ring", "inbox")
	td := createEventTodo(t, s, ctx, ep, "sr-1", true, "signed")
	rec := &readyRecorder{}
	s.SetTodoReadyHook(rec.hook)
	defer s.SetTodoReadyHook(nil)

	ageTodo(t, s, ctx, td.ID, 10*time.Minute)

	got := s.mustNudge(t, ctx)
	if len(got) != 1 || got[0].ID != td.ID {
		t.Fatalf("nudge = %v, want the stalled todo", got)
	}
	if fires := rec.take(); len(fires) != 1 || fires[0] != td.ID+":stalled:0" {
		t.Fatalf("ready fires = %v, want one stalled wake naming the todo", fires)
	}

	// Inside the cooldown: charged already, so nothing to say.
	if again := s.mustNudge(t, ctx); len(again) != 0 {
		t.Fatalf("second sweep inside the cooldown woke the queue again: %v", again)
	}
	if n, ok := ledgerRow(t, s, ctx, ep, "inbox"); !ok || n != 1 {
		t.Fatalf("ledger = (%d,%v), want one charge", n, ok)
	}
}

// TestStalledQueueIsSilentWhileWorkIsFresh covers the race guard: work that just landed is not
// evidence that nobody is coming, and a sweep must not double-wake a consumer that is about to claim.
func TestStalledQueueIsSilentWhileWorkIsFresh(t *testing.T) {
	s, ctx := hookStore(t)
	ep := stallStore(t, s, ctx, "stall-fresh", "inbox")
	createEventTodo(t, s, ctx, ep, "sf-1", true, "signed")
	rec := &readyRecorder{}
	s.SetTodoReadyHook(rec.hook)
	defer s.SetTodoReadyHook(nil)
	rec.take()

	if got := s.mustNudge(t, ctx); len(got) != 0 {
		t.Fatalf("a freshly created todo counted as stalled: %v", got)
	}
	if _, ok := ledgerRow(t, s, ctx, ep, "inbox"); ok {
		t.Fatal("a queue that was never due burned a nudge")
	}
}

// TestDrainedQueueDoesNotRing is the other half of the ask: a queue that has been claimed and
// completed stays quiet, its pacing row goes away, and the next backlog is not punished for the last
// one's budget.
func TestDrainedQueueDoesNotRing(t *testing.T) {
	s, ctx := hookStore(t)
	ep := stallStore(t, s, ctx, "stall-drained", "inbox")
	td := createEventTodo(t, s, ctx, ep, "sd-1", true, "signed")
	rec := &readyRecorder{}
	s.SetTodoReadyHook(rec.hook)
	defer s.SetTodoReadyHook(nil)

	ageTodo(t, s, ctx, td.ID, 10*time.Minute)
	if got := s.mustNudge(t, ctx); len(got) != 1 {
		t.Fatalf("first sweep = %v, want one wake", got)
	}
	rec.take()

	if _, err := s.ClaimTodo(ctx, ep, td.ID, "w", time.Minute); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := s.CompleteTodo(ctx, ep, td.ID, "w", []byte(`{"ok":true}`)); err != nil {
		t.Fatalf("complete: %v", err)
	}

	if got := s.mustNudge(t, ctx); len(got) != 0 {
		t.Fatalf("a drained queue rang its consumer: %v", got)
	}
	if _, ok := ledgerRow(t, s, ctx, ep, "inbox"); ok {
		t.Fatal("a drained queue kept its pacing row, so the next backlog inherits its backoff")
	}

	// The next backlog starts at the short delay rather than the spent budget.
	next := createEventTodo(t, s, ctx, ep, "sd-2", true, "signed")
	rec.take()
	ageTodo(t, s, ctx, next.ID, 10*time.Minute)
	if got := s.mustNudge(t, ctx); len(got) != 1 || got[0].ID != next.ID {
		t.Fatalf("after a drain the next backlog was not nudged: %v", got)
	}
	if fires := rec.take(); len(fires) != 1 || fires[0] != next.ID+":stalled:0" {
		t.Fatalf("ready fires = %v, want one wake for the new todo", fires)
	}
}

// TestStalledQueueBacksOffAndStops paces the loop guard: the Nth nudge without progress waits the Nth
// backoff, and past the cap the queue goes quiet instead of paying a model turn forever.
func TestStalledQueueBacksOffAndStops(t *testing.T) {
	s, ctx := hookStore(t)
	ep := stallStore(t, s, ctx, "stall-backoff", "inbox")
	td := createEventTodo(t, s, ctx, ep, "sb-1", true, "signed")
	ageTodo(t, s, ctx, td.ID, 24*time.Hour)
	rec := &readyRecorder{}
	s.SetTodoReadyHook(rec.hook)
	defer s.SetTodoReadyHook(nil)
	rec.take()

	setLedger(t, s, ctx, ep, "inbox", 1, 4*time.Minute) // first nudge 4 minutes ago: still inside 5m
	if got := s.mustNudge(t, ctx); len(got) != 0 {
		t.Fatalf("nudged inside the first backoff: %v", got)
	}

	setLedger(t, s, ctx, ep, "inbox", 1, 6*time.Minute) // past it: due, and now on its second nudge
	if got := s.mustNudge(t, ctx); len(got) != 1 {
		t.Fatalf("a queue past its backoff was not nudged: %v", got)
	}
	if n, _ := ledgerRow(t, s, ctx, ep, "inbox"); n != 2 {
		t.Fatalf("ledger nudges = %d, want 2", n)
	}

	// At the cap, with no progress to reset it, the queue stops costing wakes.
	setLedger(t, s, ctx, ep, "inbox", queueStallMaxNudges, 30*24*time.Hour)
	if got := s.mustNudge(t, ctx); len(got) != 0 {
		t.Fatalf("a queue at the nudge cap was woken anyway: %v", got)
	}
}

// TestStalledQueueNudgeResetsOnProgress is what keeps a deep backlog drainable: a queue whose consumer
// is claiming one todo per invocation stays on the short cadence instead of sliding to the six-hour
// step, because a claim says the consumer is alive and the backlog is merely longer than one run.
func TestStalledQueueNudgeResetsOnProgress(t *testing.T) {
	s, ctx := hookStore(t)
	ep := stallStore(t, s, ctx, "stall-progress", "inbox")
	claimed := createEventTodo(t, s, ctx, ep, "sp-1", true, "signed")
	pending := createEventTodo(t, s, ctx, ep, "sp-2", true, "signed")
	ageTodo(t, s, ctx, claimed.ID, 3*time.Hour)
	ageTodo(t, s, ctx, pending.ID, 3*time.Hour)
	rec := &readyRecorder{}
	s.SetTodoReadyHook(rec.hook)
	defer s.SetTodoReadyHook(nil)
	rec.take()

	// Four nudges, last one an hour ago: on backoff alone this queue waits until the six-hour step.
	setLedger(t, s, ctx, ep, "inbox", 4, time.Hour)
	if got := s.mustNudge(t, ctx); len(got) != 0 {
		t.Fatalf("woken before its fourth backoff: %v", got)
	}

	// A claim since that nudge: the queue is making progress.
	if _, err := s.ClaimTodo(ctx, ep, claimed.ID, "w", time.Minute); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := s.CompleteTodo(ctx, ep, claimed.ID, "w", []byte(`{}`)); err != nil {
		t.Fatalf("complete: %v", err)
	}
	got := s.mustNudge(t, ctx)
	if len(got) != 1 || got[0].ID != pending.ID {
		t.Fatalf("a queue making progress was not woken for the work left: %v", got)
	}
	if n, _ := ledgerRow(t, s, ctx, ep, "inbox"); n != 1 {
		t.Fatalf("ledger nudges = %d, want the count reset to 1 by progress", n)
	}
	if fires := rec.take(); len(fires) != 1 || fires[0] != pending.ID+":stalled:0" {
		t.Fatalf("ready fires = %v, want one wake for the remaining todo", fires)
	}
}

// TestStalledQueueNeedsAReceiver: a queue whose endpoint has no enabled notify hook is not nudged,
// and — the lesson RingUnclaimed learned the hard way — is not charged either, so the consumer's
// return does not find a spent budget. Its work stays reachable by pull and by the session doorbell.
func TestStalledQueueNeedsAReceiver(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "stall-nohook", "inbox")
	td := createEventTodo(t, s, ctx, ep, "sn-1", true, "signed")
	rec := &readyRecorder{}
	s.SetTodoReadyHook(rec.hook)
	defer s.SetTodoReadyHook(nil)
	rec.take()

	ageTodo(t, s, ctx, td.ID, 10*time.Minute)
	if got := s.mustNudge(t, ctx); len(got) != 0 {
		t.Fatalf("a queue nobody can be told about was woken: %v", got)
	}
	if _, ok := ledgerRow(t, s, ctx, ep, "inbox"); ok {
		t.Fatal("a queue with no receiver burned a nudge")
	}
}

// TestStalledQueueSenderGate: an unverified delivery's queue ages quietly. The gate is the one every
// other wake path applies, and it lives in the query so this path cannot bypass it.
func TestStalledQueueSenderGate(t *testing.T) {
	s, ctx := hookStore(t)
	ep := stallStore(t, s, ctx, "stall-gate", "inbox")
	td := createEventTodo(t, s, ctx, ep, "sg-1", false, "signed")
	ageTodo(t, s, ctx, td.ID, 10*time.Minute)
	rec := &readyRecorder{}
	s.SetTodoReadyHook(rec.hook)
	defer s.SetTodoReadyHook(nil)
	rec.take()

	if got := s.mustNudge(t, ctx); len(got) != 0 {
		t.Fatalf("an unverified queue was nudged: %v", got)
	}
}

// TestStalledQueueNudgeNamesOneTodoPerQueue keeps the token-bomb guard: a deep backlog is one wake,
// naming its oldest gate-passing row, not one POST per pending row.
func TestStalledQueueNudgeNamesOneTodoPerQueue(t *testing.T) {
	s, ctx := hookStore(t)
	ep := stallStore(t, s, ctx, "stall-batch", "inbox")
	var ids []string
	for i := 0; i < 5; i++ {
		key := fmt.Sprintf("stb-%d", i)
		td := createEventTodo(t, s, ctx, ep, key, true, "signed")
		ageTodo(t, s, ctx, td.ID, 10*time.Minute)
		ids = append(ids, td.ID)
	}
	rec := &readyRecorder{}
	s.SetTodoReadyHook(rec.hook)
	defer s.SetTodoReadyHook(nil)
	rec.take()

	got := s.mustNudge(t, ctx)
	if len(got) != 1 {
		t.Fatalf("one queue produced %d wakes, want one", len(got))
	}
	if got[0].ID != ids[0] {
		t.Fatalf("wake named %s, want the oldest pending todo %s", got[0].ID, ids[0])
	}
}

// mustNudge fails the test rather than returning an error, so the assertions read as behaviour.
func (s *Store) mustNudge(t *testing.T, ctx context.Context) []Todo {
	t.Helper()
	out, err := s.NudgeStalledQueues(ctx)
	if err != nil {
		t.Fatalf("nudge stalled queues: %v", err)
	}
	return out
}
