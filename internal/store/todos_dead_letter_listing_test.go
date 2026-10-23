package store

// DB-backed coverage for the operator's dead-letter listing (issue #588): the Dead pill reads across
// EVERY endpoint the signed-in human owns, and it discloses nothing to any other human. Skips
// without SWITCHBOARD_TEST_DATABASE_URL, like the rest of this suite.
//
// The listing exists because a dead letter is claimable by nobody but an operator, so silence about
// one costs the whole queue. That makes it a disclosure, and counts are disclosures too: the read is
// scoped by the same ownedByHuman predicate as every Board read, and its predicate is the one
// TodoCounts counts, so the pill's number and the rows under it cannot disagree.
//
// Governing: SPEC-0007 REQ "Human as Accountable Principal", SPEC-0033 REQ "Owner-Scoped History
// Reads", SPEC-0013 REQ "Todos View", ADR-0049 (proposed).

import (
	"context"
	"testing"
	"time"
)

// deadLetterOn drives one todo to a real dead letter on ep: a verified delivery, the attempt cap,
// and a fail taken there. Nothing is written by hand, so the row is dead-lettered the way the
// reaper and the fail verb dead-letter it.
func deadLetterOn(t *testing.T, s *Store, ctx context.Context, ep, key string) string {
	t.Helper()
	id := createEventTodo(t, s, ctx, ep, key, true, "signed").ID
	exhaust(t, s, ctx, id)
	if _, err := s.ClaimTodo(ctx, ep, id, "agent:dl-list", time.Hour); err != nil {
		t.Fatalf("claim %s: %v", key, err)
	}
	if _, err := s.FailTodo(ctx, ep, id, "agent:dl-list", []byte(`{"error":"still red"}`)); err != nil {
		t.Fatalf("fail %s at the cap: %v", key, err)
	}
	return id
}

func holdsDeadLetter(items []TodoItem, id string) bool {
	for _, it := range items {
		if it.ID == id {
			return true
		}
	}
	return false
}

func TestListDeadLettersSpansOwnedEndpointsAndStaysPrivate(t *testing.T) {
	s, ctx := testStore(t)

	ownerEp := seedEndpoint(t, s, ctx, "dl-list-owner", "inbox")
	otherEp := seedEndpoint(t, s, ctx, "dl-list-other", "inbox")

	// A second endpoint of the owner's own human, vended on the owner's agent: the point of the
	// listing is that an operator with more than one lane sees all of their dead letters at once.
	var agentID string
	if err := s.pool.QueryRow(ctx, `SELECT agent_id::text FROM endpoints WHERE id = $1`, ownerEp).Scan(&agentID); err != nil {
		t.Fatalf("owner agent: %v", err)
	}
	slug, err := MintSlug("dl-list-sibling")
	if err != nil {
		t.Fatalf("slug: %v", err)
	}
	sibling, err := s.CreateEndpoint(ctx, agentID, "credhash-dl-list-sibling", "sbk_dlsl", slug,
		[]string{"inbox"}, []string{"list_todos", "claim", "complete", "fail", "heartbeat"})
	if err != nil {
		t.Fatalf("sibling endpoint: %v", err)
	}

	owner, other := ownerOf(t, s, ctx, ownerEp), ownerOf(t, s, ctx, otherEp)
	if owner != ownerOf(t, s, ctx, sibling.ID) {
		t.Fatal("fixture: the sibling endpoint must belong to the owner's human")
	}
	if owner == other {
		t.Fatal("fixture: the two seeds must belong to different humans")
	}

	mineFirst := deadLetterOn(t, s, ctx, ownerEp, "dl-list-mine-a")
	mineSecond := deadLetterOn(t, s, ctx, sibling.ID, "dl-list-mine-b")
	theirs := deadLetterOn(t, s, ctx, otherEp, "dl-list-theirs")

	mine, err := s.ListDeadLetters(ctx, owner, "", 100)
	if err != nil {
		t.Fatalf("list dead letters: %v", err)
	}
	if !holdsDeadLetter(mine, mineFirst) || !holdsDeadLetter(mine, mineSecond) {
		t.Fatalf("listing = %v, want dead letters from both of the owner's endpoints", deadLetterIDs(mine))
	}
	if holdsDeadLetter(mine, theirs) {
		t.Fatalf("another human's dead letter appeared in the listing: %v", deadLetterIDs(mine))
	}
	for _, it := range mine {
		if !it.DeadLetter() {
			t.Fatalf("listed todo %s is not a dead letter (state %q, next_retry_at %v)", it.ID, it.State, it.NextRetryAt)
		}
	}

	// The pill's count and the rows under it are the same predicate read once each: a number that
	// counts rows the table cannot show is how a dead letter hides beside a big failed count.
	counts, err := s.TodoCounts(ctx, owner)
	if err != nil {
		t.Fatalf("todo counts: %v", err)
	}
	if counts.DeadLettered != len(mine) {
		t.Fatalf("Dead pill counts %d but the listing returns %d rows (%v)", counts.DeadLettered, len(mine), deadLetterIDs(mine))
	}

	theirRows, err := s.ListDeadLetters(ctx, other, "", 100)
	if err != nil {
		t.Fatalf("other human's listing: %v", err)
	}
	if !holdsDeadLetter(theirRows, theirs) {
		t.Fatalf("the other human's own dead letter is missing from their listing: %v", deadLetterIDs(theirRows))
	}
	if holdsDeadLetter(theirRows, mineFirst) || holdsDeadLetter(theirRows, mineSecond) {
		t.Fatalf("the listing leaked the owner's dead letters to another human: %v", deadLetterIDs(theirRows))
	}

	// An id that is nobody's (or an operator with no endpoints) reads empty rather than everything.
	none, err := s.ListDeadLetters(ctx, "00000000-0000-0000-0000-000000000000", "", 100)
	if err != nil {
		t.Fatalf("unknown owner listing: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("unknown owner returned %v, want nothing", deadLetterIDs(none))
	}
	assertAttemptInvariant(t, s, ctx)
}

// deadLetterIDs projects a listing for failure messages, so a failure names rows instead of dumping
// whole TodoItems (each of which carries a payload).
func deadLetterIDs(items []TodoItem) []string {
	ids := make([]string, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	return ids
}
