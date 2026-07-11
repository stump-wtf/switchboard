package store

// Coverage for the operator Todos view read models (SPEC-0013 REQ "Todos View — Durable Queue", REQ
// "Todo Detail Drawer") and the supporting store additions: filter-pill counts, filtered/searched
// listing with the dedup count, single-todo detail, the operator Release transition, agent-name
// resolution, and the RecentEvents "deduped" flag.

import (
	"testing"
	"time"
)

func TestTodoCountsByState(t *testing.T) {
	s, ctx := testStore(t)

	mk := func(key string) Todo {
		td, _, err := s.CreateTodo(ctx, CreateTodoParams{Queue: "q", Source: "github", Kind: "push", Title: "t", IdempotencyKey: key})
		if err != nil {
			t.Fatalf("create %s: %v", key, err)
		}
		return td
	}
	pend := mk("c-pending")
	claimed := mk("c-claimed")
	done := mk("c-done")
	failed := mk("c-failed")
	_ = pend

	if _, err := s.ClaimTodo(ctx, claimed.ID, "op:h", time.Hour); err != nil {
		t.Fatalf("claim: %v", err)
	}
	dc, _ := s.ClaimTodo(ctx, done.ID, "op:h", time.Hour)
	if _, err := s.CompleteTodo(ctx, dc.ID, "op:h", []byte(`{}`)); err != nil {
		t.Fatalf("complete: %v", err)
	}
	// Drive `failed` to the terminal failed state by exhausting its single attempt.
	if _, err := s.pool.Exec(ctx, `UPDATE todos SET max_attempts=1 WHERE id=$1`, failed.ID); err != nil {
		t.Fatalf("cap attempts: %v", err)
	}
	fc, _ := s.ClaimTodo(ctx, failed.ID, "op:h", time.Hour)
	if _, err := s.FailTodo(ctx, fc.ID, "op:h", []byte(`{}`)); err != nil {
		t.Fatalf("fail: %v", err)
	}

	c, err := s.TodoCounts(ctx)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if c.All != 4 || c.Pending != 1 || c.Claimed != 1 || c.Done != 1 || c.Failed != 1 {
		t.Fatalf("counts = %+v, want All4 P1 C1 D1 F1", c)
	}
}

func TestListTodoItemsFilterAndSearch(t *testing.T) {
	s, ctx := testStore(t)

	if _, _, err := s.CreateTodo(ctx, CreateTodoParams{Queue: "q", Source: "github", Kind: "push", Title: "t", IdempotencyKey: "gh1"}); err != nil {
		t.Fatalf("create gh: %v", err)
	}
	if _, _, err := s.CreateTodo(ctx, CreateTodoParams{Queue: "q", Source: "stripe", Kind: "invoice.paid", Title: "t", IdempotencyKey: "st1"}); err != nil {
		t.Fatalf("create stripe: %v", err)
	}

	// No filter, no query: both.
	all, err := s.ListTodoItems(ctx, "", "", 50)
	if err != nil || len(all) != 2 {
		t.Fatalf("list all: n=%d err=%v", len(all), err)
	}
	// Search by source.
	hits, err := s.ListTodoItems(ctx, "", "stripe", 50)
	if err != nil || len(hits) != 1 || hits[0].Source != "stripe" {
		t.Fatalf("search stripe: %+v err=%v", hits, err)
	}
	// Search by event kind substring.
	byKind, _ := s.ListTodoItems(ctx, "", "invoice", 50)
	if len(byKind) != 1 || byKind[0].Kind != "invoice.paid" {
		t.Fatalf("search kind: %+v", byKind)
	}
	// Filter by state (claim one → it drops out of a pending listing).
	if _, err := s.ClaimTodo(ctx, hits[0].ID, "op:h", time.Hour); err != nil {
		t.Fatalf("claim: %v", err)
	}
	pending, _ := s.ListTodoItems(ctx, "pending", "", 50)
	if len(pending) != 1 || pending[0].Source != "github" {
		t.Fatalf("pending filter: %+v", pending)
	}
	claimed, _ := s.ListTodoItems(ctx, "claimed", "", 50)
	if len(claimed) != 1 || claimed[0].State != "claimed" {
		t.Fatalf("claimed filter: %+v", claimed)
	}
}

// A webhook-born todo carries its event's trust mode; a plain (event-less) todo reads as "queue".
func TestTodoItemTrustModeAndDedupCount(t *testing.T) {
	s, ctx := testStore(t)

	// Two DISTINCT deliveries (different sources) collapse onto the same idempotency key and queue:
	// the first creates the todo, the second inserts a new event but dedups the todo — so the todo's
	// dedup count is 2 and the second event is orphaned (RecentEvents flags it deduped).
	in1 := EventInput{Source: "github", Family: "webhook", EventType: "push", ExternalID: "shared-key", TrustMode: "signed", Verified: true, Payload: []byte(`{}`)}
	_, td, created, err := s.CreateEventTodo(ctx, in1, CreateTodoParams{Queue: "q", Source: "github", Kind: "push", Title: "t", IdempotencyKey: "shared-key"})
	if err != nil || !created {
		t.Fatalf("first delivery: created=%v err=%v", created, err)
	}
	in2 := EventInput{Source: "gitea", Family: "webhook", EventType: "push", ExternalID: "shared-key", TrustMode: "signed", Verified: true, Payload: []byte(`{}`)}
	_, _, created2, err := s.CreateEventTodo(ctx, in2, CreateTodoParams{Queue: "q", Source: "gitea", Kind: "push", Title: "t", IdempotencyKey: "shared-key"})
	if err != nil {
		t.Fatalf("second delivery: %v", err)
	}
	if created2 {
		t.Fatalf("second delivery should have deduped the todo, not created a new one")
	}

	it, err := s.GetTodoItem(ctx, td.ID)
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if it.TrustMode != "signed" {
		t.Fatalf("trust mode = %q, want signed", it.TrustMode)
	}
	if it.DedupCount != 2 {
		t.Fatalf("dedup count = %d, want 2 (two deliveries collapsed)", it.DedupCount)
	}

	// The second delivery's event has no todo of its own and must be flagged deduped in the feed.
	events, err := s.RecentEvents(ctx, 8)
	if err != nil {
		t.Fatalf("recent events: %v", err)
	}
	var deduped, linked int
	for _, e := range events {
		if e.Deduped {
			deduped++
		}
		if e.TodoID != "" {
			linked++
		}
	}
	if deduped != 1 || linked != 1 {
		t.Fatalf("feed: deduped=%d linked=%d, want 1 and 1 (%+v)", deduped, linked, events)
	}

	// An event-less todo reads as the queue trust mode.
	plain, _, _ := s.CreateTodo(ctx, CreateTodoParams{Queue: "q2", Source: "cron", Title: "t", IdempotencyKey: "plain"})
	pit, _ := s.GetTodoItem(ctx, plain.ID)
	if pit.TrustMode != "queue" {
		t.Fatalf("event-less trust mode = %q, want queue", pit.TrustMode)
	}
}

func TestReleaseTodoReturnsToPending(t *testing.T) {
	s, ctx := testStore(t)

	td, _, err := s.CreateTodo(ctx, CreateTodoParams{Queue: "q", Title: "t", IdempotencyKey: "rel"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	claimed, err := s.ClaimTodo(ctx, td.ID, "op:h", time.Hour)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed.Attempt != 1 {
		t.Fatalf("attempt after claim = %d, want 1", claimed.Attempt)
	}

	released, err := s.ReleaseTodo(ctx, td.ID, "op:h")
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if released.State != "pending" || released.Owner != "" || released.LeaseExpiresAt != nil {
		t.Fatalf("released todo = %+v, want pending/no-owner/no-lease", released)
	}
	// Release does not consume or reset attempts (unlike fail/retry).
	if released.Attempt != 1 {
		t.Fatalf("attempt after release = %d, want 1 (unchanged)", released.Attempt)
	}

	// Only the current owner may release; a non-owner (or non-claimed) is a conflict, absent is not-found.
	if _, err := s.ReleaseTodo(ctx, td.ID, "op:other"); err != ErrConflict {
		t.Fatalf("release by non-owner err = %v, want ErrConflict", err)
	}
	if _, err := s.ReleaseTodo(ctx, "td_missing", "op:h"); err != ErrNotFound {
		t.Fatalf("release missing err = %v, want ErrNotFound", err)
	}
}

func TestAgentNameByID(t *testing.T) {
	s, ctx := testStore(t)

	h, err := s.UpsertHuman(ctx, "sub", "Joe", "joe@example.com")
	if err != nil {
		t.Fatalf("human: %v", err)
	}
	ag, err := s.CreateAgent(ctx, h.ID, "reviewer-bot", "")
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	name, err := s.AgentNameByID(ctx, ag.ID)
	if err != nil || name != "reviewer-bot" {
		t.Fatalf("agent name = %q err=%v, want reviewer-bot", name, err)
	}
	if _, err := s.AgentNameByID(ctx, "00000000-0000-0000-0000-000000000000"); err != ErrNotFound {
		t.Fatalf("missing agent err = %v, want ErrNotFound", err)
	}
}
