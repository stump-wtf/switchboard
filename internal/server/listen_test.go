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

// fakeDoorbellStore returns a canned pending set (or error) for nudgeDoorbells.
type fakeDoorbellStore struct {
	todos []store.Todo
	err   error
	queue string
}

func (f *fakeDoorbellStore) PendingDoorbellTodos(_ context.Context, queue string, _ int) ([]store.Todo, error) {
	f.queue = queue
	return f.todos, f.err
}

func TestNudgeDoorbellsPublishesOnlyUngatedTodos(t *testing.T) {
	st := &fakeDoorbellStore{todos: []store.Todo{
		{ID: "td_hooked", Queue: "ci"},
		{ID: "td_foreign", Queue: "ci"},
	}}
	gate := newDoorbellGate(time.Minute)
	// td_hooked was already pushed by the same-process store hook.
	gate.first("td_hooked")

	var published []string
	nudgeDoorbells(context.Background(), st, gate, func(t store.Todo) {
		published = append(published, t.ID)
	}, "ci", slog.New(slog.NewTextHandler(io.Discard, nil)))

	if st.queue != "ci" {
		t.Fatalf("nudge queried queue %q, want ci", st.queue)
	}
	if len(published) != 1 || published[0] != "td_foreign" {
		t.Fatalf("published %v, want exactly [td_foreign] (hooked todo deduped)", published)
	}
}

func TestNudgeDoorbellsSwallowsStoreErrors(t *testing.T) {
	st := &fakeDoorbellStore{err: errors.New("db down")}
	nudgeDoorbells(context.Background(), st, newDoorbellGate(time.Minute), func(store.Todo) {
		t.Fatal("nothing may publish on a store error")
	}, "ci", slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// End-to-end: a pg_notify('todo_ready', queue) reaches the listener's onReady callback, and
// cancelling the context shuts the loop down. Requires a real database (LISTEN needs a live
// connection; no schema is touched).
func TestListenTodoReadyDeliversNotification(t *testing.T) {
	dsn := os.Getenv("SWITCHBOARD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set SWITCHBOARD_TEST_DATABASE_URL to run LISTEN tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	got := make(chan string, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		listenTodoReady(ctx, dsn, slog.New(slog.NewTextHandler(io.Discard, nil)),
			func(_ context.Context, queue string) {
				select {
				case got <- queue:
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
		if _, err := conn.Exec(ctx, `SELECT pg_notify('todo_ready', $1)`, "alerts"); err != nil {
			t.Fatalf("pg_notify: %v", err)
		}
		select {
		case q := <-got:
			if q != "alerts" {
				t.Fatalf("notification payload = %q, want alerts", q)
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
