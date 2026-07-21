package ingest

// Hub fan-out is a tenant boundary, not a convenience filter.
//
// The Hub is the in-process doorbell the accept path rings when a delivery becomes a todo. It once
// filtered on queue NAME alone, which made it the same cross-tenant leak as the pre-ADR-0022 todos
// table: two subscribers belonging to two different humans who both called their queue "reviews" or
// "github" — the names everyone picks — each received the other's todos, payload included.
//
// These tests drive Hub.Subscribe/Hub.Publish directly, which is the whole of its public surface.
// They deliberately need no database: a Hub regression must fail the suite even on a machine with
// no Postgres, where the DB-backed accept-path tests skip.
//
// Governing: ADR-0022, SPEC-0003 REQ "Endpoint Ownership (Tenant Isolation)",
// SPEC-0011 REQ "Scope-Filtered Fan-Out".

import (
	"testing"
	"time"

	"github.com/joestump/switchboard/internal/store"
)

// hubSharedQueueNames are the collision cases from the reported bug: queue labels two unrelated
// tenants independently choose. Covered separately so a filter that special-cased one name could
// not hide behind the other.
var hubSharedQueueNames = []string{"reviews", "github"}

// recvTodo waits briefly for one todo on ch. The Hub is synchronous (Publish writes into a buffered
// channel before returning), so a short timeout is a generous upper bound, not a race.
func recvTodo(t *testing.T, ch <-chan store.Todo, what string) store.Todo {
	t.Helper()
	select {
	case td, ok := <-ch:
		if !ok {
			t.Fatalf("%s: channel closed before a todo arrived", what)
		}
		return td
	case <-time.After(2 * time.Second):
		t.Fatalf("%s: no todo delivered", what)
		return store.Todo{}
	}
}

// assertNoTodo fails if anything at all arrives on ch. Used for the negative half: silence here is
// the isolation guarantee.
func assertNoTodo(t *testing.T, ch <-chan store.Todo, what string) {
	t.Helper()
	select {
	case td := <-ch:
		t.Fatalf("%s: cross-tenant leak — received todo %s owned by endpoint %q (queue %q, title %q)",
			what, td.ID, td.EndpointID, td.Queue, td.Title)
	case <-time.After(250 * time.Millisecond):
	}
}

// TestHubNeverFansAcrossEndpointsOnASharedQueueName: a subscriber only ever sees todos owned by the
// endpoint it subscribed as, even when another endpoint publishes on the identical queue name.
//
// The negative assertion is made non-vacuous by ordering: B's todo is published SECOND and B is
// asserted to receive it. If B's subscription were dead or misconfigured, that positive assertion
// would fail, so B's silence on A's todo can only mean the endpoint filter did its job. Likewise A
// is asserted to receive its own todo, so a Hub that dropped everything would fail too.
//
// Governing: SPEC-0003 REQ "Endpoint Ownership (Tenant Isolation)" scenario "Cross-endpoint queue
// collision is isolated".
func TestHubNeverFansAcrossEndpointsOnASharedQueueName(t *testing.T) {
	for _, queue := range hubSharedQueueNames {
		t.Run(queue, func(t *testing.T) {
			hub := NewHub()

			const epA, epB = "11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"
			chA, cancelA := hub.Subscribe(epA, []string{queue})
			defer cancelA()
			chB, cancelB := hub.Subscribe(epB, []string{queue})
			defer cancelB()

			// Owned by A, on the queue name BOTH subscribers hold. Queue scope alone cannot tell
			// these two subscribers apart — only endpoint ownership can.
			aTodo := store.Todo{
				ID: "td_a_" + queue, EndpointID: epA, Queue: queue,
				Source: "github", Kind: "push", Title: "A's private work", State: "pending",
			}
			hub.Publish(aTodo)

			if got := recvTodo(t, chA, "endpoint A"); got.ID != aTodo.ID {
				t.Fatalf("endpoint A received %s, want its own todo %s", got.ID, aTodo.ID)
			}
			assertNoTodo(t, chB, "endpoint B on A's todo")

			// B's own todo proves B's subscription is live, which is what makes the silence above
			// evidence of filtering rather than evidence of a broken fixture.
			bTodo := store.Todo{
				ID: "td_b_" + queue, EndpointID: epB, Queue: queue,
				Source: "github", Kind: "push", Title: "B's own work", State: "pending",
			}
			hub.Publish(bTodo)

			if got := recvTodo(t, chB, "endpoint B"); got.ID != bTodo.ID {
				t.Fatalf("endpoint B received %s, want its own todo %s", got.ID, bTodo.ID)
			}
			// A must not see B's either — the boundary is symmetric, not a one-way ACL.
			assertNoTodo(t, chA, "endpoint A on B's todo")
		})
	}
}

// TestHubUnscopedSubscriptionReceivesNothing: there is no unscoped subscription.
//
// An empty endpoint id is not "subscribe to everything" — an unscoped subscription IS the leak. A
// caller that legitimately observes every tenant is an operator surface and reads the store's
// operator-scoped paths instead. The same fail-closed rule applies to a todo with no owner: the
// NOT NULL column makes it impossible for a persisted row, so a half-populated struct reaching
// Publish is a bug, and it must match nobody rather than everybody.
//
// Governing: ADR-0022, SPEC-0003 REQ "Endpoint Ownership (Tenant Isolation)".
func TestHubUnscopedSubscriptionReceivesNothing(t *testing.T) {
	hub := NewHub()

	const epA = "11111111-1111-1111-1111-111111111111"
	unscoped, cancelUnscoped := hub.Subscribe("", []string{"reviews", "github"})
	defer cancelUnscoped()
	scoped, cancelScoped := hub.Subscribe(epA, []string{"reviews"})
	defer cancelScoped()

	owned := store.Todo{ID: "td_owned", EndpointID: epA, Queue: "reviews", Title: "A's work"}
	hub.Publish(owned)
	if got := recvTodo(t, scoped, "scoped subscriber"); got.ID != owned.ID {
		t.Fatalf("scoped subscriber received %s, want %s", got.ID, owned.ID)
	}
	assertNoTodo(t, unscoped, "unscoped subscriber")

	// An ownerless todo fans out to nobody — not even to the subscriber whose queue it names.
	hub.Publish(store.Todo{ID: "td_ownerless", Queue: "reviews", Title: "half-populated struct"})
	assertNoTodo(t, scoped, "scoped subscriber on an ownerless todo")
	assertNoTodo(t, unscoped, "unscoped subscriber on an ownerless todo")
}
