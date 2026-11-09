package store

// DB-backed tests for the operator supervision reads: per-queue liveness inside one human's tenant,
// and the leases outstanding on it. These are the two answers a supervising agent could not get
// without a page of listing, so the tests pin the three things that make them trustworthy: a granted
// queue with no rows still answers, another tenant's rows are absent rather than withheld, and a
// lease is reported with the holder and the expiry that make it actionable.
//
// Skipped without SWITCHBOARD_TEST_DATABASE_URL. Governing: ADR-0056, ADR-0048, SPEC-0007.
//
// @joestump-agent 10/30/2026 - Added for stump.wtf/switchboard#582.

import (
	"context"
	"testing"
	"time"
)

func TestQueueLivenessForHumanAnswersEmptyAndPausedQueues(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "supervise-liveness", "alpha", "beta")
	human := endpointHuman(t, s, ctx, ep)

	if err := s.PauseQueue(ctx, "beta", human, "upgrade window"); err != nil {
		t.Fatalf("pause beta: %v", err)
	}
	first := createEventTodoIn(t, s, ctx, ep, "alpha", "sl-1", true, "signed")
	createEventTodoIn(t, s, ctx, ep, "alpha", "sl-2", true, "signed")
	if _, err := s.ClaimTodo(ctx, ep, first.ID, "holder", time.Hour); err != nil {
		t.Fatalf("claim: %v", err)
	}

	got := liveness(t, s, ctx, human, "alpha", "beta", "never-enqueued")
	if len(got) != 3 {
		t.Fatalf("liveness returned %d rows, want one per named queue: %+v", len(got), got)
	}
	if got[0].Queue != "alpha" || got[1].Queue != "beta" || got[2].Queue != "never-enqueued" {
		t.Fatalf("liveness is not sorted by queue name: %+v", got)
	}
	alpha := got[0]
	if alpha.Pending != 1 || alpha.Claimed != 1 {
		t.Errorf("alpha counts pending=%d claimed=%d, want 1 and 1", alpha.Pending, alpha.Claimed)
	}
	if alpha.OldestPendingSeconds < 0 || alpha.Paused {
		t.Errorf("alpha reads %+v, want no pause and a sane age", alpha)
	}
	if !got[1].Paused || got[1].Pending != 0 {
		t.Errorf("beta reads %+v, want a paused zero row", got[1])
	}
	// A queue that was named but has never carried work is a row of zeros, not a missing row:
	// "nothing waiting" has to be distinguishable from "no answer".
	if got[2] != (QueueLiveness{Queue: "never-enqueued"}) {
		t.Errorf("an unused queue reads %+v, want the zero row", got[2])
	}

	// A dead letter is counted, and counted only where a retry is not coming: failed with no
	// scheduled backoff. A retrying failure counts in failed alone.
	dead := createEventTodoIn(t, s, ctx, ep, "alpha", "sl-3", true, "signed")
	if _, err := s.ClaimTodo(ctx, ep, dead.ID, "holder", time.Hour); err != nil {
		t.Fatalf("claim for the dead letter: %v", err)
	}
	if _, err := s.FailTodo(ctx, ep, dead.ID, "holder", nil); err != nil {
		t.Fatalf("fail: %v", err)
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE todos SET next_retry_at = NULL WHERE id = $1`, dead.ID); err != nil {
		t.Fatalf("stop the scheduled retry: %v", err)
	}
	retry := createEventTodoIn(t, s, ctx, ep, "alpha", "sl-4", true, "signed")
	if _, err := s.ClaimTodo(ctx, ep, retry.ID, "holder", time.Hour); err != nil {
		t.Fatalf("claim for the retrying row: %v", err)
	}
	if _, err := s.FailTodo(ctx, ep, retry.ID, "holder", nil); err != nil {
		t.Fatalf("fail below the cap: %v", err)
	}
	after := liveness(t, s, ctx, human, "alpha")
	if after[0].Failed != 2 || after[0].DeadLettered != 1 {
		t.Errorf("alpha reads failed=%d dead=%d, want 2 and 1", after[0].Failed, after[0].DeadLettered)
	}
}

// Another human's rows are absent, not withheld: their queue of the same name is a zero row, which
// is what the Board's reads already promise and what a count on a supervision surface must not leak.
func TestQueueLivenessForHumanIsTenantScoped(t *testing.T) {
	s, ctx := testStore(t)
	mine := seedEndpoint(t, s, ctx, "supervise-mine", "shared")
	theirs := seedEndpoint(t, s, ctx, "supervise-theirs", "shared")
	myHuman := endpointHuman(t, s, ctx, mine)
	theirHuman := endpointHuman(t, s, ctx, theirs)

	createEventTodoIn(t, s, ctx, mine, "shared", "sl-mine-1", true, "signed")
	createEventTodoIn(t, s, ctx, mine, "shared", "sl-mine-2", true, "signed")
	theirTodo := createEventTodoIn(t, s, ctx, theirs, "shared", "sl-theirs-1", true, "signed")
	if _, err := s.ClaimTodo(ctx, theirs, theirTodo.ID, "their-holder", time.Hour); err != nil {
		t.Fatalf("their claim: %v", err)
	}

	mineRows := liveness(t, s, ctx, myHuman, "shared")
	if mineRows[0].Pending != 2 || mineRows[0].Claimed != 0 {
		t.Errorf("my liveness reads pending=%d claimed=%d, want 2 and 0",
			mineRows[0].Pending, mineRows[0].Claimed)
	}
	theirRows := liveness(t, s, ctx, theirHuman, "shared")
	if theirRows[0].Pending != 0 || theirRows[0].Claimed != 1 {
		t.Errorf("their liveness reads pending=%d claimed=%d, want 0 and 1",
			theirRows[0].Pending, theirRows[0].Claimed)
	}
}

func TestOutstandingLeasesForHuman(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "supervise-leases", "inbox")
	human := endpointHuman(t, s, ctx, ep)
	theirs := seedEndpoint(t, s, ctx, "supervise-leases-other", "inbox")
	theirHuman := endpointHuman(t, s, ctx, theirs)

	held := createEventTodoIn(t, s, ctx, ep, "inbox", "ol-1", true, "signed")
	if _, err := s.ClaimTodo(ctx, ep, held.ID, "agent:holder-1", time.Hour); err != nil {
		t.Fatalf("claim: %v", err)
	}
	theirHeld := createEventTodoIn(t, s, ctx, theirs, "inbox", "ol-2", true, "signed")
	if _, err := s.ClaimTodo(ctx, theirs, theirHeld.ID, "agent:holder-2", time.Hour); err != nil {
		t.Fatalf("their claim: %v", err)
	}

	leases, err := s.OutstandingLeasesForHuman(ctx, human, []string{"inbox"}, 0)
	if err != nil {
		t.Fatalf("outstanding leases: %v", err)
	}
	if len(leases) != 1 {
		t.Fatalf("leases = %+v, wanting only this tenant's held row", leases)
	}
	got := leases[0]
	if got.TodoID != held.ID || got.Queue != "inbox" || got.Owner != "agent:holder-1" {
		t.Errorf("lease = %+v, want the held todo, its queue and its holder", got)
	}
	if got.Attempt < 1 {
		t.Errorf("lease attempt = %d, wanting the count the next claim will follow", got.Attempt)
	}
	if got.LeaseExpiresAt == nil || !got.LeaseExpiresAt.After(time.Now()) {
		t.Errorf("lease expiry = %v, wanting one in the future", got.LeaseExpiresAt)
	}

	// A released lease stops being outstanding the moment it is handed back.
	if _, err := s.ReleaseTodo(ctx, ep, held.ID, "agent:holder-1"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if again, err := s.OutstandingLeasesForHuman(ctx, human, []string{"inbox"}, 0); err != nil {
		t.Fatalf("re-read: %v", err)
	} else if len(again) != 0 {
		t.Errorf("leases after release = %+v, want none", again)
	}

	// A queue outside the grant is not read at all.
	if none, err := s.OutstandingLeasesForHuman(ctx, human, nil, 0); err != nil || len(none) != 0 {
		t.Errorf("empty queue list = (%v, %v), want nothing read", none, err)
	}
	if mine, err := s.OutstandingLeasesForHuman(ctx, theirHuman, []string{"inbox"}, 0); err != nil {
		t.Fatalf("their re-read: %v", err)
	} else if len(mine) != 1 || mine[0].Owner != "agent:holder-2" {
		t.Errorf("their leases = %+v, wanting only their held row", mine)
	}
}

func liveness(t *testing.T, s *Store, ctx context.Context, human string, queues ...string) []QueueLiveness {
	t.Helper()
	rows, err := s.QueueLivenessForHuman(ctx, human, queues)
	if err != nil {
		t.Fatalf("queue liveness: %v", err)
	}
	return rows
}

func endpointHuman(t *testing.T, s *Store, ctx context.Context, endpointID string) string {
	t.Helper()
	human, err := s.EndpointOwnerHuman(ctx, endpointID)
	if err != nil {
		t.Fatalf("endpoint owner: %v", err)
	}
	return human
}
