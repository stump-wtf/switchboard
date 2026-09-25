package store

// Cross-tenant isolation for the OPERATOR read models and mutations.
//
// These exist because the operator web UI shipped single-tenant and stayed that way after
// Switchboard grew a second human. Every read below (`FROM todos` with no owner predicate) and
// every mutation (`WHERE id=$1` with no owner predicate) was global, so any signed-in human could
// read, and change, every other human's board — for seven weeks, across six real accounts, until
// an invited user reported seeing someone else's webhooks.
//
// The tests are written as "B must learn nothing and change nothing of A's" rather than "the query
// has a join", because the join is the current implementation and the property is the requirement.
// Each asserts on A's actual identifiers, never on a count alone: a count can match by accident,
// an id cannot.
//
// Governing: SPEC-0007 REQ "Human as Accountable Principal"; SPEC-0013 (operator board);
// ADR-0022 (endpoint-scoped todos).

import (
	"context"
	"errors"
	"testing"
	"time"
)

// contextT keeps the two helpers below readable next to the rest of this package's fixtures,
// which all take ctx as their third argument.
type contextT = context.Context

// tenants seeds two unrelated humans, each with an endpoint, and returns
// (humanA, endpointA, humanB, endpointB).
//
// Deliberately NO friend edge between them: friending is a real grant, and putting one in the
// default fixture would let a genuinely leaky read pass by looking authorised.
func tenants(t *testing.T, s *Store, ctx contextT) (string, string, string, string) {
	t.Helper()
	epA := seedEndpoint(t, s, ctx, "tenant-a", "q")
	epB := seedEndpoint(t, s, ctx, "tenant-b", "q")
	return ownerOf(t, s, ctx, epA), epA, ownerOf(t, s, ctx, epB), epB
}

// seedOwnedTodo creates one event-backed todo on an endpoint and returns it. The event is owned by
// the same endpoint, the way an operator push records it.
func seedOwnedTodo(t *testing.T, s *Store, ctx contextT, ep, key, title string) Todo {
	t.Helper()
	_, td, _, err := s.CreateEventTodo(ctx,
		EventInput{Source: "github", Family: "webhook", EventType: "push",
			ExternalID: key, TrustMode: "signed", Verified: true, Payload: []byte(`{"secret":"a-payload-only-A-may-read"}`),
			EndpointID: ep},
		CreateTodoParams{EndpointID: ep, Queue: "q", Source: "github", Kind: "push",
			Title: title, IdempotencyKey: key})
	if err != nil {
		t.Fatalf("seed todo %s: %v", key, err)
	}
	return td
}

// The listing must not carry another human's rows — asserted on A's todo id, so a reordered or
// truncated result cannot pass by coincidence.
func TestTenancyListTodoItemsExcludesOtherHumans(t *testing.T) {
	s, ctx := testStore(t)
	humanA, epA, humanB, epB := tenants(t, s, ctx)
	a := seedOwnedTodo(t, s, ctx, epA, "iso-a", "A's private PR title")
	b := seedOwnedTodo(t, s, ctx, epB, "iso-b", "B's own work")

	seen := func(human string) map[string]bool {
		items, err := s.ListTodoItems(ctx, human, "", "", 100)
		if err != nil {
			t.Fatalf("list for %s: %v", human, err)
		}
		out := map[string]bool{}
		for _, it := range items {
			out[it.ID] = true
		}
		return out
	}
	if got := seen(humanB); got[a.ID] {
		t.Fatalf("B's board contains A's todo %s", a.ID)
	}
	if got := seen(humanA); got[b.ID] {
		t.Fatalf("A's board contains B's todo %s", b.ID)
	}
	// Positive control: the scoping is not simply returning nothing.
	if got := seen(humanA); !got[a.ID] {
		t.Fatal("A cannot see A's own todo — the scope is too tight, not too loose")
	}
}

// Search is a second path into the same table and has its own filter clause, so it gets its own
// assertion: a text query must not reach across the boundary either.
func TestTenancySearchDoesNotReachAcrossTenants(t *testing.T) {
	s, ctx := testStore(t)
	_, epA, humanB, _ := tenants(t, s, ctx)
	a := seedOwnedTodo(t, s, ctx, epA, "iso-search", "A's private PR title")

	items, err := s.ListTodoItems(ctx, humanB, "", a.ID, 100)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("B searching A's exact todo id returned %d rows, want 0", len(items))
	}
}

// The drawer read must be indistinguishable-from-absent, not a permission error: ErrConflict or a
// 403 would confirm the id exists, which turns the drawer into an oracle for enumerating another
// tenant's id space even with the body withheld.
func TestTenancyGetTodoItemIsNotFoundNotForbidden(t *testing.T) {
	s, ctx := testStore(t)
	_, epA, humanB, _ := tenants(t, s, ctx)
	a := seedOwnedTodo(t, s, ctx, epA, "iso-drawer", "A's payload")

	_, err := s.GetTodoItem(ctx, humanB, a.ID)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("B reading A's todo = %v, want ErrNotFound (anything else confirms the id exists)", err)
	}
}

// Counts and tiles leak too, and are the easiest to forget because nothing in a number looks like
// somebody else's data.
func TestTenancyCountsAndStatsAreScoped(t *testing.T) {
	s, ctx := testStore(t)
	_, epA, humanB, epB := tenants(t, s, ctx)
	for _, k := range []string{"cnt-a1", "cnt-a2", "cnt-a3"} {
		seedOwnedTodo(t, s, ctx, epA, k, "A")
	}
	seedOwnedTodo(t, s, ctx, epB, "cnt-b1", "B")

	counts, err := s.TodoCounts(ctx, humanB)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if counts.All != 1 {
		t.Fatalf("B's total = %d, want 1 (A has three; the pill must not sum the instance)", counts.All)
	}
	stats, err := s.BoardStats(ctx, humanB)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.TotalTodos != 1 {
		t.Fatalf("B's tile = %d, want 1", stats.TotalTodos)
	}
}

// The events feed carries source, type and the linked todo's lease owner.
func TestTenancyEventFeedIsScoped(t *testing.T) {
	s, ctx := testStore(t)
	_, epA, humanB, _ := tenants(t, s, ctx)
	a := seedOwnedTodo(t, s, ctx, epA, "iso-feed", "A")

	evs, err := s.RecentEvents(ctx, humanB, 50)
	if err != nil {
		t.Fatalf("feed: %v", err)
	}
	for _, e := range evs {
		if e.TodoID == a.ID {
			t.Fatalf("B's feed carries A's todo: %+v", e)
		}
	}
	if len(evs) != 0 {
		t.Fatalf("B's feed has %d rows from a database where only A has deliveries", len(evs))
	}
	if a.EventID == nil {
		t.Fatal("fixture: seeded todo has no event")
	}
	if _, err := s.EventByID(ctx, humanB, *a.EventID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("B reading A's event = %v, want ErrNotFound", err)
	}
}

// Every operator mutation must refuse, AND leave the row untouched. A handler that refuses after
// writing is the failure mode a status-only assertion misses entirely.
func TestTenancyOperatorMutationsRefuseAndChangeNothing(t *testing.T) {
	s, ctx := testStore(t)
	humanA, epA, humanB, _ := tenants(t, s, ctx)
	a := seedOwnedTodo(t, s, ctx, epA, "iso-mut", "A's work")

	// A claims it, so the complete/fail/heartbeat/release paths have a live claim to attack.
	if _, err := s.ClaimTodoOperatorOwned(ctx, humanA, a.ID, "op:"+humanA, time.Hour); err != nil {
		t.Fatalf("A claims own todo: %v", err)
	}
	before, err := s.GetTodoOperatorOwned(ctx, humanA, a.ID)
	if err != nil {
		t.Fatalf("read before: %v", err)
	}

	attacks := map[string]func() (Todo, error){
		"claim": func() (Todo, error) { return s.ClaimTodoOperatorOwned(ctx, humanB, a.ID, "op:"+humanB, time.Hour) },
		"complete": func() (Todo, error) {
			return s.CompleteTodoOperatorOwned(ctx, humanB, a.ID, "op:"+humanA, []byte(`{}`))
		},
		"fail":      func() (Todo, error) { return s.FailTodoOperatorOwned(ctx, humanB, a.ID, "op:"+humanA, []byte(`{}`)) },
		"heartbeat": func() (Todo, error) { return s.HeartbeatTodoOperatorOwned(ctx, humanB, a.ID, "op:"+humanA, time.Hour) },
		"release":   func() (Todo, error) { return s.ReleaseTodoOperatorOwned(ctx, humanB, a.ID, "op:"+humanA) },
		"retry":     func() (Todo, error) { return s.RetryTodoOperatorOwned(ctx, humanB, a.ID) },
		"get":       func() (Todo, error) { return s.GetTodoOperatorOwned(ctx, humanB, a.ID) },
	}
	for name, attack := range attacks {
		t.Run(name, func(t *testing.T) {
			_, err := attack()
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("B's %s on A's todo = %v, want ErrNotFound "+
					"(ErrConflict would confirm the id exists)", name, err)
			}
			after, err := s.GetTodoOperatorOwned(ctx, humanA, a.ID)
			if err != nil {
				t.Fatalf("read after %s: %v", name, err)
			}
			if after.State != before.State || after.Owner != before.Owner || after.Attempt != before.Attempt {
				t.Fatalf("B's %s changed A's todo: state %q→%q owner %q→%q attempt %d→%d",
					name, before.State, after.State, before.Owner, after.Owner, before.Attempt, after.Attempt)
			}
		})
	}
}

// A todo with no endpoint belongs to nobody, so it is visible to nobody. An unowned row is an
// ingestion bug; defaulting it to "everyone" is the failure this whole change exists to remove.
func TestTenancyUnownedTodoIsVisibleToNobody(t *testing.T) {
	s, ctx := testStore(t)
	humanA, epA, humanB, _ := tenants(t, s, ctx)
	orphan := seedOwnedTodo(t, s, ctx, epA, "iso-orphan", "about to be orphaned")
	if _, err := s.pool.Exec(ctx, `UPDATE todos SET endpoint_id = NULL WHERE id = $1`, orphan.ID); err != nil {
		t.Skipf("endpoint_id is NOT NULL in this schema, so an orphan cannot exist: %v", err)
	}
	for _, human := range []string{humanA, humanB} {
		if _, err := s.GetTodoItem(ctx, human, orphan.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("orphaned todo visible to %s: %v", human, err)
		}
	}
}

// The event-history reads behind list_webhook_events, get_webhook_event, replay_webhook_event and
// switchboard://events/recent (#194, audit F1). B must list none of A's events and read A's by id as
// ErrNotFound, identical to an id that does not exist; an event with no owner is read by nobody.
// Governing: ADR-0038, SPEC-0033 REQ "Owner-Scoped History Reads".
func TestTenancyEventHistoryIsScoped(t *testing.T) {
	s, ctx := testStore(t)
	humanA, epA, humanB, epB := tenants(t, s, ctx)
	callerA := AuthEndpoint{ID: epA, OwnerHumanID: humanA}
	callerB := AuthEndpoint{ID: epB, OwnerHumanID: humanB}
	a := seedOwnedTodo(t, s, ctx, epA, "iso-history", "A")
	if a.EventID == nil {
		t.Fatal("fixture: seeded todo has no event")
	}

	items, err := s.ListEventHistory(ctx, callerB, EventHistoryFilter{Limit: 200})
	if err != nil {
		t.Fatalf("B lists: %v", err)
	}
	for _, it := range items {
		if it.ID == *a.EventID {
			t.Fatalf("B's history carries A's event %d", it.ID)
		}
	}
	if _, err := s.EventHistoryByID(ctx, callerB, *a.EventID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("B reading A's event = %v, want ErrNotFound", err)
	}
	if d, err := s.EventHistoryByID(ctx, callerA, *a.EventID); err != nil || d.ID != *a.EventID {
		t.Fatalf("A reading its own event = %+v, %v", d.EventHistoryItem, err)
	}

	// An unscoped caller (empty or malformed owner or endpoint) gets nothing, never an unfiltered
	// scan; nor does a caller that pairs A as the owner with B's endpoint.
	for _, c := range []AuthEndpoint{
		{}, {ID: epA}, {OwnerHumanID: humanA}, {ID: "not-a-uuid", OwnerHumanID: humanA},
		{ID: epA, OwnerHumanID: "not-a-uuid"}, {ID: epB, OwnerHumanID: humanA},
	} {
		if items, err := s.ListEventHistory(ctx, c, EventHistoryFilter{}); err != nil || len(items) != 0 {
			t.Fatalf("ListEventHistory(%+v) = %d rows, %v; want none", c, len(items), err)
		}
		if _, err := s.EventHistoryByID(ctx, c, *a.EventID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("EventHistoryByID(%+v) = %v, want ErrNotFound", c, err)
		}
	}

	// Owner-less: visible to nobody, A included.
	if _, err := s.pool.Exec(ctx, `UPDATE events SET endpoint_id = NULL WHERE id = $1`, *a.EventID); err != nil {
		t.Fatalf("orphan event: %v", err)
	}
	for _, c := range []AuthEndpoint{callerA, callerB} {
		if _, err := s.EventHistoryByID(ctx, c, *a.EventID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("owner-less event visible to %s: %v", c.OwnerHumanID, err)
		}
	}
}

// A friend approval mints the remote peer's endpoint onto one of the APPROVER's agents, so by
// owner alone it would read the approver's whole history. A friend request that asked for the
// event verbs, approved with no narrowing, must still read none of it: not by list, and not by id.
// The approver's own endpoint on the same agent keeps its reach. Until #420 gives friend endpoints
// their own authority, their history reach is empty.
// Governing: ADR-0038, SPEC-0033 REQ "Owner-Scoped History Reads", REQ "Closing the Audited
// Surfaces" (F3).
func TestTenancyFriendEndpointReadsNoHistory(t *testing.T) {
	s, ctx := testStore(t)
	humanA, epA, _, _ := tenants(t, s, ctx)
	var agentA string
	if err := s.pool.QueryRow(ctx, `SELECT agent_id::text FROM endpoints WHERE id = $1`, epA).Scan(&agentA); err != nil {
		t.Fatalf("agent of A's endpoint: %v", err)
	}
	a := seedOwnedTodo(t, s, ctx, epA, "iso-friend-history", "A")
	if a.EventID == nil {
		t.Fatal("fixture: seeded todo has no event")
	}

	_, friendEP := mustApprovedFriend(t, s, ctx, Human{ID: humanA}, Agent{ID: agentA, Name: "tenant-a"},
		[]string{"q"}, []string{"create_for", "list_webhook_events", "get_webhook_event", "replay_webhook_event"})
	friend := AuthEndpoint{ID: friendEP.ID, OwnerHumanID: humanA} // exactly what auth resolves for it

	items, err := s.ListEventHistory(ctx, friend, EventHistoryFilter{Limit: 200})
	if err != nil || len(items) != 0 {
		t.Fatalf("friend endpoint listed %d of the approver's events (%v), want none", len(items), err)
	}
	if _, err := s.EventHistoryByID(ctx, friend, *a.EventID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("friend endpoint reading the approver's event = %v, want ErrNotFound", err)
	}
	// Positive control: the approver's own endpoint still reads it.
	if d, err := s.EventHistoryByID(ctx, AuthEndpoint{ID: epA, OwnerHumanID: humanA}, *a.EventID); err != nil || d.ID != *a.EventID {
		t.Fatalf("A's own endpoint lost its event: %+v, %v", d.EventHistoryItem, err)
	}
}

// F14: two owners' deliveries sharing (source, external_id) are two events; neither is answered
// with the other's, and each owner still dedups its own redelivery.
// Governing: SPEC-0033 REQ "Closing the Audited Surfaces", scenario "Dedup cannot cross owners".
func TestTenancyEventDedupCannotCrossOwners(t *testing.T) {
	s, ctx := testStore(t)
	_, epA, _, epB := tenants(t, s, ctx)
	in := func(ep string) EventInput {
		return EventInput{Source: "github", Family: "webhook", ExternalID: "shared-delivery",
			TrustMode: "signed", Verified: true, Payload: []byte(`{}`), EndpointID: ep}
	}
	idA, err := s.InsertEvent(ctx, in(epA))
	if err != nil {
		t.Fatalf("A: %v", err)
	}
	idB, err := s.InsertEvent(ctx, in(epB))
	if err != nil {
		t.Fatalf("B: %v", err)
	}
	if idA == idB {
		t.Fatalf("B's delivery was answered with A's event %d", idA)
	}
	again, err := s.InsertEvent(ctx, in(epA))
	if err != nil || again != idA {
		t.Fatalf("A's redelivery = %d (%v), want its own event %d", again, err, idA)
	}
}

// The board must filter the way history does (#194). Since F14, two owners can each hold an event
// with the same external id, and the board's dedup relation (external_id ↔ idempotency_key) must
// not join across them: B's feed, EventByID, LIVE rate and dedup badge carry nothing of A's.
// Governing: ADR-0038, SPEC-0033 REQ "Owner-Scoped History Reads", REQ "Closing the Audited
// Surfaces" scenario "Dedup cannot cross owners (F14)".
func TestTenancyBoardDedupCannotCrossOwners(t *testing.T) {
	s, ctx := testStore(t)
	humanA, epA, humanB, epB := tenants(t, s, ctx)
	a := seedOwnedTodo(t, s, ctx, epA, "shared-delivery", "A")
	b := seedOwnedTodo(t, s, ctx, epB, "shared-delivery", "B")
	if a.EventID == nil || b.EventID == nil || *a.EventID == *b.EventID {
		t.Fatalf("fixture: want two events, got A %v and B %v", a.EventID, b.EventID)
	}

	evs, err := s.RecentEvents(ctx, humanB, 50)
	if err != nil {
		t.Fatalf("B's feed: %v", err)
	}
	if len(evs) != 1 || evs[0].ID != *b.EventID || evs[0].Deduped {
		t.Fatalf("B's feed = %+v, want only B's event %d, not deduped", evs, *b.EventID)
	}
	if _, err := s.EventByID(ctx, humanB, *a.EventID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("B reading A's same-key event = %v, want ErrNotFound", err)
	}
	stats, err := s.BoardStats(ctx, humanB)
	if err != nil {
		t.Fatalf("B's stats: %v", err)
	}
	if stats.EventsPerMin != 1 {
		t.Fatalf("B's LIVE rate = %d, want 1 (A's same-key delivery counted)", stats.EventsPerMin)
	}
	items, err := s.ListTodoItems(ctx, humanB, "", "", 100)
	if err != nil || len(items) != 1 {
		t.Fatalf("B's todos = %d (%v), want 1", len(items), err)
	}
	if items[0].DedupCount != 1 {
		t.Fatalf("B's dedup count = %d, want 1 (A's delivery counted)", items[0].DedupCount)
	}
	if it, err := s.GetTodoItem(ctx, humanA, a.ID); err != nil || it.DedupCount != 1 {
		t.Fatalf("A's drawer dedup count = %d (%v), want 1", it.DedupCount, err)
	}
}

// A friend-route recipient keeps its deduped redeliveries on the board: the delivery is owned by the
// sender's endpoint and lands on the recipient's, and a second delivery of the same key (from
// another source) collapses onto the recipient's todo. The recipient's feed flags it deduped and
// its badge counts both, while the sender, who holds no todo for it, sees neither event.
func TestTenancyBoardDedupFollowsFriendRoute(t *testing.T) {
	s, ctx := testStore(t)
	humanA, epA, humanB, epB := tenants(t, s, ctx)
	deliver := func(source string) int64 {
		t.Helper()
		id, _, err := s.CreateEventTodos(ctx, EventInput{Source: source, Family: "webhook", EventType: "push",
			ExternalID: "routed-key", TrustMode: "signed", Verified: true, Payload: []byte(`{}`), EndpointID: epA},
			[]string{epB}, CreateTodoParams{Queue: "q", Source: source, Kind: "push", Title: "t", IdempotencyKey: "routed-key"})
		if err != nil {
			t.Fatalf("deliver %s: %v", source, err)
		}
		return id
	}
	first, second := deliver("github"), deliver("gitea")
	if first == second {
		t.Fatalf("fixture: want two events, got %d twice", first)
	}

	evs, err := s.RecentEvents(ctx, humanB, 50)
	if err != nil {
		t.Fatalf("B's feed: %v", err)
	}
	var sawSecondDeduped bool
	for _, e := range evs {
		if e.ID == second && e.Deduped {
			sawSecondDeduped = true
		}
	}
	if len(evs) != 2 || !sawSecondDeduped {
		t.Fatalf("recipient's feed = %+v, want both events and %d flagged deduped", evs, second)
	}
	items, err := s.ListTodoItems(ctx, humanB, "", "", 100)
	if err != nil || len(items) != 1 || items[0].DedupCount != 2 {
		t.Fatalf("recipient's todos = %+v (%v), want one todo with dedup count 2", items, err)
	}
	if evs, err := s.RecentEvents(ctx, humanA, 50); err != nil || len(evs) != 0 {
		t.Fatalf("sender's feed = %+v (%v), want nothing: it holds no todo for the delivery", evs, err)
	}
}

// SPEC-0033: an event whose owner cannot be established is invisible to agents AND to the board,
// even to the holder of the todo it produced.
func TestTenancyBoardHidesOwnerlessEvents(t *testing.T) {
	s, ctx := testStore(t)
	humanA, epA, _, _ := tenants(t, s, ctx)
	a := seedOwnedTodo(t, s, ctx, epA, "iso-ownerless-board", "A")
	if a.EventID == nil {
		t.Fatal("fixture: seeded todo has no event")
	}
	// Positive control: owned, it is on A's board.
	if _, err := s.EventByID(ctx, humanA, *a.EventID); err != nil {
		t.Fatalf("A reading its owned event = %v", err)
	}

	if _, err := s.pool.Exec(ctx, `UPDATE events SET endpoint_id = NULL WHERE id = $1`, *a.EventID); err != nil {
		t.Fatalf("orphan event: %v", err)
	}
	if _, err := s.EventByID(ctx, humanA, *a.EventID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("owner-less event on A's board = %v, want ErrNotFound", err)
	}
	evs, err := s.RecentEvents(ctx, humanA, 50)
	if err != nil {
		t.Fatalf("A's feed: %v", err)
	}
	for _, e := range evs {
		if e.ID == *a.EventID {
			t.Fatalf("owner-less event %d is in A's feed", e.ID)
		}
	}
	if stats, err := s.BoardStats(ctx, humanA); err != nil || stats.EventsPerMin != 0 {
		t.Fatalf("A's LIVE rate = %d (%v), want 0", stats.EventsPerMin, err)
	}
}
