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
	ep := seedEndpoint(t, s, ctx, "board-stats", "reviews")
	me := ownerOf(t, s, ctx, ep)

	// Empty database: every tile is zero, and VerifiedPct's divide-by-zero guard holds.
	b, err := s.BoardStats(ctx, me)
	if err != nil {
		t.Fatalf("board stats (empty): %v", err)
	}
	if b != (BoardStats{}) {
		t.Fatalf("empty db stats = %+v, want all zeros", b)
	}

	// Seed three deliveries as event+todo PAIRS (two signed, one open). The pairing is load-bearing
	// now, not incidental: an event reaches these tiles only through a todo this human owns, so
	// three bare InsertEvents — which is what this fixture used to do — are owned by nobody and
	// counted for nobody. Attributing them is the fix, not a workaround.
	var claimID string
	for i, key := range []string{"e1", "e2", "e3"} {
		mode, verified := "signed", true
		if i == 2 {
			mode, verified = "open", false
		}
		_, td, _, err := s.CreateEventTodo(ctx, EventInput{
			Source: "github", Family: "webhook", EventType: "push",
			ExternalID: key, TrustMode: mode, Verified: verified,
		}, CreateTodoParams{EndpointID: ep, Queue: "reviews", Title: "todo", IdempotencyKey: key})
		if err != nil {
			t.Fatalf("seed delivery %s: %v", key, err)
		}
		claimID = td.ID
	}
	if _, err := s.ClaimTodo(ctx, ep, claimID, "worker-1", time.Minute); err != nil {
		t.Fatalf("claim seed todo: %v", err)
	}

	// A SECOND human's board must not move any tile above. This is the regression the tiles
	// themselves needed: counts leak just as surely as rows, and nothing in a number looks like
	// somebody else's data.
	other := seedEndpoint(t, s, ctx, "board-stats-other", "reviews")
	if _, _, _, err := s.CreateEventTodo(ctx, EventInput{
		Source: "github", Family: "webhook", EventType: "push",
		ExternalID: "other-1", TrustMode: "signed", Verified: true,
	}, CreateTodoParams{EndpointID: other, Queue: "reviews", Title: "not yours", IdempotencyKey: "other-1"}); err != nil {
		t.Fatalf("seed other human delivery: %v", err)
	}

	b, err = s.BoardStats(ctx, me)
	if err != nil {
		t.Fatalf("board stats (seeded): %v", err)
	}
	want := BoardStats{
		TotalTodos:    3,
		TodosToday:    3,
		InFlight:      1,
		AwaitingClaim: 2,
		VerifiedPct:   67, // round(100 * 2/3)
		EventsPerMin:  3,
	}
	if b != want {
		t.Fatalf("seeded stats = %+v, want %+v (the other human's delivery must not count)", b, want)
	}
}

func TestRecentEvents(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "recent-events", "q")
	me := ownerOf(t, s, ctx, ep)

	// Empty feed is empty, not an error.
	evs, err := s.RecentEvents(ctx, me, 12)
	if err != nil {
		t.Fatalf("recent events (empty): %v", err)
	}
	if len(evs) != 0 {
		t.Fatalf("empty db returned %d events", len(evs))
	}

	// Seeded as event+todo pairs: the feed shows a human their OWN deliveries, and an event is
	// theirs only through a todo they own.
	seed := []EventInput{
		{Source: "github", Family: "webhook", EventType: "push", ExternalID: "g1", TrustMode: "signed", Verified: true},
		{Source: "stripe", Family: "webhook", EventType: "invoice.paid", ExternalID: "s1", TrustMode: "token", Verified: true},
		{Source: "curl", Family: "webhook", ExternalID: "c1", TrustMode: "open"}, // no event type → ""
	}
	ids := make([]int64, 0, len(seed))
	for _, e := range seed {
		id, _, _, err := s.CreateEventTodo(ctx, e,
			CreateTodoParams{EndpointID: ep, Queue: "q", Title: "t", IdempotencyKey: e.ExternalID})
		if err != nil {
			t.Fatalf("seed %s: %v", e.Source, err)
		}
		ids = append(ids, id)
	}

	// Another human's delivery never appears in this feed, at any limit.
	other := seedEndpoint(t, s, ctx, "recent-events-other", "q")
	if _, _, _, err := s.CreateEventTodo(ctx,
		EventInput{Source: "gitea", Family: "webhook", EventType: "pull_request", ExternalID: "o1", TrustMode: "signed", Verified: true},
		CreateTodoParams{EndpointID: other, Queue: "q", Title: "not yours", IdempotencyKey: "o1"}); err != nil {
		t.Fatalf("seed other human delivery: %v", err)
	}

	// Newest first, limit respected.
	evs, err = s.RecentEvents(ctx, me, 2)
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
	evs, err = s.RecentEvents(ctx, me, 12)
	if err != nil {
		t.Fatalf("recent events after dup: %v", err)
	}
	if len(evs) != 3 {
		t.Fatalf("feed length = %d, want 3 (the other human's delivery must not appear)", len(evs))
	}
	for _, e := range evs {
		if e.Source == "gitea" {
			t.Fatalf("another human's delivery surfaced in the feed: %+v", e)
		}
	}
}
