package store

// Board live-feed read models and hooks (SPEC-0013 REQ "Board View — Live Incoming Lines", REQ
// "Live Updates and Toasts"): the event hook fires only for NEW deliveries, RecentEvents joins
// each event to its todo's lifecycle state, EventBuckets shapes the throughput bars, and the
// reaper's re-surfaces reach the transition hook.

import (
	"testing"
	"time"
)

func TestEventHookFiresOnNewEventsOnly(t *testing.T) {
	s, ctx := testStore(t)

	var got []EventSummary
	s.SetEventHook(func(e EventSummary) { got = append(got, e) })
	defer s.SetEventHook(nil)

	in := EventInput{Source: "github", Family: "webhook", EventType: "push", ExternalID: "d1", TrustMode: "signed", Verified: true, Payload: []byte(`{}`)}
	evID, td, created, err := s.CreateEventTodo(ctx, in, CreateTodoParams{Queue: "q", Title: "t", IdempotencyKey: "k1"})
	if err != nil || !created {
		t.Fatalf("create event todo: %v created=%v", err, created)
	}
	if td.EventID == nil || *td.EventID != evID {
		t.Fatalf("todo must link its event: %+v", td.EventID)
	}
	if len(got) != 1 || got[0].ID != evID || got[0].Source != "github" || got[0].EventType != "push" || got[0].TrustMode != "signed" {
		t.Fatalf("event hook fired %d times / wrong payload: %+v", len(got), got)
	}
	if got[0].ReceivedAt.IsZero() {
		t.Fatal("event hook payload missing received_at")
	}

	// Duplicate delivery (same source+external id): event dedups → hook must NOT fire again.
	if _, _, _, err := s.CreateEventTodo(ctx, in, CreateTodoParams{Queue: "q", Title: "t", IdempotencyKey: "k1"}); err != nil {
		t.Fatalf("duplicate delivery: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("hook fired on duplicate delivery: %d", len(got))
	}

	// Standalone InsertEvent also fires for new rows.
	if _, err := s.InsertEvent(ctx, EventInput{Source: "stripe", Family: "webhook", EventType: "invoice.paid", ExternalID: "d2", TrustMode: "token", Payload: []byte(`{}`)}); err != nil {
		t.Fatalf("insert event: %v", err)
	}
	if len(got) != 2 || got[1].Source != "stripe" {
		t.Fatalf("hook after InsertEvent: %+v", got)
	}
}

func TestRecentEventsJoinTodoLifecycle(t *testing.T) {
	s, ctx := testStore(t)

	in := EventInput{Source: "github", Family: "webhook", EventType: "push", ExternalID: "r1", TrustMode: "signed", Verified: true, Payload: []byte(`{}`)}
	_, td, _, err := s.CreateEventTodo(ctx, in, CreateTodoParams{Queue: "q", Title: "t", IdempotencyKey: "rk1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.ClaimTodo(ctx, td.ID, "op:h1", time.Hour); err != nil {
		t.Fatalf("claim: %v", err)
	}

	events, err := s.RecentEvents(ctx, 8)
	if err != nil {
		t.Fatalf("recent events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	e := events[0]
	if e.TodoID != td.ID || e.TodoState != "claimed" || e.TodoOwner != "op:h1" {
		t.Fatalf("todo join wrong: %+v", e)
	}

	byID, err := s.EventByID(ctx, e.ID)
	if err != nil || byID.Source != "github" || byID.EventType != "push" {
		t.Fatalf("event by id: %+v %v", byID, err)
	}
	if _, err := s.EventByID(ctx, 999999); err != ErrNotFound {
		t.Fatalf("missing event err = %v, want ErrNotFound", err)
	}
}

func TestEventBuckets(t *testing.T) {
	s, ctx := testStore(t)

	if _, err := s.InsertEvent(ctx, EventInput{Source: "github", Family: "webhook", EventType: "push", ExternalID: "b1", TrustMode: "signed", Payload: []byte(`{}`)}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	buckets, err := s.EventBuckets(ctx, 24)
	if err != nil {
		t.Fatalf("buckets: %v", err)
	}
	if len(buckets) != 24 {
		t.Fatalf("bucket count = %d, want 24", len(buckets))
	}
	if buckets[23] < 1 {
		t.Fatalf("current-minute bucket = %d, want ≥1", buckets[23])
	}
	total := 0
	for _, c := range buckets[:23] {
		total += c
	}
	if total != 0 {
		t.Fatalf("older buckets should be empty, got %v", buckets)
	}
}

// Governing: SPEC-0013 "Reaper re-surface is visible" — reaped rows fire the transition hook with
// their committed outcome so the UI can flash the row and raise a toast.
func TestReapExpiredFiresTransitionHook(t *testing.T) {
	s, ctx := testStore(t)

	td, _, err := s.CreateTodo(ctx, CreateTodoParams{Queue: "q", Title: "t", IdempotencyKey: "reap-hook"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.ClaimTodo(ctx, td.ID, "agent:a1", time.Hour); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE todos SET lease_expires_at = now() - interval '1 minute' WHERE id=$1`, td.ID); err != nil {
		t.Fatalf("expire: %v", err)
	}

	var verbs []string
	s.SetTodoTransitionHook(func(verb string, _ Todo) { verbs = append(verbs, verb) })
	defer s.SetTodoTransitionHook(nil)

	n, err := s.ReapExpired(ctx)
	if err != nil || n != 1 {
		t.Fatalf("reap: n=%d err=%v", n, err)
	}
	if len(verbs) != 1 || verbs[0] != "pending" {
		t.Fatalf("reap hook verbs = %v, want [pending]", verbs)
	}
}
