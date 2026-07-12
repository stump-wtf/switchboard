package store

// Board/shell read models for the operator board (SPEC-0013): server-computed counts, the
// recent-events slice rendered by GET /, and the per-minute activity buckets. The web layer's
// typed SSE taxonomy (internal/web/live.go) re-renders these on every committed transition.
// Governing: SPEC-0013 REQ "Information Architecture and Navigation", REQ "Board View — Live
// Incoming Lines".

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// BoardStats are the server-computed summary numbers for the Board tiles and the layout shell
// (rail todo count, top-bar LIVE rate).
type BoardStats struct {
	TotalTodos    int // every todo in the durable queue, any state — the rail Todos badge (design record)
	TodosToday    int // todos created since local midnight
	InFlight      int // todos currently claimed under lease
	AwaitingClaim int // todos pending, waiting for a claim
	VerifiedPct   int // % of today's events from signed sources (0 when no events)
	EventsPerMin  int // events received in the last minute — the LIVE pill rate
}

// BoardStats returns the Board tile counts in one round trip.
func (s *Store) BoardStats(ctx context.Context) (BoardStats, error) {
	var b BoardStats
	err := s.pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM todos),
			(SELECT count(*) FROM todos WHERE created_at >= date_trunc('day', now())),
			(SELECT count(*) FROM todos WHERE state = 'claimed'),
			(SELECT count(*) FROM todos WHERE state = 'pending'),
			(SELECT COALESCE(round(100.0 * count(*) FILTER (WHERE trust_mode = 'signed') / NULLIF(count(*), 0)), 0)::int
			   FROM events WHERE received_at >= date_trunc('day', now())),
			(SELECT count(*) FROM events WHERE received_at > now() - interval '1 minute')`,
	).Scan(&b.TotalTodos, &b.TodosToday, &b.InFlight, &b.AwaitingClaim, &b.VerifiedPct, &b.EventsPerMin)
	if err != nil {
		return BoardStats{}, fmt.Errorf("board stats: %w", err)
	}
	return b, nil
}

// EventSummary is one row of the Board's recent-events feed: the accepted delivery plus the
// lifecycle state of the todo it patched through to (empty Todo* fields when no todo is linked —
// the row renders as still "verifying…").
type EventSummary struct {
	ID         int64
	Source     string
	EventType  string
	TrustMode  string
	ReceivedAt time.Time
	TodoID     string // linked todo ("" when none)
	TodoState  string // pending|claimed|done|failed ("" when none)
	TodoOwner  string // lease owner when claimed ("" otherwise)
	Deduped    bool   // no todo links THIS event, but its delivery key collapsed onto another's todo
}

// RecentEvents returns the newest accepted events, newest first, each joined to its todo's
// lifecycle state so the feed can render the stage (verifying → patched → claimed → done). An event
// whose delivery collapsed onto an EARLIER delivery's todo (same idempotency key, so createTodo
// deduped and left this event with no todo of its own) is flagged Deduped, so the feed can render a
// truthful `deduped` stage instead of a perpetual `verifying…`.
// Governing: SPEC-0013 REQ "Board View — Live Incoming Lines".
func (s *Store) RecentEvents(ctx context.Context, limit int) ([]EventSummary, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT e.id, e.source, COALESCE(e.event_type, ''), e.trust_mode, e.received_at,
			COALESCE(t.id, ''), COALESCE(t.state, ''), COALESCE(t.owner, ''),
			(t.id IS NULL AND e.external_id IS NOT NULL
				AND EXISTS (SELECT 1 FROM todos d WHERE d.idempotency_key = e.external_id)) AS deduped
		FROM events e
		LEFT JOIN LATERAL (
			SELECT id, state, owner FROM todos WHERE event_id = e.id ORDER BY created_at DESC LIMIT 1
		) t ON true
		ORDER BY e.received_at DESC, e.id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("recent events: %w", err)
	}
	defer rows.Close()
	var out []EventSummary
	for rows.Next() {
		var e EventSummary
		if err := rows.Scan(&e.ID, &e.Source, &e.EventType, &e.TrustMode, &e.ReceivedAt,
			&e.TodoID, &e.TodoState, &e.TodoOwner, &e.Deduped); err != nil {
			return nil, fmt.Errorf("recent events scan: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("recent events rows: %w", err)
	}
	return out, nil
}

// EventByID returns the feed summary for one event (no todo join — callers updating a live row
// already hold the todo), or ErrNotFound.
func (s *Store) EventByID(ctx context.Context, id int64) (EventSummary, error) {
	var e EventSummary
	err := s.pool.QueryRow(ctx, `
		SELECT id, source, COALESCE(event_type, ''), trust_mode, received_at
		FROM events WHERE id = $1`, id,
	).Scan(&e.ID, &e.Source, &e.EventType, &e.TrustMode, &e.ReceivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return EventSummary{}, ErrNotFound
	}
	if err != nil {
		return EventSummary{}, fmt.Errorf("event by id: %w", err)
	}
	return e, nil
}

// EventBuckets returns per-minute inbound-event counts for the last n minutes, oldest bucket
// first (the final bucket is the current minute) — the throughput tile's activity bars.
// Governing: SPEC-0013 REQ "Board View — Live Incoming Lines" (recent-activity bar chart).
func (s *Store) EventBuckets(ctx context.Context, n int) ([]int, error) {
	if n <= 0 || n > 288 {
		n = 24
	}
	rows, err := s.pool.Query(ctx, `
		SELECT count(e.id)::int
		FROM generate_series($1 - 1, 0, -1) AS g(m)
		LEFT JOIN events e
			ON e.received_at >  now() - (g.m + 1) * interval '1 minute'
			AND e.received_at <= now() - g.m * interval '1 minute'
		GROUP BY g.m ORDER BY g.m DESC`, n)
	if err != nil {
		return nil, fmt.Errorf("event buckets: %w", err)
	}
	defer rows.Close()
	out := make([]int, 0, n)
	for rows.Next() {
		var c int
		if err := rows.Scan(&c); err != nil {
			return nil, fmt.Errorf("event buckets scan: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("event buckets rows: %w", err)
	}
	return out, nil
}

// Ping reports database-pool health for the layout shell's connectivity indicator.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.pool.Ping(ctx); err != nil {
		return fmt.Errorf("db ping: %w", err)
	}
	return nil
}
