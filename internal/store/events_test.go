package store

// DB-backed coverage for the SPEC-0005 event-history reads: cursor pagination stability under the
// stable received_at DESC, id DESC order (no duplicates or gaps, tie-break on id at a page edge),
// provider/event_type/since filtering, and the ByID full-record read. These run only against the
// Postgres harness (SWITCHBOARD_TEST_DATABASE_URL) and skip cleanly otherwise.
//
// Governing: SPEC-0005 REQ "Deterministic Pagination and Filtering", REQ "Event Shape Parity and
// Trust Disclosure".

import (
	"context"
	"errors"
	"testing"
	"time"
)

// seedEvent inserts one event with an explicit received_at (bypassing the now() default) so tests
// can pin the ordering — including two rows sharing a timestamp to exercise the id tiebreak.
// The event is owned by endpoint ep: every history read is owner-scoped (SPEC-0033).
func seedEvent(t *testing.T, s *Store, ctx context.Context, ep, source, eventType, trustMode string, verified bool, receivedAt time.Time) int64 {
	t.Helper()
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO events (source, family, event_type, trust_mode, verified, payload, payload_size, received_at, endpoint_id)
		VALUES ($1, 'webhook', NULLIF($2,''), $3, $4, $5, $6, $7, $8)
		RETURNING id`,
		source, eventType, trustMode, verified, []byte(`{}`), 2, receivedAt, ep).Scan(&id)
	if err != nil {
		t.Fatalf("seed event: %v", err)
	}
	return id
}

// TestListEventHistoryCursorStability pages the whole log with limit 2 using each page's last
// (received_at, id) as the next keyset bound, and asserts the walk is newest-first, exhaustive, and
// free of duplicates or gaps — including across a page edge where two rows share a received_at.
func TestListEventHistoryCursorStability(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "history", "q")
	owner := ownerOf(t, s, ctx, ep)

	base := time.Now().Truncate(time.Millisecond).Add(-time.Hour)
	// ids come back in insert order; rows 4 and 5 share a timestamp so id DESC breaks the tie, and
	// that tie straddles the boundary between page 1 (ids 5,4) and forces the keyset to be a tuple.
	id1 := seedEvent(t, s, ctx, ep, "github", "push", "signed", true, base.Add(1*time.Second))
	id2 := seedEvent(t, s, ctx, ep, "github", "push", "signed", true, base.Add(2*time.Second))
	id3 := seedEvent(t, s, ctx, ep, "stripe", "charge", "signed", true, base.Add(3*time.Second))
	id4 := seedEvent(t, s, ctx, ep, "github", "pull", "signed", true, base.Add(4*time.Second))
	id5 := seedEvent(t, s, ctx, ep, "github", "push", "token", false, base.Add(4*time.Second))
	want := []int64{id5, id4, id3, id2, id1}

	var seen []int64
	var cursorT time.Time
	var cursorID int64
	for page := 0; page < 10; page++ {
		f := EventHistoryFilter{Limit: 2, CursorTime: cursorT, CursorID: cursorID}
		items, err := s.ListEventHistory(ctx, owner, f)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if len(items) == 0 {
			break
		}
		for _, it := range items {
			seen = append(seen, it.ID)
		}
		last := items[len(items)-1]
		cursorT, cursorID = last.ReceivedAt, last.ID
		if len(items) < 2 {
			break
		}
	}
	if len(seen) != len(want) {
		t.Fatalf("paged %d ids (%v), want %d (%v)", len(seen), seen, len(want), want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("paged ids = %v, want %v (newest-first, no dupes/gaps)", seen, want)
		}
	}

	// A page inserted between requests never re-emits or skips: after taking the first page (5,4),
	// a new newest event does not appear on the continued (older) walk.
	first, err := s.ListEventHistory(ctx, owner, EventHistoryFilter{Limit: 2})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	last := first[len(first)-1]
	newest := seedEvent(t, s, ctx, ep, "github", "push", "signed", true, base.Add(10*time.Second))
	rest, err := s.ListEventHistory(ctx, owner, EventHistoryFilter{
		Limit: 100, CursorTime: last.ReceivedAt, CursorID: last.ID})
	if err != nil {
		t.Fatalf("continued page: %v", err)
	}
	for _, it := range rest {
		if it.ID == newest || it.ID == id5 || it.ID == id4 {
			t.Fatalf("continued walk leaked head/duplicate id %d: %v", it.ID, idsList(rest))
		}
	}
	if got := idsList(rest); len(got) != 3 {
		t.Fatalf("continued walk = %v, want the 3 older rows", got)
	}
}

// TestListEventHistoryFiltering covers provider, event_type, and both since forms (timestamp lower
// bound and id lower bound).
func TestListEventHistoryFiltering(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "history", "q")
	owner := ownerOf(t, s, ctx, ep)

	base := time.Now().Truncate(time.Millisecond).Add(-time.Hour)
	seedEvent(t, s, ctx, ep, "github", "push", "signed", true, base.Add(1*time.Second))
	seedEvent(t, s, ctx, ep, "stripe", "charge", "signed", true, base.Add(2*time.Second))
	id3 := seedEvent(t, s, ctx, ep, "github", "pull", "signed", true, base.Add(3*time.Second))
	id4 := seedEvent(t, s, ctx, ep, "github", "push", "signed", true, base.Add(4*time.Second))

	// provider + event_type
	got, err := s.ListEventHistory(ctx, owner, EventHistoryFilter{Provider: "github", EventType: "push"})
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	if ids := idsList(got); len(ids) != 2 || ids[0] != id4 {
		t.Fatalf("github/push = %v, want [%d 1]", ids, id4)
	}

	// since timestamp (inclusive lower bound)
	got, err = s.ListEventHistory(ctx, owner, EventHistoryFilter{SinceTime: base.Add(3 * time.Second)})
	if err != nil {
		t.Fatalf("since-time: %v", err)
	}
	if ids := idsList(got); len(ids) != 2 || ids[0] != id4 || ids[1] != id3 {
		t.Fatalf("since-time = %v, want [%d %d]", ids, id4, id3)
	}

	// since id (id >= n)
	got, err = s.ListEventHistory(ctx, owner, EventHistoryFilter{SinceID: id3})
	if err != nil {
		t.Fatalf("since-id: %v", err)
	}
	if ids := idsList(got); len(ids) != 2 || ids[0] != id4 || ids[1] != id3 {
		t.Fatalf("since-id = %v, want [%d %d]", ids, id4, id3)
	}
}

// TestEventHistoryByIDRoundTrip: the full-record read returns the sanitized detail and unknown ids
// are ErrNotFound.
func TestEventHistoryByIDRoundTrip(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "history", "q")
	owner := ownerOf(t, s, ctx, ep)

	id := seedEvent(t, s, ctx, ep, "github", "push", "signed", true, time.Now())
	d, err := s.EventHistoryByID(ctx, owner, id)
	if err != nil {
		t.Fatalf("by id: %v", err)
	}
	if d.ID != id || d.Provider != "github" || d.TrustMode != "signed" || !d.Verified {
		t.Fatalf("detail = %+v", d)
	}
	if _, err := s.EventHistoryByID(ctx, owner, 999999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id = %v, want ErrNotFound", err)
	}
}

func idsList(items []EventHistoryItem) []int64 {
	out := make([]int64, 0, len(items))
	for _, it := range items {
		out = append(out, it.ID)
	}
	return out
}
