package store

// Board/shell read models for the operator board (SPEC-0013): server-computed counts and the
// recent-events slice rendered by GET /. Live (SSE) updates layer on top in a later story.
// Governing: SPEC-0013 REQ "Information Architecture and Navigation", REQ "Board View — Live
// Incoming Lines".

import (
	"context"
	"fmt"
	"time"
)

// BoardStats are the server-computed summary numbers for the Board tiles and the layout shell
// (rail todo count, top-bar LIVE rate).
type BoardStats struct {
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
			(SELECT count(*) FROM todos WHERE created_at >= date_trunc('day', now())),
			(SELECT count(*) FROM todos WHERE state = 'claimed'),
			(SELECT count(*) FROM todos WHERE state = 'pending'),
			(SELECT COALESCE(round(100.0 * count(*) FILTER (WHERE trust_mode = 'signed') / NULLIF(count(*), 0)), 0)::int
			   FROM events WHERE received_at >= date_trunc('day', now())),
			(SELECT count(*) FROM events WHERE received_at > now() - interval '1 minute')`,
	).Scan(&b.TodosToday, &b.InFlight, &b.AwaitingClaim, &b.VerifiedPct, &b.EventsPerMin)
	if err != nil {
		return BoardStats{}, fmt.Errorf("board stats: %w", err)
	}
	return b, nil
}

// EventSummary is one row of the Board's recent-events feed.
type EventSummary struct {
	ID         int64
	Source     string
	EventType  string
	TrustMode  string
	ReceivedAt time.Time
}

// RecentEvents returns the newest accepted events, newest first.
func (s *Store) RecentEvents(ctx context.Context, limit int) ([]EventSummary, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, source, COALESCE(event_type, ''), trust_mode, received_at
		FROM events ORDER BY received_at DESC, id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("recent events: %w", err)
	}
	defer rows.Close()
	var out []EventSummary
	for rows.Next() {
		var e EventSummary
		if err := rows.Scan(&e.ID, &e.Source, &e.EventType, &e.TrustMode, &e.ReceivedAt); err != nil {
			return nil, fmt.Errorf("recent events scan: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("recent events rows: %w", err)
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
