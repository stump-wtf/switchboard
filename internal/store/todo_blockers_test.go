package store

// DB-backed tests for issue-dependency holds (ADR-0055, stump.wtf/switchboard#583): work whose tracker
// issue has open blockers is recorded and visible but never handed out, and it reaches the queue by
// itself when the last blocker closes. Each test drives the entry points the server does — create,
// both claim verbs, the close path, and the re-check candidate query — so the promise is checked where
// it is kept rather than at one predicate.
//
// Skipped without SWITCHBOARD_TEST_DATABASE_URL. Governing: ADR-0055 (proposed).

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func ref(owner, repo string, number int) BlockerRef {
	return BlockerRef{Owner: owner, Repo: repo, Number: number, State: "open"}
}

func holdParams(ep, queue, issueRef string, blockers ...BlockerRef) CreateTodoParams {
	return CreateTodoParams{
		EndpointID: ep, Queue: queue, Source: "gitea", Kind: "issues/assigned",
		Title: "hold me", Payload: []byte(`{"issue":"held"}`),
		IdempotencyKey: "idem-" + strings.ReplaceAll(issueRef, "/", "-"),
		IssueRef:       issueRef, Blockers: blockers,
	}
}

func TestHeldTodoIsCreatedVisibleAndUnclaimable(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "hold-claim", "inbox")
	var rang []string
	s.SetTodoDoorbellHook(func(td Todo) { rang = append(rang, td.ID) })
	defer s.SetTodoDoorbellHook(nil)

	held, created, err := s.CreateTodo(ctx, holdParams(ep, "inbox", "octo/repo#12", ref("octo", "repo", 7)))
	if err != nil {
		t.Fatalf("held create: %v", err)
	}
	if !created {
		t.Fatal("the first hold reported created=false")
	}
	if held.State != StateBlocked {
		t.Fatalf("state = %q, want %q", held.State, StateBlocked)
	}
	if held.IssueRef != "octo/repo#12" {
		t.Errorf("issue_ref = %q, want octo/repo#12", held.IssueRef)
	}
	// Silent on the way in: no doorbell for work nobody may start.
	if len(rang) != 0 {
		t.Fatalf("held create rang the doorbell: %v", rang)
	}
	// Visible: listed by state, so the operator sees the queue is not empty.
	listed, err := s.ListTodos(ctx, ep, []string{"inbox"}, StateBlocked, 10)
	if err != nil {
		t.Fatalf("list blocked: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != held.ID {
		t.Fatalf("list blocked = %+v, want the one held todo", listed)
	}
	// Unclaimable, by both paths.
	if _, err := s.ClaimNext(ctx, ep, []string{"inbox"}, "worker", time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("claim_next handed out a held todo: %v", err)
	}
	if _, err := s.ClaimTodo(ctx, ep, held.ID, "worker", time.Minute); err == nil {
		t.Fatal("claim by id succeeded on a held todo")
	}
	// And a plain pending listing does not pretend it is available.
	pending, err := s.ListTodos(ctx, ep, []string{"inbox"}, "pending", 10)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("a held todo showed up as pending: %+v", pending)
	}
	refs, err := s.BlockersFor(ctx, held.ID)
	if err != nil {
		t.Fatalf("blockers: %v", err)
	}
	if len(refs) != 1 || refs[0].RefString() != "octo/repo#7" || refs[0].State != "open" {
		t.Errorf("recorded refs = %+v, want octo/repo#7 open", refs)
	}
}

func TestDeliveriesForAHeldIssueCoalesce(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "hold-fold", "inbox")
	var rang []string
	s.SetTodoDoorbellHook(func(td Todo) { rang = append(rang, td.ID) })
	defer s.SetTodoDoorbellHook(nil)

	first, _, err := s.CreateTodo(ctx, holdParams(ep, "inbox", "octo/repo#12", ref("octo", "repo", 7)))
	if err != nil {
		t.Fatalf("first hold: %v", err)
	}
	// A later event for the same issue, with a different idempotency key: the issue is still waiting,
	// so it refreshes the item that is waiting instead of queueing a second copy that would all
	// release at once.
	second := holdParams(ep, "inbox", "octo/repo#12", ref("octo", "repo", 7), ref("other", "thing", 3))
	second.IdempotencyKey = "idem-later-event"
	second.Title = "still waiting"
	got, created, err := s.CreateTodo(ctx, second)
	if err != nil {
		t.Fatalf("folding create: %v", err)
	}
	if created {
		t.Fatal("a delivery for an already-held issue reported created=true")
	}
	if got.ID != first.ID {
		t.Fatalf("fold produced a new row %s, want the held one %s", got.ID, first.ID)
	}
	if got.Title != "still waiting" {
		t.Errorf("fold did not refresh the waiting row: title = %q", got.Title)
	}
	if len(rang) != 0 {
		t.Fatalf("folding create rang the doorbell: %v", rang)
	}
	// A retry of the first delivery (same idempotency key) dedups like any other delivery: the same
	// row, created=false, and no unique violation from the dedup index the hold fold does not name.
	again, created, err := s.CreateTodo(ctx, holdParams(ep, "inbox", "octo/repo#12", ref("octo", "repo", 7)))
	if err != nil {
		t.Fatalf("redelivered hold: %v", err)
	}
	if created || again.ID != first.ID {
		t.Errorf("redelivery = (%s, created=%v), want %s and created=false", again.ID, created, first.ID)
	}
	rows, err := s.ListTodos(ctx, ep, []string{"inbox"}, "", 50)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("%d todos, want the single held row: %+v", len(rows), rows)
	}
	refs, err := s.BlockersFor(ctx, first.ID)
	if err != nil {
		t.Fatalf("blockers: %v", err)
	}
	if len(refs) != 2 {
		t.Errorf("fold recorded %d refs, want both the old and the new blocker: %+v", len(refs), refs)
	}
	if h, found, err := s.HeldTodoForIssue(ctx, ep, "inbox", "octo/repo#12"); err != nil || !found || h.ID != first.ID {
		t.Errorf("HeldTodoForIssue = (%v, %v, %v)", h.ID, found, err)
	}
}

func TestClosingTheLastBlockerReleasesExactlyOnce(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "hold-release", "inbox")
	var rang []string
	s.SetTodoDoorbellHook(func(td Todo) { rang = append(rang, td.ID) })
	defer s.SetTodoDoorbellHook(nil)

	held, _, err := s.CreateTodo(ctx, holdParams(ep, "inbox", "octo/repo#12",
		ref("octo", "repo", 7), ref("other", "thing", 3)))
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	// The first close is not the last: nothing releases, and nothing rings.
	released, err := s.MarkBlockerState(ctx, "other", "thing", 3, "closed")
	if err != nil {
		t.Fatalf("mark first blocker: %v", err)
	}
	if len(released) != 0 {
		t.Fatalf("closing one of two blockers released %d todos", len(released))
	}
	if len(rang) != 0 {
		t.Fatalf("a partial release rang the doorbell: %v", rang)
	}
	if got, err := s.GetTodo(ctx, ep, held.ID); err != nil || got.State != StateBlocked {
		t.Fatalf("todo after partial close: state=%q err=%v, still want blocked", got.State, err)
	}
	// The last one closes: the dependent becomes claimable, and rings once.
	released, err = s.MarkBlockerState(ctx, "octo", "repo", 7, "closed")
	if err != nil {
		t.Fatalf("mark last blocker: %v", err)
	}
	if len(released) != 1 || released[0].ID != held.ID {
		t.Fatalf("released = %+v, want the one dependent", released)
	}
	if got, err := s.GetTodo(ctx, ep, held.ID); err != nil || got.State != "pending" {
		t.Fatalf("after release: state=%q err=%v, want pending", got.State, err)
	}
	if len(rang) != 1 || rang[0] != held.ID {
		t.Fatalf("doorbell rings = %v, want exactly one for %s", rang, held.ID)
	}
	claimable, err := s.ClaimNext(ctx, ep, []string{"inbox"}, "worker", time.Minute)
	if err != nil {
		t.Fatalf("claim after release: %v", err)
	}
	if claimable.ID != held.ID {
		t.Errorf("claimed %s, want the released todo", claimable.ID)
	}
	// A second close of the same issue (a re-delivery) releases nothing further.
	if again, err := s.MarkBlockerState(ctx, "octo", "repo", 7, "closed"); err != nil || len(again) != 0 {
		t.Errorf("re-delivered close released %+v err %v, want nothing", again, err)
	}
}

func TestReopenedBlockerReHoldsOnlyWhatIsNotYetClaimed(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "hold-reopen", "inbox")
	held, _, err := s.CreateTodo(ctx, holdParams(ep, "inbox", "octo/repo#12", ref("octo", "repo", 7)))
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	if _, err := s.MarkBlockerState(ctx, "octo", "repo", 7, "closed"); err != nil {
		t.Fatalf("close: %v", err)
	}
	// The blocker is reopened. Marking it open releases nothing — only a close releases — and the
	// re-check's HoldTodo is what puts the still-claimable row back to waiting.
	released, err := s.MarkBlockerState(ctx, "octo", "repo", 7, "open")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if len(released) != 0 {
		t.Fatalf("a reopen released %d todos, want none", len(released))
	}
	got, err := s.HoldTodo(ctx, held.ID, "octo/repo#12", []BlockerRef{ref("octo", "repo", 7)})
	if err != nil {
		t.Fatalf("re-hold: %v", err)
	}
	if got.State != StateBlocked {
		t.Fatalf("state after re-hold = %q, want blocked", got.State)
	}
	if _, err := s.ClaimNext(ctx, ep, []string{"inbox"}, "worker", time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a re-held todo is claimable: %v", err)
	}
	// Work already in flight is never interrupted by a hold.
	if _, err := s.ReleaseHeldTodo(ctx, held.ID); err != nil {
		t.Fatalf("release for the claim: %v", err)
	}
	if _, err := s.ClaimTodo(ctx, ep, held.ID, "worker", time.Minute); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := s.HoldTodo(ctx, held.ID, "octo/repo#12", []BlockerRef{ref("octo", "repo", 7)}); !errors.Is(err, ErrConflict) {
		t.Fatalf("hold of a claimed todo = %v, want ErrConflict", err)
	}
	if got, err := s.GetTodo(ctx, ep, held.ID); err != nil || got.State != "claimed" {
		t.Fatalf("a hold interrupted work in flight: state=%q err=%v", got.State, err)
	}
	// A hold with no refs would be released as an orphan by the next sweep, so it is refused outright.
	if _, err := s.HoldTodo(ctx, held.ID, "octo/repo#12", nil); err == nil {
		t.Error("HoldTodo with no blockers = nil error, want a refusal")
	}
}

func TestHoldsAcrossRepositoriesReleaseOnTheBlockersOwnClose(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "hold-crossrepo", "inbox")
	held, _, err := s.CreateTodo(ctx, holdParams(ep, "inbox", "stump.wtf/switchboard#583",
		ref("stump.wtf", "cairn", 41)))
	if err != nil {
		t.Fatalf("cross-repo hold: %v", err)
	}
	dependents, err := s.HeldTodosDependingOn(ctx, "stump.wtf", "cairn", 41)
	if err != nil {
		t.Fatalf("dependents: %v", err)
	}
	if len(dependents) != 1 || dependents[0].ID != held.ID {
		t.Fatalf("dependents = %+v, want the held todo", dependents)
	}
	released, err := s.MarkBlockerState(ctx, "stump.wtf", "cairn", 41, "closed")
	if err != nil {
		t.Fatalf("close the cross-repo blocker: %v", err)
	}
	if len(released) != 1 {
		t.Fatalf("released %+v, want the dependent", released)
	}
	after, err := s.HeldTodosDependingOn(ctx, "stump.wtf", "cairn", 41)
	if err != nil {
		t.Fatalf("dependents after release: %v", err)
	}
	if len(after) != 0 {
		t.Errorf("a released todo still lists as held: %+v", after)
	}
}

func TestTodosWithHoldsPicksStaleHoldsAndOrphans(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "hold-sweep", "inbox")

	fresh, _, err := s.CreateTodo(ctx, holdParams(ep, "inbox", "octo/repo#12", ref("octo", "repo", 7)))
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	// Just checked: the next pass must leave it alone, or a busy instance dials every tick.
	if cands, err := s.TodosWithHolds(ctx, 5*time.Minute, 50); err != nil {
		t.Fatalf("candidates: %v", err)
	} else if len(cands) != 0 {
		t.Fatalf("a just-checked hold is offered to the sweep: %+v", cands)
	}
	// Stale refs: now it is a candidate, with its refs attached and no webhook to dial (the fixture
	// todo has no event, and a todo with no event has no stored connection).
	cands, err := s.TodosWithHolds(ctx, 0, 50)
	if err != nil {
		t.Fatalf("stale candidates: %v", err)
	}
	if len(cands) != 1 || cands[0].Todo.ID != fresh.ID || len(cands[0].Blockers) != 1 {
		t.Fatalf("candidates = %+v, want the held todo with its one ref", cands)
	}
	if cands[0].WebhookID != "" {
		t.Errorf("webhook id = %q, want empty for a todo with no event", cands[0].WebhookID)
	}
	// A hold refreshed just now is not stale again.
	if err := s.HoldRefresh(ctx, fresh.ID, []BlockerRef{ref("octo", "repo", 7)}); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if cands, err := s.TodosWithHolds(ctx, 5*time.Minute, 50); err != nil || len(cands) != 0 {
		t.Fatalf("after refresh candidates = %+v err %v, want none", cands, err)
	}
	// An orphan: released because every recorded ref closed, still claimable, and the link was deleted
	// by hand. It must be offered even though it was checked a moment ago, or it is never revisited.
	if _, err := s.MarkBlockerState(ctx, "octo", "repo", 7, "closed"); err != nil {
		t.Fatalf("close: %v", err)
	}
	cands, err = s.TodosWithHolds(ctx, 5*time.Minute, 50)
	if err != nil {
		t.Fatalf("orphan candidates: %v", err)
	}
	if len(cands) != 1 || cands[0].Todo.State != "pending" {
		t.Fatalf("orphan candidates = %+v, want the released row", cands)
	}
	allClosed := true
	for _, b := range cands[0].Blockers {
		if b.State != "closed" {
			allClosed = false
		}
	}
	if !allClosed {
		t.Errorf("orphan refs = %+v, want every ref closed", cands[0].Blockers)
	}
}

func TestBlockedTodosAreCountedAndCarryRefsInOneRead(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "hold-count", "inbox")
	held, _, err := s.CreateTodo(ctx, holdParams(ep, "inbox", "octo/repo#12", ref("octo", "repo", 7)))
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	plain, _, err := s.CreateTodo(ctx, CreateTodoParams{EndpointID: ep, Queue: "inbox", Source: "gitea",
		Kind: "push", Payload: []byte(`{}`), IdempotencyKey: "plain-1"})
	if err != nil {
		t.Fatalf("plain create: %v", err)
	}
	stats, err := s.QueueStats(ctx)
	if err != nil {
		t.Fatalf("queue stats: %v", err)
	}
	var blocked, total int64
	for _, q := range stats {
		if q.Queue == "inbox" {
			blocked = q.Blocked
			total = q.Pending + q.Claimed + q.Done + q.Failed + q.Blocked
		}
	}
	if blocked != 1 || total != 2 {
		t.Errorf("inbox stats blocked=%d total=%d, want 1 and 2", blocked, total)
	}
	byID, err := s.BlockersForTodos(ctx, []string{held.ID, plain.ID})
	if err != nil {
		t.Fatalf("blockers for todos: %v", err)
	}
	if len(byID[held.ID]) != 1 {
		t.Errorf("held refs = %+v, want one", byID[held.ID])
	}
	if _, ok := byID[plain.ID]; ok {
		t.Errorf("an unheld todo came back with refs: %+v", byID[plain.ID])
	}
	// The release path never leaves a held row behind when the refs are all closed.
	if _, err := s.ReleaseHeldTodo(ctx, plain.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("release of a pending todo = %v, want ErrNotFound", err)
	}
}
