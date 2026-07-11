package store

// Event-history projections for the SPEC-0005 MCP contract: the compact summary rows behind
// list_webhook_events (and the recent-events resource) and the full sanitized record behind
// get_webhook_event. Both are projections of exactly the `events` table columns
// (internal/db/migrations/0001_init.sql) so the MCP, HTTP, and UI surfaces never drift.
//
// Governing: ADR-0005 (compact list, full get), SPEC-0005 REQ "Event Shape Parity and Trust
// Disclosure".

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// EventHistoryItem is the SPEC-0005 EventSummary projection: no payload, no headers, so broad
// scans stay small. Trust metadata (TrustMode, Verified) is always carried.
type EventHistoryItem struct {
	ID          int64
	Provider    string // the events.source column; "provider" over MCP
	EventType   string
	TrustMode   string // signed|token|open|queue
	Verified    bool
	PayloadSize int
	ReceivedAt  time.Time
}

// EventHistoryDetail is the SPEC-0005 EventDetail projection: every summary field plus the full
// sanitized record. Headers are the ingest-sanitized JSON object (signature/secret values already
// redacted); Payload is the stored raw body kept for replay.
type EventHistoryDetail struct {
	EventHistoryItem
	VerifyDetail string
	ExternalID   string
	ContentType  string
	SourceIP     string
	Headers      []byte // sanitized JSON object (redacted at ingest, ADR-0002/0003)
	Payload      []byte
}

// EventHistoryFilter bounds a ListEventHistory scan. Zero values mean "no filter"; Limit is
// clamped to [1, 200] with a default of 50 (SPEC-0005). Keyset (since/cursor) bounds are layered
// on by the pagination work (#39); the stable ORDER BY received_at DESC, id DESC is already the
// cursor-ready sort.
type EventHistoryFilter struct {
	Provider  string
	EventType string
	Limit     int
}

// ListEventHistory returns event summaries newest first under the stable
// `received_at DESC, id DESC` order SPEC-0005 REQ "Deterministic Pagination and Filtering" pins.
func (s *Store) ListEventHistory(ctx context.Context, f EventHistoryFilter) ([]EventHistoryItem, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, source, COALESCE(event_type, ''), trust_mode, verified, payload_size, received_at
		FROM events
		WHERE ($1 = '' OR source = $1) AND ($2 = '' OR event_type = $2)
		ORDER BY received_at DESC, id DESC
		LIMIT $3`, f.Provider, f.EventType, limit)
	if err != nil {
		return nil, fmt.Errorf("list event history: %w", err)
	}
	defer rows.Close()
	var out []EventHistoryItem
	for rows.Next() {
		var e EventHistoryItem
		if err := rows.Scan(&e.ID, &e.Provider, &e.EventType, &e.TrustMode, &e.Verified,
			&e.PayloadSize, &e.ReceivedAt); err != nil {
			return nil, fmt.Errorf("list event history scan: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list event history rows: %w", err)
	}
	return out, nil
}

// EventHistoryByID returns the full sanitized record for one event, or ErrNotFound.
func (s *Store) EventHistoryByID(ctx context.Context, id int64) (EventHistoryDetail, error) {
	var e EventHistoryDetail
	err := s.pool.QueryRow(ctx, `
		SELECT id, source, COALESCE(event_type, ''), trust_mode, verified, payload_size, received_at,
			COALESCE(verify_detail, ''), COALESCE(external_id, ''), COALESCE(content_type, ''),
			COALESCE(host(source_ip), ''), COALESCE(headers, '{}'::jsonb), COALESCE(payload, ''::bytea)
		FROM events WHERE id = $1`, id,
	).Scan(&e.ID, &e.Provider, &e.EventType, &e.TrustMode, &e.Verified, &e.PayloadSize, &e.ReceivedAt,
		&e.VerifyDetail, &e.ExternalID, &e.ContentType, &e.SourceIP, &e.Headers, &e.Payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return EventHistoryDetail{}, ErrNotFound
	}
	if err != nil {
		return EventHistoryDetail{}, fmt.Errorf("event history by id: %w", err)
	}
	return e, nil
}
