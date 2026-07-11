package store

// Read models for the operator Todos view (SPEC-0013 REQ "Todos View — Durable Queue", REQ "Todo
// Detail Drawer"): the filter-pill counts, the filtered/searched table listing, and the enriched
// single-todo detail. These are presentation reads over the durable queue — they implement no
// lifecycle rules of their own (SPEC-0003 owns transitions); they only join each todo to its
// originating event's trust mode and count the deliveries its idempotency key collapsed.
//
// Dedup coupling: the ingest layer sets a delivery's event external_id AND the todo idempotency_key
// to the same value (the provider delivery id, or a body-hash fallback). So the number of distinct
// deliveries collapsed onto one todo is the number of events sharing that idempotency key — the
// "dedup ×N" badge. Governing: SPEC-0013 REQ "Todos View — Durable Queue".

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// TodoCounts are the per-state todo counts backing the Todos view filter pills (All / Pending /
// Claimed / Done / Failed), each rendered live from the database.
type TodoCounts struct {
	All     int
	Pending int
	Claimed int
	Done    int
	Failed  int
}

// TodoCounts returns the filter-pill counts in one round trip.
func (s *Store) TodoCounts(ctx context.Context) (TodoCounts, error) {
	var c TodoCounts
	err := s.pool.QueryRow(ctx, `
		SELECT count(*),
			count(*) FILTER (WHERE state = 'pending'),
			count(*) FILTER (WHERE state = 'claimed'),
			count(*) FILTER (WHERE state = 'done'),
			count(*) FILTER (WHERE state = 'failed')
		FROM todos`,
	).Scan(&c.All, &c.Pending, &c.Claimed, &c.Done, &c.Failed)
	if err != nil {
		return TodoCounts{}, fmt.Errorf("todo counts: %w", err)
	}
	return c, nil
}

// TodoItem is one enriched row of the Todos view table and the detail drawer: the durable todo plus
// its originating event's trust mode ("queue" for event-less todos) and the count of deliveries its
// idempotency key collapsed (≥1; the badge shows only when >1).
type TodoItem struct {
	Todo
	TrustMode  string
	DedupCount int
}

// todoColsT is todoCols aliased to the `t` table for the joined listing queries.
const todoColsT = `t.id, t.queue, COALESCE(t.source,''), COALESCE(t.kind,''), t.title, t.payload, t.event_id,
	COALESCE(t.idempotency_key,''), COALESCE(t.assignee,''), t.state, COALESCE(t.owner,''),
	t.lease_expires_at, t.attempt, t.max_attempts, t.result, t.created_at, t.claimed_at, t.completed_at`

// scanTodoItem scans todoColsT plus the joined trust mode and dedup count.
func scanTodoItem(row pgx.Row) (TodoItem, error) {
	var it TodoItem
	err := row.Scan(&it.ID, &it.Queue, &it.Source, &it.Kind, &it.Title, &it.Payload, &it.EventID,
		&it.IdempotencyKey, &it.Assignee, &it.State, &it.Owner, &it.LeaseExpiresAt, &it.Attempt,
		&it.MaxAttempts, &it.Result, &it.CreatedAt, &it.ClaimedAt, &it.CompletedAt,
		&it.TrustMode, &it.DedupCount)
	return it, err
}

// ListTodoItems lists todos for the operator Todos view, newest first, optionally scoped to one
// state (filter pill) and/or a case-insensitive substring over id, source, and kind (search). An
// empty filter lists all states; an empty query applies no text filter. The limit is clamped.
// Governing: SPEC-0013 REQ "Todos View — Durable Queue".
func (s *Store) ListTodoItems(ctx context.Context, filter, query string, limit int) ([]TodoItem, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+todoColsT+`,
			COALESCE(e.trust_mode, 'queue') AS trust_mode,
			(SELECT count(*) FROM events ev
			   WHERE t.idempotency_key IS NOT NULL AND ev.external_id = t.idempotency_key)::int AS dedup_count
		FROM todos t
		LEFT JOIN events e ON e.id = t.event_id
		WHERE ($1 = '' OR t.state = $1)
			AND ($2 = ''
				OR t.id ILIKE '%' || $2 || '%'
				OR COALESCE(t.source, '') ILIKE '%' || $2 || '%'
				OR COALESCE(t.kind, '') ILIKE '%' || $2 || '%')
		ORDER BY t.created_at DESC
		LIMIT $3`, filter, query, limit)
	if err != nil {
		return nil, fmt.Errorf("list todo items: %w", err)
	}
	defer rows.Close()
	var out []TodoItem
	for rows.Next() {
		it, err := scanTodoItem(rows)
		if err != nil {
			return nil, fmt.Errorf("list todo items scan: %w", err)
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list todo items rows: %w", err)
	}
	return out, nil
}

// GetTodoItem returns one enriched todo (trust mode + dedup count) for the detail drawer, or
// ErrNotFound. Governing: SPEC-0013 REQ "Todo Detail Drawer".
func (s *Store) GetTodoItem(ctx context.Context, id string) (TodoItem, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT `+todoColsT+`,
			COALESCE(e.trust_mode, 'queue') AS trust_mode,
			(SELECT count(*) FROM events ev
			   WHERE t.idempotency_key IS NOT NULL AND ev.external_id = t.idempotency_key)::int AS dedup_count
		FROM todos t
		LEFT JOIN events e ON e.id = t.event_id
		WHERE t.id = $1`, id)
	it, err := scanTodoItem(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return TodoItem{}, ErrNotFound
	}
	if err != nil {
		return TodoItem{}, fmt.Errorf("get todo item: %w", err)
	}
	return it, nil
}
