package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// QueueStats is the scrape-time read behind the queue-liveness gauges. Every known queue reports a
// row with all four REQ-2 states — a queue that is only scoped on an endpoint included, as zeros —
// and the oldest-pending age reads the head of the pending line, or 0 when nothing is pending. The
// A2A states the table also admits are not counted under any of the four.
// Governing: SPEC-0023 REQ-2 "Queue liveness — the mandatory pair"; design.md "Zero-value series".
func TestQueueStatsZeroFillsEveryKnownQueue(t *testing.T) {
	s, ctx := testStore(t)
	forgeEP := seedEndpoint(t, s, ctx, "qs-forge", "forge", "mixed")
	seedEndpoint(t, s, ctx, "qs-scoped", "scoped-only")

	// forge: the incident shape — work waiting, nothing holding it.
	var forgeIDs []string
	for i := range 3 {
		td, _, err := s.CreateTodo(ctx, CreateTodoParams{EndpointID: forgeEP, Queue: "forge",
			Title: "waiting", IdempotencyKey: fmt.Sprintf("qs-forge-%d", i)})
		if err != nil {
			t.Fatalf("create forge todo: %v", err)
		}
		forgeIDs = append(forgeIDs, td.ID)
	}
	// Backdate the head of the line so the age is a reading, not a scheduling race.
	if _, err := s.pool.Exec(ctx,
		`UPDATE todos SET created_at = now() - interval '2 hours' WHERE id = $1`, forgeIDs[0]); err != nil {
		t.Fatalf("backdate forge todo: %v", err)
	}

	// mixed: one todo per REQ-2 state plus one in an A2A state that must not be counted.
	for _, state := range []string{"pending", "claimed", "done", "failed", "canceled"} {
		td, _, err := s.CreateTodo(ctx, CreateTodoParams{EndpointID: forgeEP, Queue: "mixed",
			Title: state, IdempotencyKey: "qs-mixed-" + state})
		if err != nil {
			t.Fatalf("create mixed todo: %v", err)
		}
		if _, err := s.pool.Exec(ctx, `UPDATE todos SET state = $2 WHERE id = $1`, td.ID, state); err != nil {
			t.Fatalf("set mixed todo state %s: %v", state, err)
		}
	}

	stats, err := s.QueueStats(ctx)
	if err != nil {
		t.Fatalf("queue stats: %v", err)
	}
	byQueue := make(map[string]QueueStat, len(stats))
	for i, st := range stats {
		if _, dup := byQueue[st.Queue]; dup {
			t.Fatalf("queue %q reported twice: %+v", st.Queue, stats)
		}
		if i > 0 && stats[i-1].Queue >= st.Queue {
			t.Errorf("stats not sorted by queue: %+v", stats)
		}
		byQueue[st.Queue] = st
	}

	// The known-queue set is exactly KnownQueues' — not whatever the count returned.
	known, err := s.KnownQueues(ctx)
	if err != nil {
		t.Fatalf("known queues: %v", err)
	}
	if len(known) != len(stats) {
		t.Fatalf("stats cover %d queues, KnownQueues %d: %+v vs %v", len(stats), len(known), stats, known)
	}
	for _, q := range known {
		if _, ok := byQueue[q]; !ok {
			t.Errorf("known queue %q missing from stats", q)
		}
	}

	forge := byQueue["forge"]
	if forge.Pending != 3 || forge.Claimed != 0 || forge.Done != 0 || forge.Failed != 0 {
		t.Errorf("forge = %+v, want pending=3 claimed=0 done=0 failed=0", forge)
	}
	if forge.OldestPendingSeconds < 7200 || forge.OldestPendingSeconds > 7200+300 {
		t.Errorf("forge oldest pending = %.1fs, want ~7200s (the backdated head)", forge.OldestPendingSeconds)
	}

	mixed := byQueue["mixed"]
	if mixed.Pending != 1 || mixed.Claimed != 1 || mixed.Done != 1 || mixed.Failed != 1 {
		t.Errorf("mixed = %+v, want exactly one per REQ-2 state (canceled not counted)", mixed)
	}
	if mixed.OldestPendingSeconds < 0 {
		t.Errorf("mixed oldest pending = %v, want >= 0", mixed.OldestPendingSeconds)
	}

	scoped, ok := byQueue["scoped-only"]
	if !ok {
		t.Fatalf("scope-only queue missing: a queue no todo has ridden must still report (%+v)", stats)
	}
	if scoped != (QueueStat{Queue: "scoped-only"}) {
		t.Errorf("scoped-only = %+v, want every state and the oldest age at 0", scoped)
	}
}

// With nothing pending the oldest-pending age is 0, not NULL and not the age of some terminal row.
// Governing: SPEC-0023 REQ-2 ("or 0 when none is pending").
func TestQueueStatsOldestPendingZeroWhenNothingPending(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "qs-drained", "drained")
	td, _, err := s.CreateTodo(ctx, CreateTodoParams{EndpointID: ep, Queue: "drained", Title: "t", IdempotencyKey: "qs-d"})
	if err != nil {
		t.Fatalf("create todo: %v", err)
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE todos SET state = 'done', created_at = now() - interval '1 day' WHERE id = $1`, td.ID); err != nil {
		t.Fatalf("finish todo: %v", err)
	}
	stats, err := s.QueueStats(ctx)
	if err != nil {
		t.Fatalf("queue stats: %v", err)
	}
	if len(stats) != 1 || stats[0] != (QueueStat{Queue: "drained", Done: 1}) {
		t.Fatalf("stats = %+v, want [{drained done=1 oldest=0}]", stats)
	}
}

// QueueStats honours its context, so the collector's scrape deadline actually bounds the read and
// a timeout surfaces as an error — which the collector turns into omission (REQ-6), never zeros.
// Governing: SPEC-0023 REQ-6 "Honest absence"; design.md "Shape" (timeout shorter than the scrape).
func TestQueueStatsHonoursContext(t *testing.T) {
	s, ctx := testStore(t)
	seedEndpoint(t, s, ctx, "qs-ctx", "anything")
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	stats, err := s.QueueStats(cctx)
	if err == nil {
		t.Fatalf("QueueStats on a cancelled context = %+v, nil error; want an error", stats)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled", err)
	}
	if stats != nil {
		t.Errorf("stats = %+v on error, want nil", stats)
	}
}
