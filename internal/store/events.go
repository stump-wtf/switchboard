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
	"strconv"
	"strings"
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
// clamped to [1, 200] with a default of 50 (SPEC-0005). The scan is keyset-paginated on the stable
// `received_at DESC, id DESC` order: SinceTime/SinceID bound the lower (older) edge inclusively,
// while CursorTime/CursorID are the exclusive upper bound carried across pages so that concurrent
// inserts on an always-on receiver never produce a duplicate or a gap.
// Governing: ADR-0005 (cursor over offset), SPEC-0005 REQ "Deterministic Pagination and Filtering".
type EventHistoryFilter struct {
	Provider  string
	EventType string
	Limit     int
	// SinceTime / SinceID bound the lower edge inclusively. `since` is supplied over MCP as either an
	// ISO-8601 timestamp (→ SinceTime, received_at >= t) or an event id (→ SinceID, id >= i); the tool
	// layer decides which. Both may be zero (no lower bound).
	SinceTime time.Time
	SinceID   int64
	// CursorTime / CursorID are the exclusive keyset upper bound decoded from an opaque cursor: only
	// rows strictly older than this (received_at, id) tuple are returned. Zero (CursorTime.IsZero())
	// means the first page.
	CursorTime time.Time
	CursorID   int64
}

// ListEventHistory returns event summaries newest first under the stable
// `received_at DESC, id DESC` order SPEC-0005 REQ "Deterministic Pagination and Filtering" pins.
// Filters and the keyset cursor are composed as AND-ed conditions with ordinal placeholders so a
// zero-valued filter field contributes no predicate at all.
func (s *Store) ListEventHistory(ctx context.Context, f EventHistoryFilter) ([]EventHistoryItem, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	var (
		conds []string
		args  []any
	)
	ph := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	if f.Provider != "" {
		conds = append(conds, "source = "+ph(f.Provider))
	}
	if f.EventType != "" {
		conds = append(conds, "event_type = "+ph(f.EventType))
	}
	if !f.SinceTime.IsZero() {
		conds = append(conds, "received_at >= "+ph(f.SinceTime))
	}
	if f.SinceID > 0 {
		conds = append(conds, "id >= "+ph(f.SinceID))
	}
	if !f.CursorTime.IsZero() {
		// Keyset upper bound: strictly older than the last (received_at, id) the caller saw. Row-value
		// comparison matches the ORDER BY exactly, so pagination is exhaustive and gap-free even as new
		// rows land at the head between requests.
		conds = append(conds, "(received_at, id) < ("+ph(f.CursorTime)+", "+ph(f.CursorID)+")")
	}
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}
	query := fmt.Sprintf(`
		SELECT id, source, COALESCE(event_type, ''), trust_mode, verified, payload_size, received_at
		FROM events
		%s
		ORDER BY received_at DESC, id DESC
		LIMIT %s`, where, ph(limit))

	rows, err := s.pool.Query(ctx, query, args...)
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
