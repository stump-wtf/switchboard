package store

import (
	"context"
	"errors"
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
type Todo struct {
	ID             string
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
}

const todoCols = `id, queue, COALESCE(source,''), COALESCE(kind,''), title, payload, event_id,
	COALESCE(idempotency_key,''), COALESCE(assignee,''), state, COALESCE(owner,''),
	lease_expires_at, attempt, max_attempts, result, created_at, claimed_at, completed_at`

func scanTodo(row pgx.Row) (Todo, error) {
	var t Todo
	err := row.Scan(&t.ID, &t.Queue, &t.Source, &t.Kind, &t.Title, &t.Payload, &t.EventID,
		&t.IdempotencyKey, &t.Assignee, &t.State, &t.Owner, &t.LeaseExpiresAt, &t.Attempt,
		&t.MaxAttempts, &t.Result, &t.CreatedAt, &t.ClaimedAt, &t.CompletedAt)
	return t, err
}

// CreateTodoParams are the inputs to CreateTodo.
type CreateTodoParams struct {
	Queue          string
	Source         string
	Kind           string
	Title          string
	Payload        []byte
	EventID        *int64
	IdempotencyKey string
	Assignee       string
}

// CreateTodo inserts a todo, deduping on (queue, idempotency_key) among non-terminal rows. The bool
// reports whether a new row was created (false = an existing non-terminal todo already covers it). ADR-0007.
func (s *Store) CreateTodo(ctx context.Context, p CreateTodoParams) (Todo, bool, error) {
	t, created, err := createTodo(ctx, s.pool, p)
	if err == nil && created {
		// Governing: SPEC-0004 REQ "In-Database Wakeups via LISTEN/NOTIFY". Best-effort nudge so an
		// idle worker/UI wakes without polling. The durable queue is the source of truth, so a lost
		// NOTIFY costs only latency, never work — hence the error here is deliberately ignored.
		s.notifyTodoReady(ctx, t.Queue)
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
	defer tx.Rollback(ctx) // no-op once committed; rolls back on any early return

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
	// lifecycle order (event_received → todo_created). Governing: SPEC-0013 REQ "Board View —
	// Live Incoming Lines".
	if inserted {
		s.fireEventHook(ev)
	}
	if created {
		s.notifyTodoReady(ctx, t.Queue)
		s.fireTodoHook("created", t)
		// Sender gate (SPEC-0011): only a todo whose delivery event passed per-source
		// verification is eligible for a channel push. Plain CreateTodo (no event, e.g. the dev
		// helper) never rings the doorbell — those todos degrade to pull, losing nothing.
		if e.Verified {
			s.fireDoorbell(t)
		}
	}
	return ev.ID, t, created, nil
}

// createTodo is the querier-based core of CreateTodo: it runs on either the pool or a transaction and
// performs no LISTEN/NOTIFY (the caller nudges only after a durable commit).
func createTodo(ctx context.Context, q querier, p CreateTodoParams) (Todo, bool, error) {
	id := "td_" + uuid.NewString()
	row := q.QueryRow(ctx, `
		INSERT INTO todos (id, queue, source, kind, title, payload, event_id, idempotency_key, assignee)
		VALUES ($1, $2, NULLIF($3,''), NULLIF($4,''), $5, $6, $7, NULLIF($8,''), NULLIF($9,''))
		ON CONFLICT (queue, idempotency_key)
			WHERE idempotency_key IS NOT NULL AND state <> 'done' AND state <> 'failed'
			DO NOTHING
		RETURNING `+todoCols,
		id, p.Queue, p.Source, p.Kind, p.Title, p.Payload, p.EventID, p.IdempotencyKey, p.Assignee)
	t, err := scanTodo(row)
	if err == nil {
		return t, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Todo{}, false, err
	}
	// Conflict: return the existing non-terminal todo.
	row = q.QueryRow(ctx, `SELECT `+todoCols+` FROM todos
		WHERE queue = $1 AND idempotency_key = $2 AND state <> 'done' AND state <> 'failed'
		ORDER BY created_at LIMIT 1`, p.Queue, p.IdempotencyKey)
	t, err = scanTodo(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Todo{}, false, ErrNotFound
	}
	return t, false, err
}

// notifyTodoReady emits a best-effort LISTEN/NOTIFY wakeup on the todo_ready channel carrying the
// queue name. Correctness never depends on delivery (SPEC-0004): errors are swallowed by design.
func (s *Store) notifyTodoReady(ctx context.Context, queue string) {
	// pg_notify is used (not a literal NOTIFY) so the channel payload — the queue name — is passed as
	// a bound parameter rather than interpolated into SQL text (Parameterized Queries Only).
	_, _ = s.pool.Exec(ctx, `SELECT pg_notify('todo_ready', $1)`, queue)
}

// ClaimTodo atomically claims a specific todo for owner, setting a lease. A todo is claimable when it
// is pending OR when its lease has expired (crash recovery, so a stopped reaper can't strand it) and
// it still has attempts remaining. ADR-0007 claim / SPEC-0003 lease recovery. Returns ErrConflict if
// the todo exists but is not claimable (live claim / wrong assignee / exhausted), ErrNotFound if absent.
func (s *Store) ClaimTodo(ctx context.Context, id, owner string, ttl time.Duration) (Todo, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE todos SET state='claimed', owner=$2, lease_expires_at=now()+$3::interval,
			attempt=attempt+1, claimed_at=now(), updated_at=now()
		WHERE id=$1 AND (assignee IS NULL OR assignee=$2)
			AND (state='pending'
				OR (state='claimed' AND lease_expires_at < now() AND attempt < max_attempts))
		RETURNING `+todoCols, id, owner, ttl.String())
	t, err := scanTodo(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Todo{}, s.classifyMiss(ctx, id)
	}
	if err == nil {
		s.fireTodoHook("claimed", t)
	}
	return t, err
}

// ClaimNext claims the oldest claimable todo across the allowed queues using FOR UPDATE SKIP LOCKED
// (ADR-0002), so concurrent workers never collide. A todo is claimable when it is pending OR when its
// lease has expired with attempts remaining — the scan recovers expired leases directly, so a stopped
// reaper can never strand work (SPEC-0003). Returns ErrNotFound when no work is available.
func (s *Store) ClaimNext(ctx context.Context, queues []string, owner string, ttl time.Duration) (Todo, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE todos SET state='claimed', owner=$1, lease_expires_at=now()+$2::interval,
			attempt=attempt+1, claimed_at=now(), updated_at=now()
		WHERE id = (
			SELECT id FROM todos
			WHERE queue = ANY($3) AND (assignee IS NULL OR assignee=$1)
				AND (state='pending'
					OR (state='claimed' AND lease_expires_at < now() AND attempt < max_attempts))
			ORDER BY created_at
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		RETURNING `+todoCols, owner, ttl.String(), queues)
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
// an attempt or change claimed_at. Returns ErrConflict if the todo exists but is not a live claim
// owned by owner, ErrNotFound if absent. Governing: SPEC-0003 REQ "Visibility Window, Lease, Heartbeat".
func (s *Store) HeartbeatTodo(ctx context.Context, id, owner string, ttl time.Duration) (Todo, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE todos SET lease_expires_at=now()+$3::interval, updated_at=now()
		WHERE id=$1 AND state='claimed' AND owner=$2
		RETURNING `+todoCols, id, owner, ttl.String())
	t, err := scanTodo(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Todo{}, s.classifyMiss(ctx, id)
	}
	return t, err
}

// CompleteTodo acks a claimed todo owned by owner. ADR-0007 complete.
func (s *Store) CompleteTodo(ctx context.Context, id, owner string, result []byte) (Todo, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE todos SET state='done', result=$3, completed_at=now(), updated_at=now()
		WHERE id=$1 AND state='claimed' AND owner=$2
		RETURNING `+todoCols, id, owner, result)
	t, err := scanTodo(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Todo{}, s.classifyMiss(ctx, id)
	}
	if err == nil {
		s.fireTodoHook("done", t)
	}
	return t, err
}

// FailTodo fails a claimed todo: retry (→pending) while attempt < max_attempts, else dead-letter
// (→failed). ADR-0007 fail.
func (s *Store) FailTodo(ctx context.Context, id, owner string, result []byte) (Todo, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE todos SET
			state = CASE WHEN attempt >= max_attempts THEN 'failed' ELSE 'pending' END,
			owner = CASE WHEN attempt >= max_attempts THEN owner ELSE NULL END,
			lease_expires_at = NULL, result = $3, updated_at = now()
		WHERE id=$1 AND state='claimed' AND owner=$2
		RETURNING `+todoCols, id, owner, result)
	t, err := scanTodo(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Todo{}, s.classifyMiss(ctx, id)
	}
	if err == nil {
		// verb mirrors the committed outcome: pending (will retry) or failed (dead-lettered).
		s.fireTodoHook(t.State, t)
	}
	return t, err
}

// RetryTodo re-enqueues a dead-lettered (failed) todo: the only sanctioned way a terminal todo
// re-enters pending (SPEC-0003 lifecycle: failed → pending, operator/agent retry). It resets the
// attempt budget and clears owner/lease/result so the todo gets a fresh set of tries. Returns
// ErrConflict if the todo exists but is not failed (terminal states are otherwise final), ErrNotFound
// if absent.
func (s *Store) RetryTodo(ctx context.Context, id string) (Todo, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE todos SET state='pending', owner=NULL, lease_expires_at=NULL, attempt=0,
			result=NULL, claimed_at=NULL, completed_at=NULL, updated_at=now()
		WHERE id=$1 AND state='failed'
		RETURNING `+todoCols, id)
	t, err := scanTodo(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Todo{}, s.classifyMiss(ctx, id)
	}
	if err == nil {
		s.fireTodoHook("pending", t)
	}
	return t, err
}

// ReleaseTodo returns a claimed todo to pending immediately — the operator "Release" action, which
// hands work back to the queue without waiting for the lease to expire. Only the current lease owner
// may release; it clears owner and lease but does NOT consume or reset attempts (unlike fail/retry).
// Returns ErrConflict if the todo exists but is not a live claim owned by owner, ErrNotFound if
// absent. Governing: SPEC-0013 REQ "Todo Detail Drawer" (Release), SPEC-0003 lease semantics.
func (s *Store) ReleaseTodo(ctx context.Context, id, owner string) (Todo, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE todos SET state='pending', owner=NULL, lease_expires_at=NULL, updated_at=now()
		WHERE id=$1 AND state='claimed' AND owner=$2
		RETURNING `+todoCols, id, owner)
	t, err := scanTodo(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Todo{}, s.classifyMiss(ctx, id)
	}
	if err == nil {
		s.fireTodoHook("pending", t)
	}
	return t, err
}

// ListTodos returns todos in the allowed queues, optionally filtered by state, newest first.
func (s *Store) ListTodos(ctx context.Context, queues []string, state string, limit int) ([]Todo, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `SELECT `+todoCols+` FROM todos
		WHERE queue = ANY($1) AND ($2 = '' OR state = $2)
		ORDER BY created_at DESC LIMIT $3`, queues, state, limit)
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

// GetTodo returns one todo by id.
func (s *Store) GetTodo(ctx context.Context, id string) (Todo, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+todoCols+` FROM todos WHERE id = $1`, id)
	t, err := scanTodo(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Todo{}, ErrNotFound
	}
	return t, err
}

// ReapExpired requeues (or dead-letters) todos whose lease has expired — crash safety (ADR-0002 reaper).
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
func (s *Store) classifyMiss(ctx context.Context, id string) error {
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM todos WHERE id=$1)`, id).Scan(&exists); err != nil {
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
			content_type, headers, payload, payload_size, source_ip)
		VALUES ($1,$2,NULLIF($3,''),NULLIF($4,''),$5,$6,NULLIF($7,''),NULLIF($8,''),$9,$10,$11,NULLIF($12,'')::inet)
		ON CONFLICT (source, external_id) WHERE external_id IS NOT NULL DO NOTHING
		RETURNING id, received_at`,
		e.Source, e.Family, e.EventType, e.ExternalID, e.TrustMode, e.Verified, e.VerifyDetail,
		e.ContentType, e.Headers, e.Payload, len(e.Payload), e.SourceIP).Scan(&ev.ID, &ev.ReceivedAt)
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
