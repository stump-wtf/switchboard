package server

// Coverage for the todo_ready LISTEN loop's building blocks (#186): the doorbell gate that dedups
// the same-process store hook against the in-database notification, and the notification-driven
// doorbell nudge. The end-to-end LISTEN→notify path is exercised against a real database when
// SWITCHBOARD_TEST_DATABASE_URL is set (the same gating as the store suite).
// Governing: SPEC-0004 REQ "In-Database Wakeups via LISTEN/NOTIFY", SPEC-0011 REQ "Sender Gate and
// Injection Safety".

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/joestump/switchboard/internal/store"
)

func TestDoorbellGateDedupsWithinTTLAndRingsAgainAfter(t *testing.T) {
	g := newDoorbellGate(time.Minute)
	base := time.Unix(1_000_000, 0)

	if !g.firstAt("td_1", base) {
		t.Fatal("first ring for td_1 must pass")
	}
	if g.firstAt("td_1", base.Add(time.Second)) {
		t.Fatal("second ring for td_1 within the TTL must be suppressed")
	}
	// A different todo is independent.
	if !g.firstAt("td_2", base.Add(time.Second)) {
		t.Fatal("td_2 must not share td_1's suppression")
	}
	// After the TTL the same id rings again — a reaper re-surface minutes later is a real,
	// announceable wakeup, not an echo.
	if !g.firstAt("td_1", base.Add(2*time.Minute)) {
		t.Fatal("td_1 must ring again after the TTL (re-surface case)")
	}
}

func TestDoorbellGatePrunesExpiredEntries(t *testing.T) {
	g := newDoorbellGate(time.Minute)
	base := time.Unix(1_000_000, 0)
	for _, id := range []string{"a", "b", "c"} {
		g.firstAt(id, base)
	}
	// Two TTLs later a new ring triggers the prune; only the fresh id may remain.
	g.firstAt("d", base.Add(3*time.Minute))
	g.mu.Lock()
	n := len(g.seen)
	g.mu.Unlock()
	if n != 1 {
		t.Fatalf("gate holds %d entries after prune, want 1 (expired ids evicted)", n)
	}
}

// fakeDoorbellStore returns a canned pending set (or error) for nudgeDoorbells, recording WHICH of
// the two reads the nudge chose and with what scope. Which read runs is the whole point: the
// endpoint-scoped one spends the batch budget on the tenant the wakeup was actually about, the
// cross-endpoint one is the legacy-payload fallback.
type fakeDoorbellStore struct {
	todos []store.Todo
	err   error

	scopedCalls  int    // PendingDoorbellTodos (endpoint-scoped) invocations
	byQueueCalls int    // PendingDoorbellTodosByQueue (cross-endpoint fallback) invocations
	endpointID   string // scope the nudge read with
	queue        string
}

func (f *fakeDoorbellStore) PendingDoorbellTodos(_ context.Context, endpointID, queue string, _ int) ([]store.Todo, error) {
	f.scopedCalls++
	f.endpointID, f.queue = endpointID, queue
	return f.todos, f.err
}

func (f *fakeDoorbellStore) PendingDoorbellTodosByQueue(_ context.Context, queue string, _ int) ([]store.Todo, error) {
	f.byQueueCalls++
	f.queue = queue
	return f.todos, f.err
}

const doorbellEndpointID = "6f0b6e5c-6f4a-4f0e-9b0e-2c1d3e4f5a6b"

// A todo_ready payload is "<endpoint_id>:<queue>" (store.TodoReadyPayload), and the nudge must read
// through the ENDPOINT-SCOPED query with that exact endpoint. Reading by queue alone would pull
// every tenant's pending rows on a shared queue name and burn the batch cap on todos this wakeup
// was not about — the boundary would still hold at the publisher, but one busy endpoint could
// starve every other endpoint on "reviews" of doorbells entirely.
// Governing: ADR-0022, SPEC-0003 REQ "Endpoint Ownership (Tenant Isolation)", SPEC-0004 REQ
// "In-Database Wakeups via LISTEN/NOTIFY".
func TestNudgeDoorbellsReadsScopedToTheNotifiedEndpoint(t *testing.T) {
	st := &fakeDoorbellStore{todos: []store.Todo{
		{ID: "td_hooked", EndpointID: doorbellEndpointID, Queue: "ci"},
		{ID: "td_foreign", EndpointID: doorbellEndpointID, Queue: "ci"},
	}}
	gate := newDoorbellGate(time.Minute)
	// td_hooked was already pushed by the same-process store hook.
	gate.first("td_hooked")

	var published []string
	nudgeDoorbells(context.Background(), st, gate, func(t store.Todo) {
		published = append(published, t.ID)
	}, doorbellEndpointID+":ci", slog.New(slog.NewTextHandler(io.Discard, nil)))

	if st.scopedCalls != 1 || st.byQueueCalls != 0 {
		t.Fatalf("scoped=%d byQueue=%d, want exactly one endpoint-scoped read (a queue-only read "+
			"spends the batch budget across tenants)", st.scopedCalls, st.byQueueCalls)
	}
	if st.endpointID != doorbellEndpointID || st.queue != "ci" {
		t.Fatalf("nudge read endpoint=%q queue=%q, want %q/ci", st.endpointID, st.queue, doorbellEndpointID)
	}
	if len(published) != 1 || published[0] != "td_foreign" {
		t.Fatalf("published %v, want exactly [td_foreign] (hooked todo deduped)", published)
	}
}

// The endpoint id is a uuid and therefore colon-free, so the payload splits on the FIRST colon and
// everything after it is the queue name verbatim — a queue whose name contains colons survives the
// round trip instead of being silently truncated to its first segment.
func TestNudgeDoorbellsSplitsPayloadOnTheFirstColonOnly(t *testing.T) {
	st := &fakeDoorbellStore{}
	nudgeDoorbells(context.Background(), st, newDoorbellGate(time.Minute), func(store.Todo) {},
		doorbellEndpointID+":ci:deploy:eu", slog.New(slog.NewTextHandler(io.Discard, nil)))

	if st.endpointID != doorbellEndpointID {
		t.Fatalf("endpoint scope = %q, want %q", st.endpointID, doorbellEndpointID)
	}
	if st.queue != "ci:deploy:eu" {
		t.Fatalf("queue = %q, want ci:deploy:eu (only the first colon delimits)", st.queue)
	}
}

// A bare-queue payload has no endpoint id: an older process mid-deploy, or a hand-issued
// pg_notify from psql. It degrades to the cross-endpoint read rather than being dropped — the
// per-session publisher still applies the tenant boundary, so reach is what is lost, not isolation.
func TestNudgeDoorbellsFallsBackForLegacyBareQueuePayload(t *testing.T) {
	st := &fakeDoorbellStore{todos: []store.Todo{{ID: "td_legacy", Queue: "ci"}}}

	var published []string
	nudgeDoorbells(context.Background(), st, newDoorbellGate(time.Minute), func(t store.Todo) {
		published = append(published, t.ID)
	}, "ci", slog.New(slog.NewTextHandler(io.Discard, nil)))

	if st.byQueueCalls != 1 || st.scopedCalls != 0 {
		t.Fatalf("scoped=%d byQueue=%d, want exactly one cross-endpoint fallback read",
			st.scopedCalls, st.byQueueCalls)
	}
	if st.queue != "ci" {
		t.Fatalf("fallback queried queue %q, want ci", st.queue)
	}
	if len(published) != 1 || published[0] != "td_legacy" {
		t.Fatalf("published %v, want [td_legacy] (a legacy wakeup still rings)", published)
	}
}

func TestNudgeDoorbellsSwallowsStoreErrors(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	// Both reads: a store failure is logged and dropped on either path — the durable queue is the
	// ledger, so a failed nudge costs latency, never work.
	for name, payload := range map[string]string{
		"endpoint-scoped": doorbellEndpointID + ":ci",
		"legacy queue":    "ci",
	} {
		t.Run(name, func(t *testing.T) {
			st := &fakeDoorbellStore{err: errors.New("db down")}
			nudgeDoorbells(context.Background(), st, newDoorbellGate(time.Minute), func(store.Todo) {
				t.Fatal("nothing may publish on a store error")
			}, payload, log)
		})
	}
}

// End-to-end: a pg_notify('todo_ready', '<endpoint_id>:<queue>') reaches the listener's onReady
// callback with its payload intact, and cancelling the context shuts the loop down. Requires a real
// database (LISTEN needs a live connection; no schema is touched).
func TestListenTodoReadyDeliversNotification(t *testing.T) {
	dsn := os.Getenv("SWITCHBOARD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set SWITCHBOARD_TEST_DATABASE_URL to run LISTEN tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// The wire payload the store actually emits (store.TodoReadyPayload), not a bare queue name:
	// the listener must carry the endpoint scope through untouched for the nudge to read one tenant.
	const payload = doorbellEndpointID + ":alerts"

	got := make(chan string, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		listenTodoReady(ctx, dsn, slog.New(slog.NewTextHandler(io.Discard, nil)),
			func(_ context.Context, p string) {
				select {
				case got <- p:
				default:
				}
			})
	}()

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(context.Background())
	// The listener dials + LISTENs asynchronously; retry the notify until it lands or times out.
	deadline := time.After(8 * time.Second)
	for {
		if _, err := conn.Exec(ctx, `SELECT pg_notify('todo_ready', $1)`, payload); err != nil {
			t.Fatalf("pg_notify: %v", err)
		}
		select {
		case p := <-got:
			if p != payload {
				t.Fatalf("notification payload = %q, want %q", p, payload)
			}
			cancel() // graceful shutdown: the loop must exit, not linger
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("listener did not exit after context cancel")
			}
			return
		case <-deadline:
			t.Fatal("notification never reached the listener")
		case <-time.After(100 * time.Millisecond):
			// not yet listening — notify again
		}
	}
}
