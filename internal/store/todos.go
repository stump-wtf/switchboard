package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrConflict is returned when a state transition loses a race (e.g. claim an already-claimed todo).
var ErrConflict = errors.New("store: conflict")

// Todo is a durable work-item (ADR-0007). States: pending → claimed → done|failed.
type Todo struct {
	ID             string
	Queue          string
	Source         string
	Kind           string
	Title          string
	Payload        []byte // raw JSON (jsonb)
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

const todoCols = `id, queue, COALESCE(source,''), COALESCE(kind,''), title, payload,
	COALESCE(idempotency_key,''), COALESCE(assignee,''), state, COALESCE(owner,''),
	lease_expires_at, attempt, max_attempts, result, created_at, claimed_at, completed_at`

func scanTodo(row pgx.Row) (Todo, error) {
	var t Todo
	err := row.Scan(&t.ID, &t.Queue, &t.Source, &t.Kind, &t.Title, &t.Payload, &t.IdempotencyKey,
		&t.Assignee, &t.State, &t.Owner, &t.LeaseExpiresAt, &t.Attempt, &t.MaxAttempts, &t.Result,
		&t.CreatedAt, &t.ClaimedAt, &t.CompletedAt)
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
	id := "td_" + uuid.NewString()
	row := s.pool.QueryRow(ctx, `
		INSERT INTO todos (id, queue, source, kind, title, payload, event_id, idempotency_key, assignee)
		VALUES ($1, $2, NULLIF($3,''), NULLIF($4,''), $5, $6, $7, NULLIF($8,''), NULLIF($9,''))
		ON CONFLICT (queue, idempotency_key)
			WHERE idempotency_key IS NOT NULL AND state <> 'done' AND state <> 'failed'
			DO NOTHING
		RETURNING `+todoCols,
		id, p.Queue, p.Source, p.Kind, p.Title, p.Payload, p.EventID, p.IdempotencyKey, p.Assignee)
	t, err := scanTodo(row)
	if err == nil {
		// Governing: SPEC-0004 REQ "In-Database Wakeups via LISTEN/NOTIFY". Best-effort nudge so an
		// idle worker/UI wakes without polling. The durable queue is the source of truth, so a lost
		// NOTIFY costs only latency, never work — hence the error here is deliberately ignored.
		s.notifyTodoReady(ctx, t.Queue)
		return t, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Todo{}, false, err
	}
	// Conflict: return the existing non-terminal todo.
	row = s.pool.QueryRow(ctx, `SELECT `+todoCols+` FROM todos
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

// ClaimTodo atomically claims a specific pending todo for owner, setting a lease. ADR-0007 claim.
// Returns ErrConflict if the todo exists but is not claimable (already claimed / wrong assignee),
// ErrNotFound if it does not exist.
func (s *Store) ClaimTodo(ctx context.Context, id, owner string, ttl time.Duration) (Todo, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE todos SET state='claimed', owner=$2, lease_expires_at=now()+$3::interval,
			attempt=attempt+1, claimed_at=now(), updated_at=now()
		WHERE id=$1 AND state='pending' AND (assignee IS NULL OR assignee=$2)
		RETURNING `+todoCols, id, owner, ttl.String())
	t, err := scanTodo(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Todo{}, s.classifyMiss(ctx, id)
	}
	return t, err
}

// ClaimNext claims the oldest pending todo across the allowed queues using FOR UPDATE SKIP LOCKED
// (ADR-0002), so concurrent workers never collide. Returns ErrNotFound when no work is available.
func (s *Store) ClaimNext(ctx context.Context, queues []string, owner string, ttl time.Duration) (Todo, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE todos SET state='claimed', owner=$1, lease_expires_at=now()+$2::interval,
			attempt=attempt+1, claimed_at=now(), updated_at=now()
		WHERE id = (
			SELECT id FROM todos
			WHERE queue = ANY($3) AND state='pending' AND (assignee IS NULL OR assignee=$1)
			ORDER BY created_at
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		RETURNING `+todoCols, owner, ttl.String(), queues)
	t, err := scanTodo(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Todo{}, ErrNotFound
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
// Returns the number of todos reaped.
func (s *Store) ReapExpired(ctx context.Context) (int64, error) {
	ct, err := s.pool.Exec(ctx, `
		UPDATE todos SET
			state = CASE WHEN attempt >= max_attempts THEN 'failed' ELSE 'pending' END,
			owner = NULL, lease_expires_at = NULL, updated_at = now()
		WHERE state='claimed' AND lease_expires_at < now()`)
	if err != nil {
		return 0, err
	}
	return ct.RowsAffected(), nil
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
// (existing id on a duplicate delivery).
func (s *Store) InsertEvent(ctx context.Context, e EventInput) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO events (source, family, event_type, external_id, trust_mode, verified, verify_detail,
			content_type, headers, payload, payload_size, source_ip)
		VALUES ($1,$2,NULLIF($3,''),NULLIF($4,''),$5,$6,NULLIF($7,''),NULLIF($8,''),$9,$10,$11,NULLIF($12,'')::inet)
		ON CONFLICT (source, external_id) WHERE external_id IS NOT NULL DO NOTHING
		RETURNING id`,
		e.Source, e.Family, e.EventType, e.ExternalID, e.TrustMode, e.Verified, e.VerifyDetail,
		e.ContentType, e.Headers, e.Payload, len(e.Payload), e.SourceIP).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		// Duplicate delivery — fetch the existing id.
		if err2 := s.pool.QueryRow(ctx,
			`SELECT id FROM events WHERE source=$1 AND external_id=$2`, e.Source, e.ExternalID,
		).Scan(&id); err2 != nil {
			return 0, err2
		}
		return id, nil
	}
	return id, err
}
