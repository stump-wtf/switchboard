package store

// DB-backed tests for the per-queue pause (ADR-0047): a paused queue keeps its intake and loses
// its delivery, and the resume re-rings exactly what the pause held back. Each test drives the
// same entry points the server does — the claim verbs, the hooks, and the stall sweep — so the
// whole lifecycle is exercised, not just one predicate.
//
// Skipped without SWITCHBOARD_TEST_DATABASE_URL. Governing: ADR-0047 (proposed).

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestPauseHoldsClaimsAndKeepsIntake(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "pause-claim", "inbox", "other")
	var rang []string
	s.SetTodoDoorbellHook(func(t Todo) { rang = append(rang, t.ID) })
	defer s.SetTodoDoorbellHook(nil)

	if err := s.PauseQueue(ctx, "inbox", "", "consumer upgrade"); err != nil {
		t.Fatalf("pause: %v", err)
	}

	// Intake continues: the webhook delivery persists and the todo lands pending.
	held := createEventTodo(t, s, ctx, ep, "pp-1", true, "signed")
	got, err := s.GetTodo(ctx, ep, held.ID)
	if err != nil || got.State != "pending" {
		t.Fatalf("paused intake: state=%q err=%v, want pending", got.State, err)
	}
	// ...but nothing rings: the doorbell half of the create wake stays silent.
	if len(rang) != 0 {
		t.Fatalf("paused create rang the doorbell: %v", rang)
	}

	// claim_next hands out nothing from the paused queue...
	if _, err := s.ClaimNext(ctx, ep, []string{"inbox"}, "w", time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("claim_next while paused = %v, want ErrNotFound", err)
	}
	// ...and claim by id is refused too, with the row still pending.
	if _, err := s.ClaimTodo(ctx, ep, held.ID, "w", time.Minute); err == nil {
		t.Fatal("claim by id succeeded on a paused queue")
	}
	if got, err := s.GetTodo(ctx, ep, held.ID); err != nil || got.State != "pending" {
		t.Fatalf("refused claim must leave the row pending: state=%q err=%v", got.State, err)
	}

	// A queue that was not paused is unaffected.
	live := createEventTodoIn(t, s, ctx, ep, "other", "pp-2", true, "signed")
	taken, err := s.ClaimNext(ctx, ep, []string{"other"}, "w", time.Minute)
	if err != nil || taken.ID != live.ID {
		t.Fatalf("claim_next on the live queue = (%v, %v), want the other todo", taken.ID, err)
	}
	if _, err := s.CompleteTodo(ctx, ep, taken.ID, "w", nil); err != nil {
		t.Fatalf("complete: %v", err)
	}

	// Observability: the pause reads back through both helpers.
	if paused, err := s.QueuePaused(ctx, "inbox"); err != nil || !paused {
		t.Fatalf("QueuePaused = (%v, %v), want true", paused, err)
	}
	list, err := s.PausedQueues(ctx, []string{"inbox", "other"})
	if err != nil || len(list) != 1 || list[0] != "inbox" {
		t.Fatalf("PausedQueues = (%v, %v), want [inbox]", list, err)
	}
	pauses, err := s.ListQueuePauses(ctx)
	if err != nil || len(pauses) != 1 || pauses[0].Queue != "inbox" || pauses[0].Reason != "consumer upgrade" {
		t.Fatalf("ListQueuePauses = (%v, %v), want the inbox pause with its reason", pauses, err)
	}
}

func TestResumeRingsTheBacklogOnce(t *testing.T) {
	s, ctx := hookStore(t)
	ep := seedEndpoint(t, s, ctx, "pause-resume", "inbox")
	rec := &readyRecorder{}
	s.SetTodoReadyHook(rec.hook)
	defer s.SetTodoReadyHook(nil)

	if err := s.PauseQueue(ctx, "inbox", "", ""); err != nil {
		t.Fatalf("pause: %v", err)
	}
	held := createEventTodo(t, s, ctx, ep, "pr-1", true, "signed")
	if fires := rec.take(); len(fires) != 0 {
		t.Fatalf("paused create fired the ready hook: %v", fires)
	}

	resumed, err := s.ResumeQueue(ctx, "inbox")
	if err != nil || !resumed {
		t.Fatalf("resume = (%v, %v), want true", resumed, err)
	}
	if fires := rec.take(); len(fires) != 1 || fires[0] != held.ID+":resumed:0" {
		t.Fatalf("resume wakes = %v, want one resumed wake naming the backlog todo", fires)
	}

	// The backlog is claimable again.
	taken, err := s.ClaimNext(ctx, ep, []string{"inbox"}, "w", time.Minute)
	if err != nil || taken.ID != held.ID {
		t.Fatalf("claim after resume = (%v, %v), want the held todo", taken.ID, err)
	}

	// A queue that is not paused resumes quietly: no wake, nothing was held.
	resumed, err = s.ResumeQueue(ctx, "inbox")
	if err != nil || resumed {
		t.Fatalf("second resume = (%v, %v), want false", resumed, err)
	}
	if fires := rec.take(); len(fires) != 0 {
		t.Fatalf("resume of an unpaused queue fired wakes: %v", fires)
	}
}

func TestPauseSilencesTheStallSweepAndResumeRestoresIt(t *testing.T) {
	s, ctx := hookStore(t)
	ep := stallStore(t, s, ctx, "pause-stall", "inbox")
	rec := &readyRecorder{}
	s.SetTodoReadyHook(rec.hook)
	defer s.SetTodoReadyHook(nil)

	if err := s.PauseQueue(ctx, "inbox", "", ""); err != nil {
		t.Fatalf("pause: %v", err)
	}
	td := createEventTodo(t, s, ctx, ep, "ps-1", true, "signed")
	ageTodo(t, s, ctx, td.ID, 10*time.Minute)
	rec.take() // the create wake raced the pause; the sweep's silence is what this test pins

	if got := s.mustNudge(t, ctx); len(got) != 0 {
		t.Fatalf("paused queue was nudged: %v", got)
	}
	if n, ok := ledgerRow(t, s, ctx, ep, "inbox"); ok {
		t.Fatalf("paused queue spent nudge budget: %d", n)
	}

	if _, err := s.ResumeQueue(ctx, "inbox"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got := s.mustNudge(t, ctx); len(got) != 1 || got[0].ID != td.ID {
		t.Fatalf("sweep after resume = %v, want the backlog todo", got)
	}
}

func TestQueueStatsShowsThePause(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "pause-stats", "inbox")
	if _, _, err := s.CreateTodo(ctx, CreateTodoParams{EndpointID: ep, Queue: "inbox", Title: "t", Payload: []byte("{}"), IdempotencyKey: "qs-1"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	// A paused queue with no todos at all is still a known queue: the pause is its claim on the view.
	if err := s.PauseQueue(ctx, "quiet", "", "parked"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	stats, err := s.QueueStats(ctx)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	seen := map[string]QueueStat{}
	for _, q := range stats {
		seen[q.Queue] = q
	}
	if q, ok := seen["quiet"]; !ok || !q.Paused {
		t.Fatalf("paused empty queue missing from stats: %v", stats)
	}
	if q, ok := seen["inbox"]; !ok || q.Paused {
		t.Fatalf("live queue reads as paused: %v", q)
	}
}

func TestQuarantineQueueRefusesPause(t *testing.T) {
	s, ctx := testStore(t)
	if err := s.PauseQueue(ctx, QueueQuarantine, "", ""); err == nil {
		t.Fatal("pausing quarantine must be refused")
	}
}

// createEventTodoIn is createEventTodo with the queue named: the pause tests need one queue paused
// and one left live inside the same endpoint.
func createEventTodoIn(t *testing.T, s *Store, ctx context.Context, ep, queue, key string, verified bool, trust string) Todo {
	t.Helper()
	_, td, created, err := s.CreateEventTodo(ctx,
		EventInput{Source: "gitea", Family: "webhook", EventType: "issues", ExternalID: key,
			TrustMode: trust, Verified: verified, Payload: []byte("raw")},
		CreateTodoParams{EndpointID: ep, Queue: queue, Source: "gitea", Kind: "issue", Title: "t " + key,
			Payload: []byte(`{}`), IdempotencyKey: key})
	if err != nil || !created {
		t.Fatalf("create %s: created=%v err=%v", key, created, err)
	}
	return td
}
