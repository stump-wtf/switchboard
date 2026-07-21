// LISTEN todo_ready — the in-database wakeup loop.
//
// Governing: SPEC-0004 REQ "In-Database Wakeups via LISTEN/NOTIFY". The store emits
// pg_notify('todo_ready', <queue>) after every durably committed enqueue/re-surface
// (store.notifyTodoReady); this loop is the consumer that turns those notifications into wakeups:
// the web SSE hub refreshes its count regions and the MCP channel doorbell re-rings for
// push-eligible pending todos. Without it the wakeup chain depends entirely on same-process store
// hooks — work enqueued by another process (a second switchboard instance, manual SQL) would sit
// silent until an agent polls. Delivery stays best-effort end to end: the durable queue is the
// ledger, so a missed notification costs latency, never work.
package server

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/joestump/switchboard/internal/store"
)

const (
	// listenChannel is the store's enqueue-wakeup NOTIFY channel; the payload is the queue name.
	listenChannel = "todo_ready"
	// listenBackoffMin/Max bound the reconnect backoff after the LISTEN connection drops:
	// exponential from 1s, capped at 30s, reset after any healthy (listening) connection.
	listenBackoffMin = time.Second
	listenBackoffMax = 30 * time.Second
	// nudgeTimeout bounds the store read behind one notification-driven doorbell nudge.
	nudgeTimeout = 3 * time.Second
	// nudgeBatch caps how many pending todos one notification re-rings; anything beyond stays
	// recoverable by pull (list_todos) and by the next notification.
	nudgeBatch = 16
	// doorbellGateTTL is how long a todo id suppresses duplicate doorbells. Long enough to
	// absorb the same-process double-fire (store hook + this loop observing one commit), short
	// enough that a reaper re-surface minutes later rings again.
	doorbellGateTTL = time.Minute
)

// listenTodoReady holds one dedicated LISTEN connection and invokes onReady(queue) for every
// todo_ready notification. Context-managed: it exits when ctx is cancelled (graceful shutdown,
// mirroring the reaper/pruner) and reconnects with capped exponential backoff when the connection
// drops, because a dead listener would silently degrade every cross-process wakeup for the life
// of the process. Governing: SPEC-0004 REQ "In-Database Wakeups via LISTEN/NOTIFY".
func listenTodoReady(ctx context.Context, dsn string, log *slog.Logger, onReady func(ctx context.Context, queue string)) {
	backoff := listenBackoffMin
	for ctx.Err() == nil {
		listening, err := runListen(ctx, dsn, log, onReady)
		if ctx.Err() != nil {
			return // shutdown: the connection error (if any) is just the cancel propagating
		}
		if listening {
			backoff = listenBackoffMin // the connection was healthy; don't punish the retry
		}
		log.Warn("todo_ready listener disconnected; reconnecting", "err", err, "backoff", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > listenBackoffMax {
			backoff = listenBackoffMax
		}
	}
}

// runListen dials one connection, LISTENs, and blocks delivering notifications until the
// connection or ctx dies. The returned bool reports whether the LISTEN was established (drives
// backoff reset). The deferred Close uses a fresh context because ctx is typically already
// cancelled on the shutdown path.
func runListen(ctx context.Context, dsn string, log *slog.Logger, onReady func(ctx context.Context, queue string)) (bool, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return false, err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = conn.Close(closeCtx)
	}()
	if _, err := conn.Exec(ctx, "LISTEN "+listenChannel); err != nil {
		return false, err
	}
	log.Info("listening for todo_ready wakeups", "channel", listenChannel)
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return true, err
		}
		onReady(ctx, n.Payload)
	}
}

// doorbellGate dedups MCP channel doorbells for the same todo across the two wakeup paths: the
// same-process store hook and the LISTEN loop both observe one committed enqueue, and the push
// contract wants a hint, not an echo. Entries expire after ttl so a legitimately re-surfaced todo
// (reaper, retry) rings again. Bounded by construction: expired ids are pruned on use.
type doorbellGate struct {
	mu        sync.Mutex
	ttl       time.Duration
	seen      map[string]time.Time
	lastPrune time.Time
}

func newDoorbellGate(ttl time.Duration) *doorbellGate {
	return &doorbellGate{ttl: ttl, seen: map[string]time.Time{}}
}

// first reports whether id has not been doorbelled within the TTL, recording it when so.
func (g *doorbellGate) first(id string) bool { return g.firstAt(id, time.Now()) }

func (g *doorbellGate) firstAt(id string, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	// Prune at most once per TTL so the map cannot grow unboundedly with todo ids (the same
	// bounded-growth posture as the rate limiter's bucket map).
	if now.Sub(g.lastPrune) >= g.ttl {
		g.lastPrune = now
		for k, t := range g.seen {
			if now.Sub(t) > g.ttl {
				delete(g.seen, k)
			}
		}
	}
	if t, ok := g.seen[id]; ok && now.Sub(t) <= g.ttl {
		return false
	}
	g.seen[id] = now
	return true
}

// doorbellStore is the single store seam the notification-driven doorbell nudge needs;
// *store.Store satisfies it. Narrowed to an interface so the nudge is unit-testable without a
// database.
type doorbellStore interface {
	PendingDoorbellTodosByQueue(ctx context.Context, queue string, limit int) ([]store.Todo, error)
}

// nudgeDoorbells re-rings the MCP channel doorbell for push-eligible pending todos on queue, in
// response to a todo_ready notification. The store applies the SPEC-0011 sender gate in SQL
// (verified delivery events only); the doorbellGate drops todos this process already pushed via
// the store hook, so in the common single-process deployment the LISTEN path adds no duplicate
// noise, while a todo enqueued elsewhere still wakes local sessions. Errors are logged and
// dropped — the durable queue is the ledger, so the todo stays claimable by pull.
func nudgeDoorbells(ctx context.Context, st doorbellStore, gate *doorbellGate, publish func(store.Todo), queue string, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(ctx, nudgeTimeout)
	defer cancel()
	todos, err := st.PendingDoorbellTodosByQueue(ctx, queue, nudgeBatch)
	if err != nil {
		if ctx.Err() == nil {
			log.Warn("todo_ready doorbell nudge", "queue", queue, "err", err)
		}
		return
	}
	for _, t := range todos {
		if gate.first(t.ID) {
			publish(t)
		}
	}
}
