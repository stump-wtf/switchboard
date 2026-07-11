package store

// Store-level coverage for the Board/shell read models against real SQL: tile counts, the
// recent-events slice, and pool health. Skips without SWITCHBOARD_TEST_DATABASE_URL like every
// other store test; Gitea CI provides the database.
// Governing: SPEC-0013 REQ "Information Architecture and Navigation", REQ "Board View — Live
// Incoming Lines".

import (
	"testing"
	"time"
)

func TestPingHealthy(t *testing.T) {
	s, ctx := testStore(t)
	if err := s.Ping(ctx); err != nil {
		t.Fatalf("ping on a healthy pool: %v", err)
	}
}

func TestBoardStats(t *testing.T) {
	s, ctx := testStore(t)

	// Empty database: every tile is zero, and VerifiedPct's divide-by-zero guard holds.
	b, err := s.BoardStats(ctx)
	if err != nil {
		t.Fatalf("board stats (empty): %v", err)
	}
	if b != (BoardStats{}) {
		t.Fatalf("empty db stats = %+v, want all zeros", b)
	}

	// Seed: three todos created today (one claimed), three events today (two signed, one open).
	for i, key := range []string{"e1", "e2", "e3"} {
		mode, verified := "signed", true
		if i == 2 {
			mode, verified = "open", false
		}
		if _, err := s.InsertEvent(ctx, EventInput{
			Source: "github", Family: "webhook", EventType: "push",
			ExternalID: key, TrustMode: mode, Verified: verified,
		}); err != nil {
			t.Fatalf("seed event %s: %v", key, err)
		}
	}
	var claimID string
	for i := 0; i < 3; i++ {
		td, created, err := s.CreateTodo(ctx, CreateTodoParams{Queue: "reviews", Title: "todo"})
		if err != nil || !created {
			t.Fatalf("seed todo %d: created=%v err=%v", i, created, err)
		}
		claimID = td.ID
	}
	if _, err := s.ClaimTodo(ctx, claimID, "worker-1", time.Minute); err != nil {
		t.Fatalf("claim seed todo: %v", err)
	}

	b, err = s.BoardStats(ctx)
	if err != nil {
		t.Fatalf("board stats (seeded): %v", err)
	}
	want := BoardStats{
		TodosToday:    3,
		InFlight:      1,
		AwaitingClaim: 2,
		VerifiedPct:   67, // round(100 * 2/3)
		EventsPerMin:  3,
	}
	if b != want {
		t.Fatalf("seeded stats = %+v, want %+v", b, want)
	}
}

func TestRecentEvents(t *testing.T) {
	s, ctx := testStore(t)

	// Empty feed is empty, not an error.
	evs, err := s.RecentEvents(ctx, 12)
	if err != nil {
		t.Fatalf("recent events (empty): %v", err)
	}
	if len(evs) != 0 {
		t.Fatalf("empty db returned %d events", len(evs))
	}

	seed := []EventInput{
		{Source: "github", Family: "webhook", EventType: "push", ExternalID: "g1", TrustMode: "signed", Verified: true},
		{Source: "stripe", Family: "webhook", EventType: "invoice.paid", ExternalID: "s1", TrustMode: "token", Verified: true},
		{Source: "curl", Family: "webhook", ExternalID: "c1", TrustMode: "open"}, // no event type → ""
	}
	ids := make([]int64, 0, len(seed))
	for _, e := range seed {
		id, err := s.InsertEvent(ctx, e)
		if err != nil {
			t.Fatalf("seed %s: %v", e.Source, err)
		}
		ids = append(ids, id)
	}

	// Newest first, limit respected.
	evs, err = s.RecentEvents(ctx, 2)
	if err != nil {
		t.Fatalf("recent events: %v", err)
	}
	if len(evs) != 2 {
		t.Fatalf("limit 2 returned %d events", len(evs))
	}
	if evs[0].ID != ids[2] || evs[1].ID != ids[1] {
		t.Fatalf("order wrong: got ids %d,%d want %d,%d", evs[0].ID, evs[1].ID, ids[2], ids[1])
	}
	if evs[0].Source != "curl" || evs[0].TrustMode != "open" {
		t.Fatalf("newest event fields wrong: %+v", evs[0])
	}
	if evs[0].EventType != "" {
		t.Fatalf("NULL event_type should surface as empty string, got %q", evs[0].EventType)
	}
	if evs[0].ReceivedAt.IsZero() {
		t.Fatal("ReceivedAt not populated")
	}

	// A duplicate delivery (same source + external id) does not add a feed row.
	if _, err := s.InsertEvent(ctx, seed[0]); err != nil {
		t.Fatalf("duplicate insert: %v", err)
	}
	evs, err = s.RecentEvents(ctx, 12)
	if err != nil {
		t.Fatalf("recent events after dup: %v", err)
	}
	if len(evs) != 3 {
		t.Fatalf("duplicate delivery changed feed length: %d", len(evs))
	}
}
