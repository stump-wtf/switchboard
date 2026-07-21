package mcp

// The push half of the cross-tenant leak, asserted on a LIVE session's wire stream.
//
// Two agents belonging to two different humans, each with its own vended endpoint, both scoped to
// the same queue name. A's delivery must ring A's doorbell and must be inaudible to B — including
// the todo id and title, which the notification carries in clear text and which are themselves the
// disclosure.
//
// This complements the store-side isolation suite (internal/store/tenant_isolation_test.go, which
// asserts the pull path and the doorbell HOOK's endpoint key) by asserting the final hop: the
// filter inside PublishTodoReady that decides which attached sessions a committed todo reaches.
//
// Governing: ADR-0022, ADR-0013; SPEC-0003 REQ "Endpoint Ownership (Tenant Isolation)" scenario
// "Doorbell never crosses endpoints"; SPEC-0011 REQ "Scope-Filtered Fan-Out".

import (
	"strings"
	"testing"
	"time"

	"github.com/joestump/switchboard/internal/store"
)

// tenantIsoTodo builds a todo pinned to endpointID. Pinning is not optional decoration:
// PublishTodoReady's first check is the ADR-0022 tenant boundary, so a fixture that forgot the
// owner would be dropped before the queue filter ran and the test would assert nothing.
func tenantIsoTodo(endpointID, id, queue, title string) store.Todo {
	return store.Todo{
		EndpointID: endpointID, ID: id, Queue: queue,
		Kind: "push", Source: "github", Title: title,
	}
}

// TestDoorbellIsolatesTwoHumansSharingAQueueName drives both sides of the boundary on one Handler.
//
// The ordering is the proof, and it is what keeps the negative assertion honest. A's todo is
// published first and B is asserted silent; then a todo owned by B is published on the SAME queue
// and B is asserted to receive it. If B's stream were never open — the way a "wait a second and see
// nothing" test silently degrades into asserting nothing at all — that second assertion would fail.
// So B's silence on A's todo can only mean the tenant filter ran.
func TestDoorbellIsolatesTwoHumansSharingAQueueName(t *testing.T) {
	// "github" deliberately, not "reviews": the sibling test in doorbell_test.go covers "reviews",
	// and a filter that special-cased one well-known queue name must not be able to hide.
	const queue = "github"
	const slugA, slugB = "tenant-a-33333333", "tenant-b-44444444"

	f := newFakeStore()
	tokenA := vend(t, f, slugA, []string{queue}, []string{"list_todos", "claim"})
	tokenB := vend(t, f, slugB, []string{queue}, []string{"list_todos", "claim"})
	ts, h := newTestServerHandler(t, f)

	sessA := rawInitialize(t, ts.URL+"/mcp/"+slugA, tokenA)
	eventsA, _ := sessA.openStream()
	sessB := rawInitialize(t, ts.URL+"/mcp/"+slugB, tokenB)
	eventsB, _ := sessB.openStream()

	epA, epB := endpointIDFor(slugA), endpointIDFor(slugB)
	if epA == epB {
		t.Fatalf("both sessions resolved to endpoint %s: there is no tenant boundary to test", epA)
	}

	// --- A's delivery: A hears it, B must not. ---
	aTodo := tenantIsoTodo(epA, "td_tenant_a", queue, "A's private work")
	if got := publishUntilDelivered(t, h, aTodo, eventsA).Params.Meta["todo_id"]; got != aTodo.ID {
		t.Fatalf("endpoint A's doorbell carried todo_id %q, want %q", got, aTodo.ID)
	}
	select {
	case n := <-eventsB:
		t.Fatalf("cross-tenant doorbell leak on queue %q: endpoint %s received %+v, a todo owned by %s",
			queue, epB, n, aTodo.EndpointID)
	case <-time.After(time.Second):
	}

	// --- B's own delivery: B hears it. This is what proves B's stream was alive above. ---
	bTodo := tenantIsoTodo(epB, "td_tenant_b", queue, "B's own work")
	got := publishUntilDelivered(t, h, bTodo, eventsB)
	if got.Params.Meta["todo_id"] != bTodo.ID {
		t.Fatalf("endpoint B's doorbell carried todo_id %q, want %q", got.Params.Meta["todo_id"], bTodo.ID)
	}
	// The boundary is symmetric: A must not hear B's delivery either. A's stream is likewise known
	// live, because it delivered aTodo above.
	select {
	case n := <-eventsA:
		t.Fatalf("cross-tenant doorbell leak on queue %q: endpoint %s received %+v, a todo owned by %s",
			queue, epA, n, bTodo.EndpointID)
	case <-time.After(time.Second):
	}

	// The leaked field that mattered most in the report was the human-readable title, which the
	// doorbell inlines. Assert B never saw A's title text in anything it did receive.
	if content := got.Params.Content; strings.Contains(content, aTodo.Title) {
		t.Fatalf("endpoint B's doorbell content leaked A's title: %q", content)
	}
}
