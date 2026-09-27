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

// ownedByHuman is the tenant predicate every operator read below carries, kept as one constant so
// the six of them cannot drift apart. It walks the ownership chain a todo actually has —
// todos.endpoint_id → endpoints.agent_id → agents.owner_human_id — which is the same chain
// ListEndpointCards has always used ("strictly by owner_human_id so one operator can never see
// another's endpoints", agents.go).
//
// These reads used to carry no tenant predicate at all. Endpoints was scoped; the Board never was,
// so every signed-in human saw and could mutate every other human's todos, counts and raw payloads
// — for seven weeks, across six real accounts, until an invited user reported seeing someone else's
// webhooks. The MCP surface was never affected: it is endpoint-scoped by construction (ADR-0022),
// and it is only the operator web reads that bypassed the model.
//
// A todo with a NULL endpoint_id belongs to nobody and is therefore visible to nobody. That is
// deliberate: an unowned row is an ingestion bug, and defaulting it to "everyone" is exactly the
// failure being fixed here. The JOIN (not LEFT JOIN) enforces it.
//
// Governing: SPEC-0007 REQ "Human as Accountable Principal"; SPEC-0013 (operator board).
const ownedByHuman = `
		JOIN endpoints ep ON ep.id = t.endpoint_id
		JOIN agents    ag ON ag.id = ep.agent_id AND ag.owner_human_id = `

// TodoCounts are the per-state todo counts backing the Todos view filter pills (All / Pending /
// Claimed / Done / Failed), each rendered live from the database.
//
// The four A2A states (canceled/rejected/input-required/auth-required, SPEC-0018) are deliberately
// NOT broken out here: the filter-pill set is the fixed Board surface defined by SPEC-0013, which
// story #58 (state-machine + migration) does not extend — dedicated A2A-state UI is the province of
// the A2A feature stories. They are still included in `All` (count(*) has no state filter), so no
// A2A-state todo is dropped from the total; it simply has no dedicated pill yet.
// Governing: SPEC-0018 REQ "Task State Machine Extension" (audit of every state-consuming site).
type TodoCounts struct {
	All     int
	Pending int
	Claimed int
	Done    int
	Failed  int
}

// TodoCounts returns the filter-pill counts for ONE human's todos in one round trip. The counts
// are as tenant-scoped as the table they summarise: a count is a disclosure too, and a pill
// reading "412 pending" on a board showing four rows tells the viewer plenty about everyone else.
func (s *Store) TodoCounts(ctx context.Context, ownerHumanID string) (TodoCounts, error) {
	var c TodoCounts
	err := s.pool.QueryRow(ctx, `
		SELECT count(*),
			count(*) FILTER (WHERE t.state = 'pending'),
			count(*) FILTER (WHERE t.state = 'claimed'),
			count(*) FILTER (WHERE t.state = 'done'),
			count(*) FILTER (WHERE t.state = 'failed')
		FROM todos t`+ownedByHuman+`$1`, ownerHumanID,
	).Scan(&c.All, &c.Pending, &c.Claimed, &c.Done, &c.Failed)
	if err != nil {
		return TodoCounts{}, fmt.Errorf("todo counts: %w", err)
	}
	return c, nil
}

// TodoItem is one enriched row of the Todos view table and the detail drawer: the durable todo plus
// its originating event's trust mode ("queue" for event-less todos) and the count of deliveries its
// idempotency key collapsed (≥1; the badge shows only when >1). The count is bounded to events with
// the same owner as the todo's own event: since 0024 another owner can hold an event with the same
// external id, and counting it would leak that owner's delivery into this badge (F14). A todo with
// no event, or whose event has no owner, counts 0. Governing: ADR-0038, SPEC-0033 REQ "Closing the
// Audited Surfaces" (F14).
type TodoItem struct {
	Todo
	TrustMode  string
	DedupCount int
}

// todoColsT is todoCols aliased to the `t` table for the joined listing queries.
const todoColsT = `t.id, t.endpoint_id::text, t.queue, COALESCE(t.source,''), COALESCE(t.kind,''), t.title, t.payload, t.event_id,
	COALESCE(t.idempotency_key,''), COALESCE(t.assignee,''), t.state, COALESCE(t.owner,''),
	t.lease_expires_at, t.attempt, t.max_attempts, t.result, t.created_at, t.claimed_at, t.completed_at,
	t.next_retry_at`

// scanTodoItem scans todoColsT plus the joined trust mode and dedup count.
func scanTodoItem(row pgx.Row) (TodoItem, error) {
	var it TodoItem
	var endpointID *string
	err := row.Scan(&it.ID, &endpointID, &it.Queue, &it.Source, &it.Kind, &it.Title, &it.Payload, &it.EventID,
		&it.IdempotencyKey, &it.Assignee, &it.State, &it.Owner, &it.LeaseExpiresAt, &it.Attempt,
		&it.MaxAttempts, &it.Result, &it.CreatedAt, &it.ClaimedAt, &it.CompletedAt, &it.NextRetryAt,
		&it.TrustMode, &it.DedupCount)
	if endpointID != nil {
		it.EndpointID = *endpointID
	}
	return it, err
}

// ListTodoItems lists todos for the operator Todos view, newest first, optionally scoped to one
// state (filter pill) and/or a case-insensitive substring over id, source, and kind (search). An
// empty filter lists all states; an empty query applies no text filter. The limit is clamped.
// Governing: SPEC-0013 REQ "Todos View — Durable Queue".
func (s *Store) ListTodoItems(ctx context.Context, ownerHumanID, filter, query string, limit int) ([]TodoItem, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+todoColsT+`,
			COALESCE(e.trust_mode, 'queue') AS trust_mode,
			(SELECT count(*) FROM events ev
			   WHERE t.idempotency_key IS NOT NULL AND ev.external_id = t.idempotency_key
			     AND ev.endpoint_id = e.endpoint_id)::int AS dedup_count
		FROM todos t`+ownedByHuman+`$1
		LEFT JOIN events e ON e.id = t.event_id
		WHERE ($2 = '' OR t.state = $2)
			AND ($3 = ''
				OR t.id ILIKE '%' || $3 || '%'
				OR COALESCE(t.source, '') ILIKE '%' || $3 || '%'
				OR COALESCE(t.kind, '') ILIKE '%' || $3 || '%')
		ORDER BY t.created_at DESC
		LIMIT $4`, ownerHumanID, filter, query, limit)
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

// GetTodoItem returns one enriched todo (trust mode + dedup count) for the detail drawer, scoped to
// the human who owns it, or ErrNotFound.
//
// Another human's todo is ErrNotFound, NOT a permission error, and the difference matters: a 403
// confirms the id exists, which turns the drawer into an oracle for enumerating other tenants' work
// even once the body is withheld. Indistinguishable-from-absent is the only answer that leaks
// nothing. Governing: SPEC-0013 REQ "Todo Detail Drawer"; SPEC-0007 REQ "Human as Accountable
// Principal".
func (s *Store) GetTodoItem(ctx context.Context, ownerHumanID, id string) (TodoItem, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT `+todoColsT+`,
			COALESCE(e.trust_mode, 'queue') AS trust_mode,
			(SELECT count(*) FROM events ev
			   WHERE t.idempotency_key IS NOT NULL AND ev.external_id = t.idempotency_key
			     AND ev.endpoint_id = e.endpoint_id)::int AS dedup_count
		FROM todos t`+ownedByHuman+`$1
		LEFT JOIN events e ON e.id = t.event_id
		WHERE t.id = $2`, ownerHumanID, id)
	it, err := scanTodoItem(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return TodoItem{}, ErrNotFound
	}
	if err != nil {
		return TodoItem{}, fmt.Errorf("get todo item: %w", err)
	}
	return it, nil
}
