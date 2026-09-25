package store

// The headline regression suite for the cross-tenant todo leak ADR-0022 closes.
//
// Before endpoint ownership, `todos` carried a free-form global `queue` string and no owner. Two
// endpoints belonging to two DIFFERENT humans that happened to pick the same queue name — and
// "reviews" and "github" are the names everyone picks — shared full visibility into each other's
// work: B's list_todos returned A's todos, B could claim them, and A's delivery rang B's doorbell.
// These tests reconstruct exactly that arrangement (two humans, two agents, two vended endpoints,
// one shared queue name) and assert the boundary from BOTH sides: A's own work flows end to end,
// and every caller-facing entry point refuses B.
//
// Every unhappy assertion here is deliberately paired with a happy one on the same call in the same
// test. A regression that broke scoping open would fail the unhappy half; a regression that broke
// scoping SHUT (every query returning nothing) would fail the happy half. Neither can pass vacuously.
//
// Governing: ADR-0022, ADR-0008; SPEC-0003 REQ "Endpoint Ownership (Tenant Isolation)",
// SPEC-0003 REQ "Per-Endpoint Idempotency and Dedup".

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// tenantIsoSharedQueueNames are the collision cases from the reported bug: the queue labels two unrelated
// tenants independently choose. They are covered as separate sub-tests rather than one, so a scope
// check that special-cased a single name could not hide.
var tenantIsoSharedQueueNames = []string{"reviews", "github"}

// tenantIsoFixture is the two-tenant arrangement under test: two distinct humans, each with their
// own agent and vended endpoint, both scoped to the SAME queue name.
type tenantIsoFixture struct {
	queue    string
	epA, epB string
	humanA   string
	humanB   string
}

// newTenantIsoFixture seeds the collision. It asserts up front that the two endpoints really do
// resolve to two different humans — the whole point is a boundary ACROSS humans, and a fixture bug
// that handed back one human twice would make every assertion below meaningless.
// Governing: ADR-0008 (the endpoint's owning human is the tenant), ADR-0022.
func newTenantIsoFixture(t *testing.T, s *Store, ctx context.Context, queue string) tenantIsoFixture {
	t.Helper()
	epA := seedEndpoint(t, s, ctx, "tenant-a-"+queue, queue)
	epB := seedEndpoint(t, s, ctx, "tenant-b-"+queue, queue)
	if epA == epB {
		t.Fatalf("fixture seeded one endpoint twice (%s): there is no tenant boundary to test", epA)
	}
	humanA, err := s.EndpointOwner(ctx, epA)
	if err != nil {
		t.Fatalf("endpoint owner A: %v", err)
	}
	humanB, err := s.EndpointOwner(ctx, epB)
	if err != nil {
		t.Fatalf("endpoint owner B: %v", err)
	}
	if humanA == humanB {
		t.Fatalf("both endpoints resolve to human %s: this is a same-tenant fixture, not a cross-tenant one", humanA)
	}
	return tenantIsoFixture{queue: queue, epA: epA, epB: epB, humanA: humanA, humanB: humanB}
}

// seedTenantIsoTodo mints a todo the way a real accepted webhook delivery does — an inbound event
// that passed verification, committed atomically with its todo — so the row is eligible for the
// channel doorbell as well as the pull path. Governing: SPEC-0001 REQ "Enqueue Accepted Delivery
// as Endpoint-Owned Todo", SPEC-0011 REQ "Sender Gate and Injection Safety".
func seedTenantIsoTodo(t *testing.T, s *Store, ctx context.Context, endpointID, queue, externalID, title string) Todo {
	t.Helper()
	_, td, created, err := s.CreateEventTodo(ctx,
		EventInput{Source: "github", Family: "webhook", EventType: "push", ExternalID: externalID,
			TrustMode: "signed", Verified: true, Payload: []byte("raw")},
		CreateTodoParams{EndpointID: endpointID, Queue: queue, Source: "github", Kind: "push",
			Title: title, Payload: []byte(`{}`), IdempotencyKey: externalID})
	if err != nil || !created {
		t.Fatalf("seed verified todo %s: created=%v err=%v", externalID, created, err)
	}
	if td.EndpointID != endpointID {
		t.Fatalf("seeded todo %s pinned to %q, want %q", td.ID, td.EndpointID, endpointID)
	}
	return td
}

// tenantIsoIDs projects a todo slice to its ids so list assertions can compare sets by identity rather
// than by length alone — a length check would pass if the store returned the RIGHT COUNT of the
// WRONG tenant's rows.
func tenantIsoIDs(ts []Todo) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.ID)
	}
	return out
}

func tenantIsoHasID(ts []Todo, id string) bool {
	for _, t := range ts {
		if t.ID == id {
			return true
		}
	}
	return false
}

// TestTenantIsolationAcrossHumansOnASharedQueueName is the reported bug, reconstructed.
//
// Two humans, two agents, two vended endpoints, one queue name. A's work must flow (list, claim,
// heartbeat, complete/fail/retry, doorbell) and B must be unable to see, claim, or even PROBE it.
//
// Governing: SPEC-0003 REQ "Endpoint Ownership (Tenant Isolation)", scenarios "Cross-endpoint queue
// collision is isolated" and "A foreign todo id is indistinguishable from a nonexistent one".
func TestTenantIsolationAcrossHumansOnASharedQueueName(t *testing.T) {
	for _, queue := range tenantIsoSharedQueueNames {
		t.Run(queue, func(t *testing.T) {
			s, ctx := testStore(t)
			f := newTenantIsoFixture(t, s, ctx, queue)

			// The doorbell hook is the seam the MCP channel push hangs off (store.SetTodoDoorbellHook
			// → mcp.PublishTodoReady). Recording the endpoint id it carries proves the push path is
			// handed the tenant key it needs to filter sessions on; a hook that fired with a blank or
			// wrong endpoint would fan A's delivery to every session on the queue name.
			type ring struct{ todoID, endpointID string }
			var rings []ring
			s.SetTodoDoorbellHook(func(td Todo) { rings = append(rings, ring{td.ID, td.EndpointID}) })
			defer s.SetTodoDoorbellHook(nil)

			aPending := seedTenantIsoTodo(t, s, ctx, f.epA, queue, "a-pending", "A's pending work")
			aClaimed := seedTenantIsoTodo(t, s, ctx, f.epA, queue, "a-claimed", "A's claimed work")
			bOwn := seedTenantIsoTodo(t, s, ctx, f.epB, queue, "b-own", "B's own work")

			// --- HAPPY: the doorbell rings, once per todo, naming the OWNING endpoint. ---
			want := []ring{{aPending.ID, f.epA}, {aClaimed.ID, f.epA}, {bOwn.ID, f.epB}}
			if len(rings) != len(want) {
				t.Fatalf("doorbell rang %d times (%+v), want %d", len(rings), rings, len(want))
			}
			for i, w := range want {
				if rings[i] != w {
					t.Fatalf("doorbell %d = %+v, want %+v (a todo must ring for its own endpoint only)", i, rings[i], w)
				}
			}

			// The push-eligible set the todo_ready listener reads is likewise per-tenant: A's nudge
			// carries only A's rows even though B's row sits on the same queue name.
			gotA, err := s.PendingDoorbellTodos(ctx, f.epA, queue, 10)
			if err != nil {
				t.Fatalf("pending doorbell todos A: %v", err)
			}
			if len(gotA) != 2 || !tenantIsoHasID(gotA, aPending.ID) || !tenantIsoHasID(gotA, aClaimed.ID) {
				t.Fatalf("A's doorbell set = %v, want exactly [%s %s]", tenantIsoIDs(gotA), aPending.ID, aClaimed.ID)
			}
			if tenantIsoHasID(gotA, bOwn.ID) {
				t.Fatalf("A's doorbell set leaked B's todo %s: %v", bOwn.ID, tenantIsoIDs(gotA))
			}
			gotB, err := s.PendingDoorbellTodos(ctx, f.epB, queue, 10)
			if err != nil {
				t.Fatalf("pending doorbell todos B: %v", err)
			}
			if len(gotB) != 1 || gotB[0].ID != bOwn.ID {
				t.Fatalf("B's doorbell set = %v, want exactly [%s]", tenantIsoIDs(gotB), bOwn.ID)
			}

			// --- HAPPY: A lists, claims, heartbeats, and reads back its own work. ---
			listA, err := s.ListTodos(ctx, f.epA, []string{queue}, "", 50)
			if err != nil {
				t.Fatalf("list A: %v", err)
			}
			if len(listA) != 2 || !tenantIsoHasID(listA, aPending.ID) || !tenantIsoHasID(listA, aClaimed.ID) {
				t.Fatalf("A's list = %v, want exactly A's two todos", tenantIsoIDs(listA))
			}

			if _, err := s.ClaimTodo(ctx, f.epA, aClaimed.ID, "agent-a", time.Hour); err != nil {
				t.Fatalf("A claiming its own todo must succeed: %v", err)
			}
			if _, err := s.HeartbeatTodo(ctx, f.epA, aClaimed.ID, "agent-a", time.Hour); err != nil {
				t.Fatalf("A heartbeating its own claim must succeed: %v", err)
			}
			if got, err := s.GetTodo(ctx, f.epA, aClaimed.ID); err != nil || got.ID != aClaimed.ID {
				t.Fatalf("A reading its own todo: got %+v err=%v", got, err)
			}
			// ...and its attempt history (get_todo's read), whose open attempt the foreign probe
			// below must not reveal. Governing: SPEC-0034 REQ-10.
			if as, total, _, err := s.TodoAttempts(ctx, f.epA, aClaimed.ID, 20); err != nil ||
				len(as) != 1 || total != 1 || as[0].EndedAt != nil {
				t.Fatalf("A reading its own attempts: %d attempts, total %d, err=%v; want one open attempt", len(as), total, err)
			}

			// --- UNHAPPY: B's pull path cannot see A's work. ---
			listB, err := s.ListTodos(ctx, f.epB, []string{queue}, "", 50)
			if err != nil {
				t.Fatalf("list B: %v", err)
			}
			if len(listB) != 1 || listB[0].ID != bOwn.ID {
				t.Fatalf("B's list = %v, want exactly its own todo %s — cross-tenant leak on queue %q",
					tenantIsoIDs(listB), bOwn.ID, queue)
			}
			// Same assertion with the state filter B's agent would actually use: a pending-only list
			// must not surface A's pending row either.
			pendingB, err := s.ListTodos(ctx, f.epB, []string{queue}, "pending", 50)
			if err != nil {
				t.Fatalf("list B (pending): %v", err)
			}
			if tenantIsoHasID(pendingB, aPending.ID) {
				t.Fatalf("B's pending list leaked A's pending todo %s: %v", aPending.ID, tenantIsoIDs(pendingB))
			}

			// --- UNHAPPY: B cannot DRAIN A's queue either. ---
			// B's own todo is claimable, so the first ClaimNext must succeed (proving the scope is not
			// simply broken shut); the second must find nothing, even though A's pending todo is
			// sitting on the very same queue name, unclaimed and oldest-first.
			drained, err := s.ClaimNext(ctx, f.epB, []string{queue}, "agent-b", time.Hour)
			if err != nil {
				t.Fatalf("B claiming next from its own queue must succeed: %v", err)
			}
			if drained.ID != bOwn.ID {
				t.Fatalf("B's ClaimNext returned %s, want its own todo %s — it drained another tenant", drained.ID, bOwn.ID)
			}
			if _, err := s.ClaimNext(ctx, f.epB, []string{queue}, "agent-b", time.Hour); !errors.Is(err, ErrNotFound) {
				t.Fatalf("B's second ClaimNext err = %v, want ErrNotFound — A's pending todo is not B's work", err)
			}

			// --- UNHAPPY: the existence oracle. ---
			// aClaimed is claimed by agent-a. If the endpoint predicate were dropped from these
			// statements, the row WOULD be found and the state/owner guard would reject it as
			// ErrConflict. So ErrConflict here is precisely the signature of a missing tenant check,
			// and it would let B binary-search which todo ids A owns. Every one of these must be
			// ErrNotFound, indistinguishable from an id that was never minted.
			// Governing: SPEC-0003 scenario "A foreign todo id is indistinguishable from a
			// nonexistent one".
			const neverMinted = "td_never_minted"
			foreign := []struct {
				name string
				call func(id string) error
			}{
				{"GetTodo", func(id string) error { _, err := s.GetTodo(ctx, f.epB, id); return err }},
				{"ClaimTodo", func(id string) error {
					_, err := s.ClaimTodo(ctx, f.epB, id, "agent-b", time.Hour)
					return err
				}},
				{"CompleteTodo", func(id string) error {
					_, err := s.CompleteTodo(ctx, f.epB, id, "agent-a", []byte(`{}`))
					return err
				}},
				{"FailTodo", func(id string) error {
					_, err := s.FailTodo(ctx, f.epB, id, "agent-a", []byte(`{}`))
					return err
				}},
				{"HeartbeatTodo", func(id string) error {
					_, err := s.HeartbeatTodo(ctx, f.epB, id, "agent-a", time.Hour)
					return err
				}},
				{"ReleaseTodo", func(id string) error {
					_, err := s.ReleaseTodo(ctx, f.epB, id, "agent-a")
					return err
				}},
				{"RetryTodo", func(id string) error { _, err := s.RetryTodo(ctx, f.epB, id); return err }},
				// get_todo's history read: a foreign todo's attempts are not_found, never an empty list
				// that would confirm the id. Governing: SPEC-0034 REQ-10 "Foreign todo".
				{"TodoAttempts", func(id string) error {
					_, _, _, err := s.TodoAttempts(ctx, f.epB, id, 20)
					return err
				}},
			}
			// The owner strings above are deliberately A's ("agent-a") on the transitions that check
			// ownership: B guessing the right owner must STILL be refused, so the refusal cannot be
			// mistaken for the owner guard doing the work.
			for _, tc := range foreign {
				foreignErr := tc.call(aClaimed.ID)
				if errors.Is(foreignErr, ErrConflict) {
					t.Fatalf("%s from endpoint B on A's todo returned ErrConflict — that CONFIRMS the todo "+
						"exists and is an existence oracle across tenants; want ErrNotFound", tc.name)
				}
				if !errors.Is(foreignErr, ErrNotFound) {
					t.Fatalf("%s from endpoint B on A's todo err = %v, want ErrNotFound", tc.name, foreignErr)
				}
				// The nonexistent id must produce the byte-identical outcome, which is what makes the
				// two indistinguishable to a probing caller.
				missingErr := tc.call(neverMinted)
				if !errors.Is(missingErr, ErrNotFound) {
					t.Fatalf("%s on a never-minted id err = %v, want ErrNotFound", tc.name, missingErr)
				}
				if foreignErr.Error() != missingErr.Error() {
					t.Fatalf("%s: foreign-id error %q differs from never-minted error %q — the difference "+
						"itself discloses that A owns that id", tc.name, foreignErr, missingErr)
				}
			}
			// A's pending todo is refused the same way: an unclaimed row is the case where a missing
			// endpoint predicate would not merely disclose the todo but hand B the claim.
			if _, err := s.ClaimTodo(ctx, f.epB, aPending.ID, "agent-b", time.Hour); !errors.Is(err, ErrNotFound) {
				t.Fatalf("B claiming A's PENDING todo err = %v, want ErrNotFound", err)
			}
			if got, err := s.GetTodo(ctx, f.epB, aPending.ID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("B reading A's pending todo = %+v err=%v, want ErrNotFound", got, err)
			}
			// ...and it is still A's, still pending, still claimable by A afterwards. The refusals
			// above must not have consumed an attempt or moved the row.
			after, err := s.GetTodo(ctx, f.epA, aPending.ID)
			if err != nil {
				t.Fatalf("A re-reading its pending todo: %v", err)
			}
			if after.State != "pending" || after.Attempt != 0 || after.Owner != "" {
				t.Fatalf("B's refused calls mutated A's todo: state=%q attempt=%d owner=%q",
					after.State, after.Attempt, after.Owner)
			}

			// --- CONTROL: ErrConflict is reachable on this exact row, from the RIGHT endpoint. ---
			// Without this, every ErrNotFound above could be explained by classifyMiss never returning
			// ErrConflict at all, and the assertions would be vacuous.
			if _, err := s.CompleteTodo(ctx, f.epA, aClaimed.ID, "someone-else", []byte(`{}`)); !errors.Is(err, ErrConflict) {
				t.Fatalf("A completing its own claimed todo as the WRONG owner err = %v, want ErrConflict "+
					"(control: the store must be capable of distinguishing exists-but-not-yours)", err)
			}

			// --- HAPPY: A drives its own todo to a terminal state and retries it. ---
			failed, err := s.FailTodo(ctx, f.epA, aClaimed.ID, "agent-a", []byte(`{"err":"boom"}`))
			if err != nil {
				t.Fatalf("A failing its own claim must succeed: %v", err)
			}
			if failed.State != "failed" {
				t.Fatalf("failed todo state = %q, want failed", failed.State)
			}
			// A failed row is retryable — and only by its owner. B must not be able to resurrect it.
			if _, err := s.RetryTodo(ctx, f.epB, aClaimed.ID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("B retrying A's failed todo err = %v, want ErrNotFound", err)
			}
			if _, err := s.RetryTodo(ctx, f.epA, aClaimed.ID); err != nil {
				t.Fatalf("A retrying its own failed todo must succeed: %v", err)
			}
			// And B still cannot claim it now that it is pending again.
			if _, err := s.ClaimTodo(ctx, f.epB, aClaimed.ID, "agent-b", time.Hour); !errors.Is(err, ErrNotFound) {
				t.Fatalf("B claiming A's re-queued todo err = %v, want ErrNotFound", err)
			}
		})
	}
}

// TestTenantIsolationRejectsBrokenScopeCleanly: an absent or malformed endpoint id owns nothing.
//
// endpoint_id is a uuid column, so an empty or garbage Go string reaches SQL as an invalid uuid
// literal and Postgres answers 22P02 ("invalid input syntax for type uuid"). That is not merely an
// ugly error — it is its own disclosure channel, because it distinguishes "your scope is broken"
// from "no such row" on the exact paths whose purpose is to make foreign and nonexistent
// indistinguishable. A broken scope MUST look like a scope that simply owns nothing.
//
// Governing: ADR-0022; SPEC-0003 REQ "Endpoint Ownership (Tenant Isolation)" — "An absent or
// malformed endpoint_id MUST behave as a scope that owns nothing ... and MUST NOT surface as a
// database type error."
func TestTenantIsolationRejectsBrokenScopeCleanly(t *testing.T) {
	s, ctx := testStore(t)
	f := newTenantIsoFixture(t, s, ctx, "reviews")

	live := seedTenantIsoTodo(t, s, ctx, f.epA, "reviews", "scope-live", "A's work")
	if _, err := s.ClaimTodo(ctx, f.epA, live.ID, "agent-a", time.Hour); err != nil {
		t.Fatalf("seed claim: %v", err)
	}

	// Empty, non-uuid, SQL-shaped, and a uuid-with-trailing-junk: all must land on the same clean miss.
	badScopes := []string{"", "not-a-uuid", "' OR '1'='1", f.epA + "x", "   "}

	assertCleanMiss := func(t *testing.T, scope, name string, err error) {
		t.Helper()
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			t.Fatalf("%s with scope %q surfaced a Postgres error %s (%s) instead of a clean miss",
				name, scope, pgErr.Code, pgErr.Message)
		}
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s with scope %q err = %v, want ErrNotFound", name, scope, err)
		}
	}

	for _, scope := range badScopes {
		// List paths: a scope owning nothing is an EMPTY LIST and a nil error, not a type error.
		got, err := s.ListTodos(ctx, scope, []string{"reviews"}, "", 50)
		if err != nil {
			t.Fatalf("ListTodos with scope %q: %v", scope, err)
		}
		if len(got) != 0 {
			t.Fatalf("ListTodos with scope %q returned %v — a broken scope must own nothing, "+
				"not fall back to an unscoped query", scope, tenantIsoIDs(got))
		}
		none, err := s.PendingDoorbellTodos(ctx, scope, "reviews", 50)
		if err != nil {
			t.Fatalf("PendingDoorbellTodos with scope %q: %v", scope, err)
		}
		if len(none) != 0 {
			t.Fatalf("PendingDoorbellTodos with scope %q returned %v, want none", scope, tenantIsoIDs(none))
		}

		// Single-row and mutation paths: ErrNotFound, never a driver error, never ErrConflict.
		_, err = s.GetTodo(ctx, scope, live.ID)
		assertCleanMiss(t, scope, "GetTodo", err)
		_, err = s.ClaimTodo(ctx, scope, live.ID, "agent-x", time.Hour)
		assertCleanMiss(t, scope, "ClaimTodo", err)
		_, err = s.ClaimNext(ctx, scope, []string{"reviews"}, "agent-x", time.Hour)
		assertCleanMiss(t, scope, "ClaimNext", err)
		_, err = s.CompleteTodo(ctx, scope, live.ID, "agent-a", []byte(`{}`))
		assertCleanMiss(t, scope, "CompleteTodo", err)
		_, err = s.FailTodo(ctx, scope, live.ID, "agent-a", []byte(`{}`))
		assertCleanMiss(t, scope, "FailTodo", err)
		_, err = s.HeartbeatTodo(ctx, scope, live.ID, "agent-a", time.Hour)
		assertCleanMiss(t, scope, "HeartbeatTodo", err)
		_, err = s.ReleaseTodo(ctx, scope, live.ID, "agent-a")
		assertCleanMiss(t, scope, "ReleaseTodo", err)
		_, err = s.RetryTodo(ctx, scope, live.ID)
		assertCleanMiss(t, scope, "RetryTodo", err)
		_, _, _, err = s.TodoAttempts(ctx, scope, live.ID, 20)
		assertCleanMiss(t, scope, "TodoAttempts", err)
	}

	// The row is untouched and A can still work it — proof the broken-scope refusals above are
	// refusals, not a store that has stopped functioning.
	if got, err := s.GetTodo(ctx, f.epA, live.ID); err != nil || got.State != "claimed" {
		t.Fatalf("A's todo after the broken-scope sweep: %+v err=%v, want a live claim", got, err)
	}
	if _, err := s.CompleteTodo(ctx, f.epA, live.ID, "agent-a", []byte(`{}`)); err != nil {
		t.Fatalf("A completing its own claim after the sweep must succeed: %v", err)
	}
}

// TestTenantIsolationDedupNamespaceIsPerEndpoint: the idempotency namespace is per-endpoint.
//
// The dedup index was once global on (queue, idempotency_key), so one GitHub delivery id fanned to
// two endpoints on the queue name "github" collapsed into a SINGLE todo — whichever tenant raced in
// first got the work and the other silently got nothing. Worse, a hostile tenant could suppress a
// neighbour's work simply by minting a todo with a guessable delivery id on a shared queue name.
//
// Governing: ADR-0022; SPEC-0003 REQ "Per-Endpoint Idempotency and Dedup", scenarios "Same delivery
// routed to two endpoints yields two todos" and "Redelivery to one target dedups only within that
// target".
func TestTenantIsolationDedupNamespaceIsPerEndpoint(t *testing.T) {
	s, ctx := testStore(t)
	f := newTenantIsoFixture(t, s, ctx, "github")

	const deliveryID = "delivery-guid-shared"
	create := func(endpointID, title string) (Todo, bool) {
		t.Helper()
		td, created, err := s.CreateTodo(ctx, CreateTodoParams{
			EndpointID: endpointID, Queue: f.queue, Source: "github", Kind: "push",
			Title: title, Payload: []byte(`{}`), IdempotencyKey: deliveryID,
		})
		if err != nil {
			t.Fatalf("create todo for %s: %v", endpointID, err)
		}
		return td, created
	}

	aTodo, createdA := create(f.epA, "A's copy")
	if !createdA {
		t.Fatal("A's first create reported no new row")
	}
	bTodo, createdB := create(f.epB, "B's copy")
	if !createdB {
		t.Fatalf("B's create collapsed onto A's todo %s — the dedup namespace is still global, so one "+
			"tenant's delivery id suppresses another's work", aTodo.ID)
	}
	if aTodo.ID == bTodo.ID {
		t.Fatalf("both endpoints share todo %s: dedup crossed the tenant boundary", aTodo.ID)
	}

	// Within one endpoint the contract is unchanged: a redelivery still collapses.
	dupA, createdDupA := create(f.epA, "A's redelivery")
	if createdDupA {
		t.Fatalf("A's redelivery minted a second todo %s; want the existing %s", dupA.ID, aTodo.ID)
	}
	if dupA.ID != aTodo.ID {
		t.Fatalf("A's redelivery returned %s, want the existing live todo %s", dupA.ID, aTodo.ID)
	}

	// Two todos total, one per tenant, each visible only to its owner.
	listA, err := s.ListTodos(ctx, f.epA, []string{f.queue}, "", 50)
	if err != nil {
		t.Fatalf("list A: %v", err)
	}
	if len(listA) != 1 || listA[0].ID != aTodo.ID {
		t.Fatalf("A's list = %v, want exactly [%s]", tenantIsoIDs(listA), aTodo.ID)
	}
	listB, err := s.ListTodos(ctx, f.epB, []string{f.queue}, "", 50)
	if err != nil {
		t.Fatalf("list B: %v", err)
	}
	if len(listB) != 1 || listB[0].ID != bTodo.ID {
		t.Fatalf("B's list = %v, want exactly [%s]", tenantIsoIDs(listB), bTodo.ID)
	}

	// Each tenant claims its own copy, independently — the delivery genuinely reached both agents.
	for _, tc := range []struct{ ep, id, owner string }{
		{f.epA, aTodo.ID, "agent-a"},
		{f.epB, bTodo.ID, "agent-b"},
	} {
		if _, err := s.ClaimTodo(ctx, tc.ep, tc.id, tc.owner, time.Hour); err != nil {
			t.Fatalf("%s claiming its own copy %s: %v", tc.owner, tc.id, err)
		}
	}
	// ...and neither can touch the other's copy of the same delivery.
	if _, err := s.ClaimTodo(ctx, f.epB, aTodo.ID, "agent-b", time.Hour); !errors.Is(err, ErrNotFound) {
		t.Fatalf("B claiming A's copy of the shared delivery err = %v, want ErrNotFound", err)
	}
}
