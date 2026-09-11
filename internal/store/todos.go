package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrConflict is returned when a state transition loses a race (e.g. claim an already-claimed todo).
var ErrConflict = errors.New("store: conflict")

// querier is the subset of pgx shared by *pgxpool.Pool and pgx.Tx, so the raw-SQL helpers can run
// either directly on the pool or inside a transaction (e.g. the atomic event+todo insert). ADR-0002.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Todo is a durable work-item (ADR-0007). States: pending → claimed → done|failed.
// EndpointID pins every agent-facing todo to exactly one vended MCP endpoint for its entire
// lifecycle (ADR-0022): the endpoint's owning human is the todo's tenant, and every agent-facing
// query predicates on it so queue-name collisions across endpoints can never leak work across
// tenants. EndpointID is always set — todos.endpoint_id is NOT NULL and there is no system-todo
// exception. Friend approvals, which once motivated a nullable owner, are not todos at all: the
// request is durable in friend_edges and the Board renders pending approvals from that edge.
type Todo struct {
	ID             string
	EndpointID     string // always set — the owning endpoint, NOT NULL in the schema (ADR-0022)
	Queue          string
	Source         string
	Kind           string
	Title          string
	Payload        []byte // raw JSON (jsonb)
	EventID        *int64 // originating inbound event, when webhook-born (links Board feed rows)
	IdempotencyKey string
	Assignee       string
	State          string
	Owner          string
	LeaseExpiresAt *time.Time
	Attempt        int
	MaxAttempts    int
	Result         []byte
	CreatedAt      time.Time
	ClaimedAt      *time.Time
	CompletedAt    *time.Time
	NextRetryAt    *time.Time // scheduled backoff re-queue for a failed-below-cap todo; nil = dead-lettered/none
	// RoutingTrace is how the delivery that minted this todo was routed (routing.Trace JSON), nil for
	// todos that did not come through the routing stage. Governing: SPEC-0020 REQ "Routing Trace"
	// (every todo explains itself).
	RoutingTrace []byte
}

const todoCols = `id, endpoint_id::text, queue, COALESCE(source,''), COALESCE(kind,''), title, payload, event_id,
	COALESCE(idempotency_key,''), COALESCE(assignee,''), state, COALESCE(owner,''),
	lease_expires_at, attempt, max_attempts, result, created_at, claimed_at, completed_at,
	next_retry_at, routing_trace`

func scanTodo(row pgx.Row) (Todo, error) {
	var t Todo
	var endpointID *string
	err := row.Scan(&t.ID, &endpointID, &t.Queue, &t.Source, &t.Kind, &t.Title, &t.Payload, &t.EventID,
		&t.IdempotencyKey, &t.Assignee, &t.State, &t.Owner, &t.LeaseExpiresAt, &t.Attempt,
		&t.MaxAttempts, &t.Result, &t.CreatedAt, &t.ClaimedAt, &t.CompletedAt, &t.NextRetryAt, &t.RoutingTrace)
	if endpointID != nil {
		t.EndpointID = *endpointID
	}
	return t, err
}

// Retry backoff schedule (SPEC-0003 REQ "Bounded Retries via max_attempts", scheduled backoff): a
// fail below the attempt cap parks the todo in `failed` with next_retry_at = now() + backoff, where
// the backoff doubles per attempt from a 30s base and caps at 15m — attempt 1 → 30s, 2 → 1m,
// 3 → 2m, 4 → 4m, 5 → 8m, 6+ → 15m (cap). The SQL in FailTodo mirrors retryBackoff exactly (same
// base/cap constants, same power-of-two curve) so the Go function is the documented, testable spec
// of the schedule.
const (
	retryBackoffBase = 30 * time.Second
	retryBackoffCap  = 15 * time.Minute
)

// retryBackoff returns the scheduled delay before the given (1-based, just-failed) attempt is
// re-queued. Out-of-range attempts clamp to the base; the doubling curve caps at retryBackoffCap.
func retryBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := retryBackoffBase
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= retryBackoffCap {
			return retryBackoffCap
		}
	}
	return d
}

// endpointScope validates a tenant-scope endpoint id before it reaches SQL. Every endpoint-scoped
// query compares `endpoint_id = $n` — a uuid column — against a Go string, so an empty or
// malformed id would surface as a Postgres "invalid input syntax for type uuid" (SQLSTATE 22P02)
// rather than a clean miss. That is both a poor error and an information leak in its own right: it
// distinguishes "you asked with a broken scope" from "no such row" on a path whose entire purpose
// is to make those indistinguishable. Guarding here returns ErrNotFound for mutation/single-row
// reads and an empty result for list reads, which is exactly what a scope owning nothing looks
// like. Governing: ADR-0022, SPEC-0003 REQ "Endpoint Ownership (Tenant Isolation)".
func endpointScope(endpointID string) error {
	if endpointID == "" {
		return ErrNotFound
	}
	if _, err := uuid.Parse(endpointID); err != nil {
		return ErrNotFound
	}
	return nil
}

// CreateTodoParams are the inputs to CreateTodo.
type CreateTodoParams struct {
	EndpointID     string // the owning vended endpoint; pins this todo to its tenant (ADR-0022)
	Queue          string
	Source         string
	Kind           string
	Title          string
	Payload        []byte
	EventID        *int64
	IdempotencyKey string
	Assignee       string
	RoutingTrace   []byte // routing.Trace JSON when the todo came through the routing stage (SPEC-0020)
}

// CreateTodo inserts a todo, deduping on (endpoint_id, idempotency_key) among LIVE rows — pending,
// claimed, or a parked retry (failed with an open next_retry_at window); only `done` and true
// dead-letters leave dedup. The bool reports whether a new row was created (false = an existing
// live todo already covers it). ADR-0007; SPEC-0003 REQ "Idempotent Enqueue and Dedup",
// SPEC-0003 REQ "Per-Endpoint Idempotency and Dedup" (ADR-0022: dedup is per-endpoint).
func (s *Store) CreateTodo(ctx context.Context, p CreateTodoParams) (Todo, bool, error) {
	t, created, err := createTodo(ctx, s.pool, p)
	if err == nil && created {
		// Governing: SPEC-0004 REQ "In-Database Wakeups via LISTEN/NOTIFY". Best-effort nudge so an
		// idle worker/UI wakes without polling. The durable queue is the source of truth, so a lost
		// NOTIFY costs only latency, never work — hence the error here is deliberately ignored.
		s.notifyTodoReady(ctx, t.EndpointID, t.Queue)
		s.fireTodoHook("created", t)
	}
	return t, created, err
}

// CreateEventTodo records an accepted delivery and enqueues its todo in ONE transaction, so a failure
// enqueuing the todo can never orphan a persisted event row (and vice-versa). The event id is linked
// onto the todo. On success it returns the event id, the todo, and whether a NEW todo was created
// (false = idempotent duplicate). Governing: SPEC-0002/0004 REQ atomic ingestion — event and todo
// commit together or not at all.
func (s *Store) CreateEventTodo(ctx context.Context, e EventInput, p CreateTodoParams) (int64, Todo, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, Todo{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op once committed; rolls back on any early return

	ev, inserted, err := insertEvent(ctx, tx, e)
	if err != nil {
		return 0, Todo{}, false, err
	}
	p.EventID = &ev.ID
	t, created, err := createTodo(ctx, tx, p)
	if err != nil {
		return 0, Todo{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, Todo{}, false, err
	}
	// Hooks fire only after the durable commit, event before todo, mirroring the Board's
	// lifecycle order (the committed event precedes its todo_created lane movement).
	// Governing: SPEC-0015 REQ "Patch Panel Board".
	if inserted {
		s.fireEventHook(ev)
	}
	if created {
		s.notifyTodoReady(ctx, t.EndpointID, t.Queue)
		s.fireTodoHook("created", t)
		// Sender gate (SPEC-0011): only a todo whose delivery event passed per-source
		// verification is eligible for a channel push. Plain CreateTodo (no event, e.g. the dev
		// helper) never rings the doorbell — those todos degrade to pull, losing nothing.
		// EXCEPTION (ADR-0023, the basics): a token-trust SELF-MANAGED webhook is authenticated
		// by its unguessable ingest URL (SPEC-0006 — the URL is the credential), so its
		// deliveries ring the doorbell like verified ones do; an anonymous delivery never
		// reaches this path with a token trust mode.
		if e.Verified || e.TrustMode == "token" {
			s.fireDoorbell(t)
		}
	}
	return ev.ID, t, created, nil
}

// CreatedTodo pairs a fanned-out todo with whether THIS delivery actually minted it. New is false
// when the row came back from the idempotent-redelivery path — createTodo returned a pre-existing
// live row (pending, claimed, or a parked retry) instead of inserting. Callers need this per-todo,
// not as an aggregate count: a delivery routed to A and B may be new for A and a redelivery for B,
// and only A may ring hooks or the doorbell. Governing: ADR-0022, SPEC-0003 REQ "Per-Endpoint
// Idempotency and Dedup".
type CreatedTodo struct {
	Todo Todo
	New  bool
}

// CreateEventTodos records an accepted delivery and fans it out into N todos — one per target
// endpoint — in ONE transaction, so a failure enqueuing any todo can never orphan a persisted
// event row or leave a partial fan-out (SPEC-0002/0004 atomic ingestion generalized to N targets;
// ADR-0022 deterministic route fan-out). The event id is linked onto every todo. Each target
// endpoint gets its own todo with an independently-namespaced idempotency key (per-target dedup),
// so the same delivery routed to A and B produces two todos that each dedup independently across
// redeliveries.
//
// On success it returns the event id and the N results in target order, each carrying its own New
// flag. Hooks and the doorbell fire ONLY for genuinely new rows: fireTodoHook is NOT covered by the
// server's doorbellGate, so firing "created" on an idempotent redelivery would push a spurious
// lifecycle frame to the operator Board for work that did not actually appear.
// Governing: ADR-0022, SPEC-0001 REQ "Deterministic Route Fan-Out (Token-Free)".
func (s *Store) CreateEventTodos(ctx context.Context, e EventInput, targetEndpointIDs []string, p CreateTodoParams) (int64, []CreatedTodo, error) {
	id, out, _, err := s.CreateRoutedEventTodos(ctx, e, false, targetEndpointIDs, p)
	return id, out, err
}

// CreateRoutedEventTodos is CreateEventTodos with the routing stage's outcome applied (SPEC-0020).
// When drop is true — or when this delivery is a redelivery of one that was ALREADY dropped — the
// event row is recorded (with its routing trace, spending its (source, external_id) dedup slot) and
// the transaction commits with no todo, no todo hook, and no doorbell. The stickiness is what keeps
// the dedup contract routing-independent: a producer redelivering a dropped event after the owner
// edited the rules does not get it re-processed into work. The returned bool reports that outcome.
// Governing: SPEC-0020 REQ "Drop Action Semantics", design "Drop semantics: spend the dedup slot,
// keep the receipt"; ADR-0024.
func (s *Store) CreateRoutedEventTodos(ctx context.Context, e EventInput, drop bool, targetEndpointIDs []string, p CreateTodoParams) (int64, []CreatedTodo, bool, error) {
	if !drop && len(targetEndpointIDs) == 0 {
		return 0, nil, false, fmt.Errorf("store: CreateEventTodos requires at least one target endpoint")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, nil, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op once committed; rolls back on any early return

	ev, inserted, err := insertEvent(ctx, tx, e)
	if err != nil {
		return 0, nil, false, err
	}
	if !inserted && !drop {
		// The first routing decision recorded for a delivery is the one that counts for a drop.
		if err := tx.QueryRow(ctx,
			`SELECT COALESCE((routing_trace->'action'->>'drop')::boolean, false) FROM events WHERE id = $1`, ev.ID,
		).Scan(&drop); err != nil {
			return 0, nil, false, fmt.Errorf("store: read prior routing: %w", err)
		}
	}
	if drop {
		if err := tx.Commit(ctx); err != nil {
			return 0, nil, false, err
		}
		if inserted {
			s.fireEventHook(ev)
		}
		return ev.ID, nil, true, nil
	}
	p.EventID = &ev.ID
	out := make([]CreatedTodo, 0, len(targetEndpointIDs))
	for _, epID := range targetEndpointIDs {
		// Each target dedups independently WITHOUT rewriting the key: the dedup namespace is the
		// composite index (endpoint_id, idempotency_key) (0012), so endpoint_id already separates
		// two targets of the same delivery. Prefixing the key with the endpoint id as well would be
		// redundant — and actively harmful, because the stored value is a correlation key, not an
		// opaque token: the ingest layer sets the event's external_id and each todo's
		// idempotency_key to the SAME string, and three consumers join on that equality
		// (queue_view.go's dedup_count subquery, board.go's deduped flag, and live.go's rxCardID,
		// which retires the Board's ephemeral "received" card). A prefixed key matches none of
		// them, so the dedup badge silently reads 0 and the received card sticks forever.
		// Governing: ADR-0022, SPEC-0003 REQ "Per-Endpoint Idempotency and Dedup".
		tp := p
		tp.EndpointID = epID
		t, wasNew, err := createTodo(ctx, tx, tp)
		if err != nil {
			return 0, nil, false, err
		}
		out = append(out, CreatedTodo{Todo: t, New: wasNew})
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, nil, false, err
	}
	// Hooks fire only after the durable commit, event before todos, mirroring the Board's
	// lifecycle order. Governing: SPEC-0015 REQ "Patch Panel Board".
	if inserted {
		s.fireEventHook(ev)
	}
	for _, ct := range out {
		// Skip redeliveries entirely. The doorbellGate (internal/server) would absorb a duplicate
		// doorbell, but it does not gate fireTodoHook — an unconditional fire would emit a
		// "created" SSE frame to the Board on every redelivery of an already-live todo.
		if !ct.New {
			continue
		}
		s.fireTodoHook("created", ct.Todo)
		s.notifyTodoReady(ctx, ct.Todo.EndpointID, ct.Todo.Queue)
		// Sender gate (SPEC-0011): only a todo whose delivery event passed per-source verification
		// is eligible for a channel push. EXCEPTION (ADR-0023, the basics): a token-trust
		// SELF-MANAGED webhook is authenticated by its unguessable ingest URL (SPEC-0006 — the
		// URL is the credential), so its deliveries ring the doorbell like verified ones; an
		// anonymous (open) source never reaches this path carrying a token trust mode.
		if e.Verified || e.TrustMode == "token" {
			s.fireDoorbell(ct.Todo)
		}
	}
	return ev.ID, out, false, nil
}

// createTodo is the querier-based core of CreateTodo: it runs on either the pool or a transaction and
// performs no LISTEN/NOTIFY (the caller nudges only after a durable commit).
//
// The ON CONFLICT clause mirrors the idx_todos_dedupe partial-index predicate (0012): a LIVE row is
// anything not `done` and not a true dead-letter — a parked retry (`failed` with an open
// next_retry_at window, SPEC-0003 scheduled backoff) still holds its dedup slot, so a redelivery
// during the backoff collapses onto it instead of minting a duplicate active todo (which the
// re-queue transition would then collide with, SQLSTATE 23505). The dedup key is
// (endpoint_id, idempotency_key) per ADR-0022: two endpoints that share an idempotency key each
// retain their own todo, while redelivery to one endpoint collapses.
func createTodo(ctx context.Context, q querier, p CreateTodoParams) (Todo, bool, error) {
	id := "td_" + uuid.NewString()
	// Every todo is owned by exactly one endpoint — todos.endpoint_id is NOT NULL (0012) and there
	// is no sentinel or system-todo escape hatch (ADR-0022). Reject an empty EndpointID here rather
	// than letting it reach the INSERT, so a caller that forgets to set it gets a named error
	// instead of an opaque 23502 not-null violation.
	if p.EndpointID == "" {
		return Todo{}, false, fmt.Errorf("store: createTodo requires a non-empty EndpointID")
	}
	row := q.QueryRow(ctx, `
		INSERT INTO todos (id, endpoint_id, queue, source, kind, title, payload, event_id, idempotency_key, assignee, routing_trace)
		VALUES ($1, $2, $3, NULLIF($4,''), NULLIF($5,''), $6, $7, $8, NULLIF($9,''), NULLIF($10,''), $11)
		ON CONFLICT (endpoint_id, idempotency_key)
			WHERE idempotency_key IS NOT NULL AND state <> 'done'
				AND (state <> 'failed' OR next_retry_at IS NOT NULL)
			DO NOTHING
		RETURNING `+todoCols,
		id, p.EndpointID, p.Queue, p.Source, p.Kind, p.Title, p.Payload, p.EventID, p.IdempotencyKey, p.Assignee, p.RoutingTrace)
	t, err := scanTodo(row)
	if err == nil {
		return t, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Todo{}, false, err
	}
	// Conflict: return the existing live todo (pending/claimed, or a parked retry). Scoped to the
	// same endpoint, mirroring the (endpoint_id, idempotency_key) dedup key — a colliding key under
	// a DIFFERENT endpoint is a different tenant's row and must never be returned here.
	row = q.QueryRow(ctx, `SELECT `+todoCols+` FROM todos
		WHERE idempotency_key = $2 AND state <> 'done'
			AND (state <> 'failed' OR next_retry_at IS NOT NULL)
			AND endpoint_id = $1
		ORDER BY created_at LIMIT 1`, p.EndpointID, p.IdempotencyKey)
	t, err = scanTodo(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Todo{}, false, ErrNotFound
	}
	return t, false, err
}

// TodoReadyPayload builds the todo_ready NOTIFY payload: "<endpoint_id>:<queue>". The endpoint id
// leads because it is a uuid and therefore colon-free, so the listener can split on the FIRST
// colon and recover a queue name that itself contains colons.
//
// The payload carries the endpoint id — not just the queue — so the LISTEN loop can scope its
// follow-up read to the one tenant whose work actually became ready. With a queue-only payload the
// listener had no choice but to read EVERY endpoint's pending todos on that queue and then rely on
// the per-session doorbell filter to discard the rest: the tenant separation held, but the read did
// not, and the shared nudgeBatch cap meant one busy endpoint could crowd every other endpoint's
// todos out of the same wakeup. Governing: ADR-0022, SPEC-0003 REQ "Endpoint Ownership (Tenant
// Isolation)", SPEC-0004 REQ "In-Database Wakeups via LISTEN/NOTIFY".
func TodoReadyPayload(endpointID, queue string) string { return endpointID + ":" + queue }

// notifyTodoReady emits a best-effort LISTEN/NOTIFY wakeup on the todo_ready channel carrying the
// owning endpoint id and queue name. Correctness never depends on delivery (SPEC-0004): errors are
// swallowed by design.
func (s *Store) notifyTodoReady(ctx context.Context, endpointID, queue string) {
	// pg_notify is used (not a literal NOTIFY) so the channel payload is passed as a bound
	// parameter rather than interpolated into SQL text (Parameterized Queries Only).
	_, _ = s.pool.Exec(ctx, `SELECT pg_notify('todo_ready', $1)`, TodoReadyPayload(endpointID, queue))
}

// ClaimTodo atomically claims a specific todo for owner, setting a lease. A todo is claimable when
// it is pending, OR when its lease has expired (crash recovery, so a stopped reaper can't strand
// it) with attempts remaining, OR when its scheduled retry backoff has elapsed (so a hot worker
// need not wait for the retry scheduler's next tick — a failed todo whose next_retry_at is still in
// the future stays unclaimable). ADR-0007 claim / SPEC-0003 lease recovery + Bounded Retries.
// endpointID is the tenant scope (ADR-0022): the UPDATE is constrained to a row owned by this
// endpoint so a caller authenticated to endpoint A can never claim a todo owned by endpoint B.
// Returns ErrConflict if the todo exists but is not claimable (live claim / wrong assignee /
// exhausted / backoff pending / wrong tenant), ErrNotFound if absent.
func (s *Store) ClaimTodo(ctx context.Context, endpointID, id, owner string, ttl time.Duration) (Todo, error) {
	if err := endpointScope(endpointID); err != nil {
		return Todo{}, err
	}
	row := s.pool.QueryRow(ctx, `
		UPDATE todos SET state='claimed', owner=$3, lease_expires_at=now()+$4::interval,
			attempt=attempt+1, claimed_at=now(), next_retry_at=NULL, updated_at=now()
		WHERE id=$2 AND endpoint_id=$1 AND (assignee IS NULL OR assignee=$3)
			AND (state='pending'
				OR (state='claimed' AND lease_expires_at < now() AND attempt < max_attempts)
				OR (state='failed' AND next_retry_at IS NOT NULL AND next_retry_at <= now()
					AND attempt < max_attempts))
		RETURNING `+todoCols, endpointID, id, owner, ttl.String())
	t, err := scanTodo(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Todo{}, s.classifyMiss(ctx, endpointID, id)
	}
	if err == nil {
		s.fireTodoHook("claimed", t)
	}
	return t, err
}

// ClaimNext claims the oldest claimable todo across the allowed queues using FOR UPDATE SKIP LOCKED
// (ADR-0002), so concurrent workers never collide. A todo is claimable when it is pending, when its
// lease has expired with attempts remaining, or when its scheduled retry backoff has elapsed — the
// scan recovers expired leases and due retries directly, so a stopped reaper/scheduler can never
// strand work (SPEC-0003). A failed todo whose backoff has not elapsed is skipped. endpointID is
// the tenant scope (ADR-0022): the scan is constrained to rows owned by this endpoint. Returns
// ErrNotFound when no work is available.
func (s *Store) ClaimNext(ctx context.Context, endpointID string, queues []string, owner string, ttl time.Duration) (Todo, error) {
	if err := endpointScope(endpointID); err != nil {
		return Todo{}, err
	}
	row := s.pool.QueryRow(ctx, `
		UPDATE todos SET state='claimed', owner=$1, lease_expires_at=now()+$2::interval,
			attempt=attempt+1, claimed_at=now(), next_retry_at=NULL, updated_at=now()
		WHERE id = (
			SELECT id FROM todos
			WHERE endpoint_id=$4 AND queue = ANY($3) AND (assignee IS NULL OR assignee=$1)
				AND (state='pending'
					OR (state='claimed' AND lease_expires_at < now() AND attempt < max_attempts)
					OR (state='failed' AND next_retry_at IS NOT NULL AND next_retry_at <= now()
						AND attempt < max_attempts))
			ORDER BY created_at
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		RETURNING `+todoCols, owner, ttl.String(), queues, endpointID)
	t, err := scanTodo(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Todo{}, ErrNotFound
	}
	if err == nil {
		s.fireTodoHook("claimed", t)
	}
	return t, err
}

// HeartbeatTodo extends the visibility lease on a claimed todo (SQS ChangeMessageVisibility). Only
// the current lease owner may heartbeat (guarded by state='claimed' AND owner); it does not consume
// an attempt or change claimed_at. endpointID is the tenant scope (ADR-0022). Returns ErrConflict if
// the todo exists but is not a live claim owned by owner, ErrNotFound if absent. Governing:
// SPEC-0003 REQ "Visibility Window, Lease, Heartbeat".
func (s *Store) HeartbeatTodo(ctx context.Context, endpointID, id, owner string, ttl time.Duration) (Todo, error) {
	if err := endpointScope(endpointID); err != nil {
		return Todo{}, err
	}
	row := s.pool.QueryRow(ctx, `
		UPDATE todos SET lease_expires_at=now()+$3::interval, updated_at=now()
		WHERE id=$2 AND endpoint_id=$1 AND state='claimed' AND owner=$4
		RETURNING `+todoCols, endpointID, id, ttl.String(), owner)
	t, err := scanTodo(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Todo{}, s.classifyMiss(ctx, endpointID, id)
	}
	return t, err
}

// CompleteTodo acks a claimed todo owned by owner. endpointID is the tenant scope (ADR-0022).
// ADR-0007 complete.
func (s *Store) CompleteTodo(ctx context.Context, endpointID, id, owner string, result []byte) (Todo, error) {
	if err := endpointScope(endpointID); err != nil {
		return Todo{}, err
	}
	row := s.pool.QueryRow(ctx, `
		UPDATE todos SET state='done', result=$4, completed_at=now(), updated_at=now()
		WHERE id=$2 AND endpoint_id=$1 AND state='claimed' AND owner=$3
		RETURNING `+todoCols, endpointID, id, owner, result)
	t, err := scanTodo(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Todo{}, s.classifyMiss(ctx, endpointID, id)
	}
	if err == nil {
		s.fireTodoHook("done", t)
	}
	return t, err
}

// FailTodo fails a claimed todo. Below the attempt cap it schedules a retry with exponential
// backoff — the todo parks in `failed` with next_retry_at = now() + retryBackoff(attempt) (30s
// base, doubling, 15m cap) and re-enters `pending` only when the backoff elapses (retry scheduler,
// or the claim scan once due). At the cap it dead-letters (`failed` with next_retry_at NULL). The
// owner is kept on the failed row so the UI can show which agent it failed under; the re-queue
// clears it. endpointID is the tenant scope (ADR-0022). Governing: SPEC-0003 REQ "Bounded Retries
// via max_attempts" (scheduled backoff); ADR-0007 fail.
func (s *Store) FailTodo(ctx context.Context, endpointID, id, owner string, result []byte) (Todo, error) {
	if err := endpointScope(endpointID); err != nil {
		return Todo{}, err
	}
	row := s.pool.QueryRow(ctx, `
		UPDATE todos SET
			state = 'failed',
			next_retry_at = CASE WHEN attempt >= max_attempts THEN NULL
				ELSE now() + make_interval(secs =>
					LEAST($5::float8 * power(2, GREATEST(attempt, 1) - 1), $6::float8)) END,
			lease_expires_at = NULL, result = $4, updated_at = now()
		WHERE id=$2 AND endpoint_id=$1 AND state='claimed' AND owner=$3
		RETURNING `+todoCols, endpointID, id, owner, result,
		retryBackoffBase.Seconds(), retryBackoffCap.Seconds())
	t, err := scanTodo(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Todo{}, s.classifyMiss(ctx, endpointID, id)
	}
	if err == nil {
		// Both outcomes commit as 'failed'; the hook payload's NextRetryAt distinguishes a
		// scheduled retry (countdown UI) from a dead-letter (retry is manual-only).
		s.fireTodoHook(t.State, t)
	}
	return t, err
}

// RequeueDueRetries re-queues failed todos whose scheduled retry backoff has elapsed — the
// background retry scheduler that runs alongside the lease reaper (server.go).
//
// DELIBERATELY NOT endpoint-scoped. It is a system-wide maintenance sweep on a timer, with no
// authenticated endpoint context to scope TO: no caller, no credential, no tenant. It reads no
// todo out to anyone and moves each row only within its own lifecycle (failed → pending), so it
// cannot leak or transfer work across tenants — the per-row wakeup it emits is already scoped by
// the row's own endpoint_id. Scoping it would mean either inventing a tenant or iterating every
// endpoint to reproduce the same set, trading a correct sweep for a slower identical one. The
// tenant boundary is enforced where work is READ and CLAIMED, not where it ages. SPEC-0003 REQ
// "Endpoint Ownership (Tenant Isolation)" is amended to state this exemption explicitly.
// Governing: ADR-0022, SPEC-0003 REQ "Endpoint Ownership (Tenant Isolation)" (background-sweep
// exemption), REQ "Bounded Retries via max_attempts".
//
// Each re-queued row
// returns to `pending` with owner/lease/next_retry_at cleared, fires the transition hook (the UI's
// re-surface flash), and nudges idle workers via the todo_ready wakeup (SPEC-0004; best-effort,
// correctness never depends on delivery). Returns the number of todos re-queued.
// Governing: SPEC-0003 REQ "Bounded Retries via max_attempts" (scheduled backoff).
func (s *Store) RequeueDueRetries(ctx context.Context) (int64, error) {
	rows, err := s.pool.Query(ctx, `
		UPDATE todos SET state='pending', owner=NULL, lease_expires_at=NULL, next_retry_at=NULL,
			updated_at=now()
		WHERE state='failed' AND next_retry_at IS NOT NULL AND next_retry_at <= now()
		RETURNING `+todoCols)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var requeued []Todo
	for rows.Next() {
		t, err := scanTodo(rows)
		if err != nil {
			return 0, err
		}
		requeued = append(requeued, t)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	// One wakeup per (endpoint, queue), not per queue: the todo_ready payload is now endpoint-scoped
	// (TodoReadyPayload), so collapsing on queue alone would emit a single notification naming
	// whichever endpoint happened to come first and silently strand every other tenant re-queued in
	// the same sweep. Governing: ADR-0022, SPEC-0004 REQ "In-Database Wakeups via LISTEN/NOTIFY".
	nudged := map[string]bool{}
	for _, t := range requeued {
		s.fireTodoHook(t.State, t)
		k := TodoReadyPayload(t.EndpointID, t.Queue)
		if !nudged[k] {
			nudged[k] = true
			s.notifyTodoReady(ctx, t.EndpointID, t.Queue)
		}
	}
	return int64(len(requeued)), nil
}

// RetryTodo re-enqueues a failed todo immediately: the explicit agent "Retry now" that re-queues a
// dead-letter (SPEC-0003 lifecycle: failed → pending) and doubles as the manual override of a
// scheduled backoff (the retry window is discarded, next_retry_at cleared). It resets the attempt
// budget and clears owner/lease/result so the todo gets a fresh set of tries. endpointID is the
// tenant scope (ADR-0022): re-queueing another endpoint's dead-letter would resurrect that tenant's
// work, so the UPDATE is constrained to a row this endpoint owns. The operator path is
// RetryTodoAnyEndpoint. Returns ErrConflict if the todo exists within this endpoint but is not
// failed (terminal states are otherwise final), ErrNotFound if absent or foreign.
func (s *Store) RetryTodo(ctx context.Context, endpointID, id string) (Todo, error) {
	if err := endpointScope(endpointID); err != nil {
		return Todo{}, err
	}
	row := s.pool.QueryRow(ctx, `
		UPDATE todos SET state='pending', owner=NULL, lease_expires_at=NULL, attempt=0,
			result=NULL, claimed_at=NULL, completed_at=NULL, next_retry_at=NULL, updated_at=now()
		WHERE id=$2 AND endpoint_id=$1 AND state='failed'
		RETURNING `+todoCols, endpointID, id)
	t, err := scanTodo(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Todo{}, s.classifyMiss(ctx, endpointID, id)
	}
	if err == nil {
		s.fireTodoHook("pending", t)
	}
	return t, err
}

// operatorOwns is the tenant predicate the OperatorOwned mutations below carry. It is a correlated
// EXISTS rather than a join because these are UPDATEs on `todos` and the predicate has to constrain
// the row being written, not a projection of it.
//
// These are the paths that used to be named …AnyEndpoint, and the old name described the old
// behaviour exactly: the doc said "the operator can see and act on every todo regardless of
// tenant". That was true when Switchboard had one operator. With six it meant any signed-in human
// could claim, complete, fail, retry or release anyone else's work from the Board. The rename is
// part of the fix — the name is what made the call sites look correct.
//
// The agent-facing siblings (ClaimTodo, CompleteTodo, …) are unchanged: they take an endpointID and
// were always scoped. Only the human-session paths were open.
// Governing: SPEC-0007 REQ "Human as Accountable Principal"; ADR-0022.
const operatorOwns = `
			AND EXISTS (SELECT 1 FROM endpoints ep
			              JOIN agents ag ON ag.id = ep.agent_id
			             WHERE ep.id = todos.endpoint_id AND ag.owner_human_id = `

// RetryTodoOperatorOwned is the OPERATOR-ONLY variant of RetryTodo (the Board's "Retry now" action,
// authenticated by the human session rather than an endpoint credential). The agent path MUST use
// RetryTodo. Governing: ADR-0022, SPEC-0003 lifecycle failed → pending.
func (s *Store) RetryTodoOperatorOwned(ctx context.Context, ownerHumanID, id string) (Todo, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE todos SET state='pending', owner=NULL, lease_expires_at=NULL, attempt=0,
			result=NULL, claimed_at=NULL, completed_at=NULL, next_retry_at=NULL, updated_at=now()
		WHERE id=$1 AND state='failed'`+operatorOwns+`$2)
		RETURNING `+todoCols, id, ownerHumanID)
	t, err := scanTodo(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Todo{}, s.classifyMissForHuman(ctx, ownerHumanID, id)
	}
	if err == nil {
		s.fireTodoHook("pending", t)
	}
	return t, err
}

// ReleaseTodo returns a claimed todo to pending immediately — the "Release" action, which hands
// work back to the queue without waiting for the lease to expire. Only the current lease owner may
// release; it clears owner and lease but does NOT consume or reset attempts (unlike fail/retry).
// endpointID is the tenant scope (ADR-0022): the owner check alone is not a tenant boundary (owner
// is a free-form string), so the UPDATE is additionally constrained to a row this endpoint owns.
// The operator path is ReleaseTodoOperatorOwned. Returns ErrConflict if the todo exists within this
// endpoint but is not a live claim owned by owner, ErrNotFound if absent or foreign.
// Governing: SPEC-0013 REQ "Todo Detail Drawer" (Release), SPEC-0003 lease semantics.
func (s *Store) ReleaseTodo(ctx context.Context, endpointID, id, owner string) (Todo, error) {
	if err := endpointScope(endpointID); err != nil {
		return Todo{}, err
	}
	row := s.pool.QueryRow(ctx, `
		UPDATE todos SET state='pending', owner=NULL, lease_expires_at=NULL, updated_at=now()
		WHERE id=$2 AND endpoint_id=$1 AND state='claimed' AND owner=$3
		RETURNING `+todoCols, endpointID, id, owner)
	t, err := scanTodo(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Todo{}, s.classifyMiss(ctx, endpointID, id)
	}
	if err == nil {
		s.fireTodoHook("pending", t)
	}
	return t, err
}

// ReleaseTodoOperatorOwned is the OPERATOR-ONLY variant of ReleaseTodo (the Board's Release action,
// authenticated by the human session rather than an endpoint credential). The agent path MUST use
// ReleaseTodo. Governing: ADR-0022, SPEC-0013 REQ "Todo Detail Drawer" (Release).
func (s *Store) ReleaseTodoOperatorOwned(ctx context.Context, ownerHumanID, id, owner string) (Todo, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE todos SET state='pending', owner=NULL, lease_expires_at=NULL, updated_at=now()
		WHERE id=$1 AND state='claimed' AND owner=$2`+operatorOwns+`$3)
		RETURNING `+todoCols, id, owner, ownerHumanID)
	t, err := scanTodo(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Todo{}, s.classifyMissForHuman(ctx, ownerHumanID, id)
	}
	if err == nil {
		s.fireTodoHook("pending", t)
	}
	return t, err
}

// ListTodos returns todos in the allowed queues for one endpoint, optionally filtered by state,
// newest first. endpointID is the tenant scope (ADR-0022): only todos pinned to this endpoint are
// returned, regardless of queue-name collisions across endpoints.
func (s *Store) ListTodos(ctx context.Context, endpointID string, queues []string, state string, limit int) ([]Todo, error) {
	// An absent or malformed scope owns nothing, which is exactly an empty list — not a uuid cast
	// error (endpointScope).
	if err := endpointScope(endpointID); err != nil {
		return nil, nil
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `SELECT `+todoCols+` FROM todos
		WHERE endpoint_id = $1 AND queue = ANY($2) AND ($3 = '' OR state = $3)
		ORDER BY created_at DESC LIMIT $4`, endpointID, queues, state, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Todo
	for rows.Next() {
		t, err := scanTodo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// PendingDoorbellTodos returns pending todos for one endpoint on queue that are eligible for a
// channel push under the SPEC-0011 sender gate: only todos whose delivery event exists AND passed
// per-source verification, oldest first (the claim-scan order), capped at limit. endpointID is the
// tenant scope (ADR-0022). The todo_ready LISTEN loop (internal/server/listen.go) uses it to
// re-ring the MCP doorbell for work enqueued outside this process's store hooks — the gate lives in
// SQL so the wakeup path can never push an unverified or event-less todo the HTTP path would have
// withheld. Governing: SPEC-0004 REQ "In-Database Wakeups via LISTEN/NOTIFY", SPEC-0011 REQ
// "Sender Gate and Injection Safety", SPEC-0003 REQ "Endpoint Ownership (Tenant Isolation)".
func (s *Store) PendingDoorbellTodos(ctx context.Context, endpointID, queue string, limit int) ([]Todo, error) {
	// An absent or malformed scope owns nothing (endpointScope) — no todos, no doorbell.
	if err := endpointScope(endpointID); err != nil {
		return nil, nil
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `SELECT `+todoCols+` FROM todos
		WHERE endpoint_id = $1 AND queue = $2 AND state = 'pending' AND event_id IS NOT NULL
		  AND EXISTS (SELECT 1 FROM events e WHERE e.id = todos.event_id AND e.verified)
		ORDER BY created_at LIMIT $3`, endpointID, queue, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Todo
	for rows.Next() {
		t, err := scanTodo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// PendingDoorbellTodosByQueue is the cross-endpoint LEGACY-PAYLOAD variant used by the LISTEN loop
// only when a todo_ready notification carries a bare queue name with no endpoint id — a
// notification emitted by an older process mid-deploy, or by hand from psql. The current payload is
// endpoint-scoped (TodoReadyPayload) and the listener prefers PendingDoorbellTodos, which reads one
// tenant's rows; this fallback reads every endpoint's push-eligible rows on the queue and leans on
// the doorbell publisher (PublishTodoReady) for per-session endpoint scoping. That is correct but
// unscoped and subject to the shared batch cap, so it is deliberately the fallback, not the path.
// Governing: SPEC-0004 REQ "In-Database Wakeups via LISTEN/NOTIFY", SPEC-0011 REQ "Sender Gate
// and Injection Safety", ADR-0022.
func (s *Store) PendingDoorbellTodosByQueue(ctx context.Context, queue string, limit int) ([]Todo, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `SELECT `+todoCols+` FROM todos
		WHERE queue = $1 AND state = 'pending' AND event_id IS NOT NULL
		  AND EXISTS (SELECT 1 FROM events e WHERE e.id = todos.event_id AND e.verified)
		ORDER BY created_at LIMIT $2`, queue, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Todo
	for rows.Next() {
		t, err := scanTodo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetTodo returns one todo by id. endpointID is the tenant scope (ADR-0022): a todo pinned to
// another endpoint is not visible through this lookup.
func (s *Store) GetTodo(ctx context.Context, endpointID, id string) (Todo, error) {
	if err := endpointScope(endpointID); err != nil {
		return Todo{}, err
	}
	row := s.pool.QueryRow(ctx, `SELECT `+todoCols+` FROM todos WHERE id = $1 AND endpoint_id = $2`, id, endpointID)
	t, err := scanTodo(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Todo{}, ErrNotFound
	}
	return t, err
}

// GetTodoOperatorOwned returns one todo by id from the Board, scoped to the human who owns it.
// Another human's todo is ErrNotFound, not a permission error — see GetTodoItem for why the
// distinction is load-bearing. The agent-facing path MUST use GetTodo, which enforces the endpoint
// scope. Governing: ADR-0022; SPEC-0007 REQ "Human as Accountable Principal".
func (s *Store) GetTodoOperatorOwned(ctx context.Context, ownerHumanID, id string) (Todo, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+todoCols+` FROM todos WHERE id = $1`+operatorOwns+`$2)`, id, ownerHumanID)
	t, err := scanTodo(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Todo{}, ErrNotFound
	}
	return t, err
}

// ClaimTodoOperatorOwned is the OPERATOR-ONLY variant of ClaimTodo that resolves the endpoint from
// the todo row instead of requiring it up front (the Board's Claim action is authenticated by the
// human session, not by an endpoint credential). The agent path MUST use ClaimTodo. Governing:
// ADR-0022, SPEC-0003 claim-under-lease semantics.
func (s *Store) ClaimTodoOperatorOwned(ctx context.Context, ownerHumanID, id, owner string, ttl time.Duration) (Todo, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE todos SET state='claimed', owner=$2, lease_expires_at=now()+$3::interval,
			attempt=attempt+1, claimed_at=now(), next_retry_at=NULL, updated_at=now()
		WHERE id=$1 AND (assignee IS NULL OR assignee=$2)
			AND (state='pending'
				OR (state='claimed' AND lease_expires_at < now() AND attempt < max_attempts)
				OR (state='failed' AND next_retry_at IS NOT NULL AND next_retry_at <= now()
					AND attempt < max_attempts))`+operatorOwns+`$4)
		RETURNING `+todoCols, id, owner, ttl.String(), ownerHumanID)
	t, err := scanTodo(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Todo{}, s.classifyMissForHuman(ctx, ownerHumanID, id)
	}
	if err == nil {
		s.fireTodoHook("claimed", t)
	}
	return t, err
}

// CompleteTodoOperatorOwned is the OPERATOR-ONLY variant of CompleteTodo. The agent path MUST use
// CompleteTodo. Governing: ADR-0022, ADR-0007 complete.
func (s *Store) CompleteTodoOperatorOwned(ctx context.Context, ownerHumanID, id, owner string, result []byte) (Todo, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE todos SET state='done', result=$3, completed_at=now(), updated_at=now()
		WHERE id=$1 AND state='claimed' AND owner=$2`+operatorOwns+`$4)
		RETURNING `+todoCols, id, owner, result, ownerHumanID)
	t, err := scanTodo(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Todo{}, s.classifyMissForHuman(ctx, ownerHumanID, id)
	}
	if err == nil {
		s.fireTodoHook("done", t)
	}
	return t, err
}

// FailTodoOperatorOwned is the OPERATOR-ONLY variant of FailTodo. The agent path MUST use FailTodo.
// Governing: ADR-0022, SPEC-0003 fail.
func (s *Store) FailTodoOperatorOwned(ctx context.Context, ownerHumanID, id, owner string, result []byte) (Todo, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE todos SET
			state = 'failed',
			next_retry_at = CASE WHEN attempt >= max_attempts THEN NULL
				ELSE now() + make_interval(secs =>
					LEAST($4::float8 * power(2, GREATEST(attempt, 1) - 1), $5::float8)) END,
			lease_expires_at = NULL, result = $3, updated_at = now()
		WHERE id=$1 AND state='claimed' AND owner=$2`+operatorOwns+`$6)
		RETURNING `+todoCols, id, owner, result,
		retryBackoffBase.Seconds(), retryBackoffCap.Seconds(), ownerHumanID)
	t, err := scanTodo(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Todo{}, s.classifyMissForHuman(ctx, ownerHumanID, id)
	}
	if err == nil {
		s.fireTodoHook(t.State, t)
	}
	return t, err
}

// HeartbeatTodoOperatorOwned is the OPERATOR-ONLY variant of HeartbeatTodo. The agent path MUST use
// HeartbeatTodo. Governing: ADR-0022, SPEC-0003 heartbeat.
func (s *Store) HeartbeatTodoOperatorOwned(ctx context.Context, ownerHumanID, id, owner string, ttl time.Duration) (Todo, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE todos SET lease_expires_at=now()+$3::interval, updated_at=now()
		WHERE id=$1 AND state='claimed' AND owner=$2`+operatorOwns+`$4)
		RETURNING `+todoCols, id, owner, ttl.String(), ownerHumanID)
	t, err := scanTodo(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Todo{}, s.classifyMissForHuman(ctx, ownerHumanID, id)
	}
	return t, err
}

// ReapExpired requeues (or dead-letters) todos whose lease has expired — crash safety (ADR-0002 reaper).
//
// DELIBERATELY NOT endpoint-scoped, for the same reason as RequeueDueRetries: a timer-driven
// system sweep has no authenticated endpoint context, returns no todo to any caller, and moves
// each row only within its own lifecycle. Scoping it would make crash recovery depend on someone
// being logged in. SPEC-0003 REQ "Endpoint Ownership (Tenant Isolation)" is amended to exempt
// background sweeps explicitly. Governing: ADR-0022, SPEC-0003 (background-sweep exemption).
//
// Returns the number of todos reaped. Each reaped row fires the transition hook with its committed
// outcome (pending = re-surfaced, failed = dead-lettered) so the UI can announce reaper re-surfaces.
// Governing: SPEC-0013 REQ "Live Updates and Toasts" (reaper re-surface is visible).
func (s *Store) ReapExpired(ctx context.Context) (int64, error) {
	rows, err := s.pool.Query(ctx, `
		UPDATE todos SET
			state = CASE WHEN attempt >= max_attempts THEN 'failed' ELSE 'pending' END,
			owner = NULL, lease_expires_at = NULL, updated_at = now()
		WHERE state='claimed' AND lease_expires_at < now()
		RETURNING `+todoCols)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var reaped []Todo
	for rows.Next() {
		t, err := scanTodo(rows)
		if err != nil {
			return 0, err
		}
		reaped = append(reaped, t)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, t := range reaped {
		s.fireTodoHook(t.State, t)
	}
	return int64(len(reaped)), nil
}

// classifyMiss distinguishes "todo absent" (ErrNotFound) from "todo present but not in the expected
// state / not owned" (ErrConflict) after a conditional UPDATE affected no rows.
//
// The existence probe is TENANT-SCOPED: endpointID narrows it to rows the caller owns, so a todo
// belonging to a different endpoint classifies as ErrNotFound, exactly like an id that was never
// minted. Probing globally made this function a cross-tenant existence oracle — endpoint B could
// call ClaimTodo with a guessed id and read the returned error to learn whether endpoint A owned
// it (ErrConflict = "exists", ErrNotFound = "does not"), leaking the membership of A's id space
// through a path that correctly refused to return the row itself.
//
// The OPERATOR paths must not use this one. They used to: the carve-out here said an empty
// endpointID meant "an operator twin, which legitimately sees every tenant and wants the global
// probe", which was true of a single-operator instance and false the moment there were six. It
// left every Board action a cross-tenant existence oracle even after the UPDATE itself was
// scoped — the row was correctly refused and the error still confirmed it existed. They use
// classifyMissForHuman instead.
//
// The global probe still has callers: the A2A state primitives in todos_a2a.go (CancelTodo,
// RejectTodo and the two interrupt transitions). Those are not a live leak today — nothing outside
// the store package calls them yet, they are built ahead of the A2A handler — but they MUST take a
// tenant scope before that handler lands, or they reintroduce this oracle on a new surface. Tracked
// on #176.
// Governing: ADR-0022, SPEC-0003 REQ "Endpoint Ownership (Tenant Isolation)".
func (s *Store) classifyMiss(ctx context.Context, endpointID, id string) error {
	var exists bool
	var err error
	if endpointID == "" {
		err = s.pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM todos WHERE id=$1)`, id).Scan(&exists)
	} else {
		err = s.pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM todos WHERE id=$1 AND endpoint_id=$2)`, id, endpointID).Scan(&exists)
	}
	if err != nil {
		return err
	}
	if exists {
		return ErrConflict
	}
	return ErrNotFound
}

// classifyMissForHuman is classifyMiss for the operator (human-session) paths: same
// present-but-wrong-state vs absent distinction, with the existence probe narrowed to the todos
// that human owns.
//
// Scoping the probe is not belt-and-braces on top of the scoped UPDATE — it is the half that
// closes the oracle. A scoped UPDATE alone still answers "conflict" for another tenant's live
// todo, and "conflict" vs "not found" is exactly the one bit an attacker needs to enumerate
// another tenant's ids. Governing: SPEC-0007 REQ "Human as Accountable Principal"; ADR-0022.
func (s *Store) classifyMissForHuman(ctx context.Context, ownerHumanID, id string) error {
	var exists bool
	if err := s.pool.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM todos t
		                JOIN endpoints ep ON ep.id = t.endpoint_id
		                JOIN agents    ag ON ag.id = ep.agent_id AND ag.owner_human_id = $2
		               WHERE t.id = $1)`, id, ownerHumanID).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return ErrConflict
	}
	return ErrNotFound
}

// EventInput is the input to InsertEvent.
type EventInput struct {
	Source       string
	Family       string
	EventType    string
	ExternalID   string
	TrustMode    string
	Verified     bool
	VerifyDetail string
	ContentType  string
	Headers      []byte // sanitized JSON
	Payload      []byte
	SourceIP     string
	// WebhookID is the self-managed webhook the delivery arrived on ("" for other receivers), and
	// RoutingTrace how the routing stage decided it (nil when the delivery was not routed). Both are
	// written with the row so history can never show a delivery without its route.
	// Governing: SPEC-0020 REQ "Routing Trace", REQ "Drop Action Semantics".
	WebhookID    string
	RoutingTrace []byte
}

// InsertEvent records an accepted delivery, deduping on (source, external_id). Returns the event id
// (existing id on a duplicate delivery). Newly inserted events fire the event hook.
func (s *Store) InsertEvent(ctx context.Context, e EventInput) (int64, error) {
	ev, inserted, err := insertEvent(ctx, s.pool, e)
	if err != nil {
		return 0, err
	}
	if inserted {
		s.fireEventHook(ev)
	}
	return ev.ID, nil
}

// insertEvent is the querier-based core of InsertEvent, runnable on the pool or inside a
// transaction. The returned bool reports whether a NEW row was inserted (false = duplicate
// delivery, existing row returned). The EventSummary carries the fields the Board feed renders.
func insertEvent(ctx context.Context, q querier, e EventInput) (EventSummary, bool, error) {
	ev := EventSummary{Source: e.Source, EventType: e.EventType, TrustMode: e.TrustMode}
	err := q.QueryRow(ctx, `
		INSERT INTO events (source, family, event_type, external_id, trust_mode, verified, verify_detail,
			content_type, headers, payload, payload_size, source_ip, webhook_id, routing_trace)
		VALUES ($1,$2,NULLIF($3,''),NULLIF($4,''),$5,$6,NULLIF($7,''),NULLIF($8,''),$9,$10,$11,NULLIF($12,'')::inet,
			NULLIF($13,'')::uuid, $14)
		ON CONFLICT (source, external_id) WHERE external_id IS NOT NULL DO NOTHING
		RETURNING id, received_at`,
		e.Source, e.Family, e.EventType, e.ExternalID, e.TrustMode, e.Verified, e.VerifyDetail,
		e.ContentType, e.Headers, e.Payload, len(e.Payload), e.SourceIP, e.WebhookID, e.RoutingTrace).Scan(&ev.ID, &ev.ReceivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// Duplicate delivery — fetch the existing row.
		if err2 := q.QueryRow(ctx,
			`SELECT id, received_at FROM events WHERE source=$1 AND external_id=$2`, e.Source, e.ExternalID,
		).Scan(&ev.ID, &ev.ReceivedAt); err2 != nil {
			return EventSummary{}, false, err2
		}
		return ev, false, nil
	}
	if err != nil {
		return EventSummary{}, false, err
	}
	return ev, true, nil
}

// Doorbell heartbeat
//
// A doorbell is a hint and the queue is the ledger (ADR-0013) — but under push-only delivery that
// promise was only half true. A push that arrived while every worker was mid-turn, or was dropped
// by a transport fault, or landed during a restart, was never repeated, so the todo sat pending
// forever with nothing to surface it again. Fifty rows accumulated exactly that way, every one of
// them work somebody asked for.
//
// This is the other half: pending rows that nobody claimed get rung again, on a widening interval,
// a bounded number of times.
//
// @joestump-agent 09/06/2026 - Added after the push path was fixed and the backlog it had already
// created turned out to have no way to drain itself.

// ringBackoff is the delay before the Nth re-ring of an unclaimed todo (1-based: attempt 1 has
// already happened at creation). It widens so a todo nobody wants costs a handful of pushes rather
// than one per sweep forever, and stops entirely at ringMaxAttempts.
//
// Chosen against how these consumers actually behave: a worker mid-turn on a PR review is busy for
// minutes, not seconds, so the first retry waits long enough to outlast an ordinary turn instead of
// arriving while the same worker is still head-down.
func ringBackoff(attempt int) time.Duration {
	switch {
	case attempt <= 1:
		return 5 * time.Minute
	case attempt == 2:
		return 20 * time.Minute
	case attempt == 3:
		return time.Hour
	default:
		return 6 * time.Hour
	}
}

const (
	// ringMaxAttempts bounds re-ringing. After this many pushes the row stays pending and visible
	// on the board but stops costing a model turn: if five doorbells over seven hours have not got
	// it picked up, a sixth is not the answer and something else is wrong.
	ringMaxAttempts = 5

	// ringSweepLimit caps how many todos one sweep re-rings. A backlog is exactly when this matters
	// — waking every worker for fifty rows at once is a token bomb, and each push costs a model
	// turn. Oldest-first ordering means a small limit still drains steadily.
	ringSweepLimit = 3
)

// RingUnclaimed returns pending todos whose doorbell is due to be repeated, marking them rung in
// the same statement so two sweeps only rarely select the same row: the lock step re-checks ids
// picked under an earlier snapshot rather than re-running the eligibility predicates at lock time,
// so a concurrent ring committing between the snapshot and the FOR UPDATE can double-ring once.
// That narrowed, best-effort exclusivity is acceptable — the system is best-effort and the
// doorbell is a hint.
//
// The sweep round-robins across endpoints: each todo is ranked within its OWN endpoint's backlog
// and the pick orders by that rank first, so every endpoint contributes its best candidate before
// any endpoint contributes its second. A single deep backlog can no longer absorb the whole budget
// and starve the endpoint someone is actually waiting on, while an endpoint alone in having work
// still gets the full budget (the higher ranks are all it has to offer).
//
// Only todos on an ACTIVE endpoint are candidates. A revoked endpoint's rows are undrainable by
// construction — the credential no longer authenticates, so no session can ever exist to receive
// the push — and because older endpoints hold the oldest rows, an unguarded sweep reaches for them
// first. Without this join the entire ring budget is spent on work nobody can ever do:
// observed in production with 1,442 pending rows across two revoked endpoints absorbing every
// sweep while live endpoints, holding the work someone had actually asked for, were never reached.
// The symptom was silent — each ring resolved to no session, so PublishTodoReady returned early
// and logged neither a delivery nor a drop.
//
// DELIBERATELY NOT endpoint-scoped, for the same reason as RequeueDueRetries: it is a system-wide
// maintenance sweep on a timer with no authenticated caller to scope to. It moves no row between
// tenants and reads nothing out to anyone — each returned row carries its own endpoint_id, and the
// publish path filters on that, so the tenant boundary is enforced where the work is delivered and
// claimed rather than where it ages.
//
// Governing: ADR-0013 (queue is the ledger, push is a hint); SPEC-0003 REQ "Endpoint Ownership
// (Tenant Isolation)" background-sweep exemption; SPEC-0011 REQ "Best-Effort Lossy Delivery".
func (s *Store) RingUnclaimed(ctx context.Context) ([]Todo, error) {
	rows, err := s.pool.Query(ctx, `
		WITH ranked AS (
			SELECT t.id,
			       t.last_ringed_at,
			       t.created_at,
			       -- Rank within each endpoint's own backlog. Ordering the pick by this rank
			       -- first is what makes the sweep round-robin: every endpoint contributes its
			       -- best candidate before any endpoint contributes its second. A single
			       -- endpoint sitting on hundreds of rows can no longer absorb the whole budget
			       -- and starve the endpoint someone is actually waiting on — while an endpoint
			       -- that is alone in having work still gets the full budget, because the higher
			       -- ranks are all it has to offer.
			       row_number() OVER (
			         PARTITION BY t.endpoint_id
			         ORDER BY t.last_ringed_at NULLS FIRST, t.created_at
			       ) AS rn
			FROM todos t
			-- A revoked endpoint's credential no longer authenticates, so no session can ever
			-- receive its pushes. Its todos are undrainable by construction, and because
			-- revocation happens to old endpoints they are also the oldest rows in the table —
			-- exactly the rows a globally oldest-first sweep reaches for. Excluded at the source.
			JOIN endpoints ep ON ep.id = t.endpoint_id AND ep.state = 'active'
			WHERE t.state = 'pending'
			  AND t.ring_attempts < $1
			  -- Sender gate (SPEC-0011): only a todo whose delivery event exists AND passed
			  -- per-source verification — or came from a token-trust self-managed webhook,
			  -- whose credential is the unguessable ingest URL (SPEC-0006) — is ever
			  -- doorbell-eligible. The gate must live in the wakeup query, not the publisher,
			  -- so it cannot be bypassed: the reaper republishes through the same path a fresh
			  -- delivery uses, and the create path deliberately withholds the doorbell from
			  -- unverified (open-trust) and event-less todos, which degrade to pull. This is
			  -- the same predicate the create path applies when deciding whether to ring (see
			  -- the CreateEventTodos gates above); PendingDoorbellTodos, the pull read, is
			  -- deliberately stricter (e.verified only) since a human-driven pull should only
			  -- surface verified deliveries.
			  AND t.event_id IS NOT NULL
			  AND EXISTS (SELECT 1 FROM events ev WHERE ev.id = t.event_id AND (ev.verified OR ev.trust_mode = 'token'))
			  AND (
			    -- Never rung by this mechanism: wait out the first backoff from creation, so a
			    -- todo whose original doorbell is still in flight is not immediately doubled.
			    (t.last_ringed_at IS NULL AND t.created_at < now() - $2::interval)
			    OR t.last_ringed_at < now() - (
			      CASE t.ring_attempts
			        WHEN 1 THEN $3::interval
			        WHEN 2 THEN $4::interval
			        WHEN 3 THEN $5::interval
			        ELSE $6::interval
			      END
			    )
			  )
		),
		picked AS (
			SELECT id FROM ranked
			ORDER BY rn, last_ringed_at NULLS FIRST, created_at
			LIMIT $7
		)
		UPDATE todos SET ring_attempts = ring_attempts + 1, last_ringed_at = now()
		WHERE id IN (
			-- Re-select through a plain scan so SKIP LOCKED still applies: a window function
			-- cannot be combined with FOR UPDATE, and dropping the lock would let two sweeps
			-- (or two instances) ring the same todo twice. Re-check the claim guard at lock
			-- time so a todo claimed between the pick and the lock is not rung anyway.
			SELECT id FROM todos WHERE id IN (SELECT id FROM picked) AND state = 'pending'
			FOR UPDATE SKIP LOCKED
		)
		RETURNING `+todoCols,
		ringMaxAttempts,
		ringBackoff(1), ringBackoff(2), ringBackoff(3), ringBackoff(4), ringBackoff(5),
		ringSweepLimit)
	if err != nil {
		return nil, fmt.Errorf("store: ring unclaimed: %w", err)
	}
	defer rows.Close()
	var out []Todo
	for rows.Next() {
		t, err := scanTodo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// deadLetterEndpointTodos is the todo half of the revocation cascade, run inside the SAME
// transaction that kills the endpoint rows (RevokeEndpoint, ExpireEndpoints, RevokeFriendEdge).
//
// A revoked endpoint's credential no longer authenticates, so no session can ever claim its
// pending work again: those todos are undrainable by construction. Left pending they are not
// merely untidy — they are load-bearing garbage. They accumulate at the OLD end of the table (an
// endpoint is revoked after a life of receiving work), so every oldest-first sweep reaches for
// them first, and a doorbell that resolves to no session logs nothing at all. In production 1,442
// such rows silently absorbed the entire heartbeat budget while live endpoints holding real work
// were never rung (#169).
//
// They dead-letter rather than retry: next_retry_at stays NULL because there is no future in which
// this endpoint drains them. The result records why, so the row explains itself to whoever finds it.
// Governing: SPEC-0007 REQ "Instant, Total Revocation"; SPEC-0016 REQ "Revocation Cascade".
func deadLetterEndpointTodos(ctx context.Context, q querier, endpointIDs []string) error {
	if len(endpointIDs) == 0 {
		return nil
	}
	if _, err := q.Exec(ctx, `
		UPDATE todos SET
			state = 'failed',
			next_retry_at = NULL,
			lease_expires_at = NULL,
			owner = NULL,
			result = jsonb_build_object(
				'error', 'endpoint_revoked',
				'detail', 'the endpoint this todo was routed to was revoked; no session can claim it'
			),
			updated_at = now(),
			completed_at = now()
		WHERE endpoint_id = ANY($1::uuid[]) AND state IN ('pending', 'claimed', 'input-required', 'auth-required')`,
		endpointIDs); err != nil {
		return fmt.Errorf("store: dead-letter endpoint todos: %w", err)
	}
	return nil
}
