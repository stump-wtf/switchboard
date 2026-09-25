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
//
// InFlight counts strictly `claimed` and AwaitingClaim strictly `pending`; the four A2A states
// (SPEC-0018) are deliberately excluded from both. The interrupt states (input-required/
// auth-required) are paused, not actively working, so they are not "in flight"; the terminal A2A
// states (canceled/rejected) are neither in flight nor awaiting a claim. They remain in TotalTodos
// (an unfiltered count) so nothing is dropped from the grand total — matching the intentional-ignore
// documented on TodoCounts. A dedicated A2A-state tile is the province of the A2A feature stories,
// not story #58. Governing: SPEC-0018 REQ "Task State Machine Extension".
// eventOwnedBy builds the tenant predicate for the EVENT-shaped reads below. Every event records
// the endpoint that owns it (events.endpoint_id, 0024); an event without one has no provable owner
// and is invisible to every board, as SPEC-0033 requires. Over owned events, the board shows a
// human the events that produced their work: one delivery fans out to N todos, one per target
// endpoint (ADR-0022), and an event is on a human's board if any todo it produced is theirs.
//
// That is a strictly narrower rule than it looks. A delivery fanned out to two tenants (a friend
// route) is visible to BOTH, which is correct: each of them genuinely received that work, and the
// recipient's todo needs the event's trust metadata. What it never does is expose an event whose
// todos all belong to somebody else, which is what the unscoped reads did.
//
// The second arm covers DEDUPED deliveries, and it is not optional. A redelivery collapses onto an
// earlier todo and is left with no todo of its own (createTodo dedups on the idempotency key), so
// the first arm alone makes it ownerless: it would vanish from its rightful owner's feed, and the
// "deduped" stage the feed renders would never appear again. See eventDedupedOnto for the match.
// Caught by TestTodoItemTrustModeAndDedupCount, which is why that test earns its keep.
//
// Kept as one builder beside ownedByHuman in queue_view.go for the same reason: the predicate is
// the security boundary, and a boundary copy-pasted into six queries is a boundary that drifts.
// It takes the placeholder as an argument because the owner id appears twice in it.
// Governing: SPEC-0007 REQ "Human as Accountable Principal"; SPEC-0013 (operator board); ADR-0038,
// SPEC-0033 REQ "Owner-Scoped History Reads".
func eventOwnedBy(param string) string {
	return `(e.endpoint_id IS NOT NULL AND (
		EXISTS (SELECT 1 FROM todos t
		          JOIN endpoints ep ON ep.id = t.endpoint_id
		          JOIN agents    ag ON ag.id = ep.agent_id
		         WHERE t.event_id = e.id AND ag.owner_human_id = ` + param + `)
		OR ` + eventDedupedOnto(param) + `
	))`
}

// eventDedupedOnto is the predicate "event e collapsed onto a todo the human at param holds": a
// todo whose idempotency key is e's external id AND whose own event has e's owner. The owner match
// is the tenant boundary (F14): since 0024 two owners can each hold an event with the same external
// id, so matching the key alone would put A's event on B's feed, flag it deduped there, and count
// A's delivery in B's dedup badge. Matching the todo's EVENT owner rather than the todo's endpoint
// keeps a friend-route recipient's deduped redeliveries: their todo is on their own endpoint, but
// its event is still the sender's. The source is deliberately not matched, because two sources
// delivering one key is a dedup by design (TestTodoItemTrustModeAndDedupCount).
// Governing: ADR-0038, SPEC-0033 REQ "Closing the Audited Surfaces" (F14).
func eventDedupedOnto(param string) string {
	return `(e.external_id IS NOT NULL AND EXISTS (
		      SELECT 1 FROM todos d
		        JOIN events    de  ON de.id = d.event_id AND de.endpoint_id = e.endpoint_id
		        JOIN endpoints dep ON dep.id = d.endpoint_id
		        JOIN agents    dag ON dag.id = dep.agent_id
		       WHERE d.idempotency_key = e.external_id AND dag.owner_human_id = ` + param + `))`
}

func (s *Store) BoardStats(ctx context.Context, ownerHumanID string) (BoardStats, error) {
	var b BoardStats
	err := s.pool.QueryRow(ctx, `
		WITH mine AS (
			SELECT t.id, t.state, t.created_at, t.event_id
			FROM todos t
			JOIN endpoints ep ON ep.id = t.endpoint_id
			JOIN agents    ag ON ag.id = ep.agent_id AND ag.owner_human_id = $1
		)
		SELECT
			(SELECT count(*) FROM mine),
			(SELECT count(*) FROM mine WHERE created_at >= date_trunc('day', now())),
			(SELECT count(*) FROM mine WHERE state = 'claimed'),
			(SELECT count(*) FROM mine WHERE state = 'pending'),
			(SELECT COALESCE(round(100.0 * count(*) FILTER (WHERE e.trust_mode = 'signed') / NULLIF(count(*), 0)), 0)::int
			   FROM events e WHERE e.received_at >= date_trunc('day', now()) AND `+eventOwnedBy("$1")+`),
			(SELECT count(*) FROM events e WHERE e.received_at > now() - interval '1 minute'
			   AND `+eventOwnedBy("$1")+`)`, ownerHumanID,
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
func (s *Store) RecentEvents(ctx context.Context, ownerHumanID string, limit int) ([]EventSummary, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT e.id, e.source, COALESCE(e.event_type, ''), e.trust_mode, e.received_at,
			COALESCE(t.id, ''), COALESCE(t.state, ''), COALESCE(t.owner, ''),
			(t.id IS NULL AND `+eventDedupedOnto("$1")+`) AS deduped
		FROM events e
		-- The LATERAL picks the row the feed SHOWS, so it must be scoped too, not just the outer
		-- filter: without the join here a visible event would render another tenant's todo id,
		-- state and lease owner in the "patched → claimed" line.
		LEFT JOIN LATERAL (
			SELECT t.id, t.state, t.owner FROM todos t
			JOIN endpoints ep ON ep.id = t.endpoint_id
			JOIN agents    ag ON ag.id = ep.agent_id AND ag.owner_human_id = $1
			WHERE t.event_id = e.id ORDER BY t.created_at DESC LIMIT 1
		) t ON true
		WHERE `+eventOwnedBy("$1")+`
		ORDER BY e.received_at DESC, e.id DESC LIMIT $2`, ownerHumanID, limit)
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
func (s *Store) EventByID(ctx context.Context, ownerHumanID string, id int64) (EventSummary, error) {
	var e EventSummary
	err := s.pool.QueryRow(ctx, `
		SELECT e.id, e.source, COALESCE(e.event_type, ''), e.trust_mode, e.received_at
		FROM events e WHERE e.id = $2 AND `+eventOwnedBy("$1")+``, ownerHumanID, id,
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
func (s *Store) EventBuckets(ctx context.Context, ownerHumanID string, n int) ([]int, error) {
	if n <= 0 || n > 288 {
		n = 24
	}
	rows, err := s.pool.Query(ctx, `
		SELECT count(e.id)::int
		FROM generate_series($2 - 1, 0, -1) AS g(m)
		LEFT JOIN events e
			ON e.received_at >  now() - (g.m + 1) * interval '1 minute'
			AND e.received_at <= now() - g.m * interval '1 minute'
			AND `+eventOwnedBy("$1")+`
		GROUP BY g.m ORDER BY g.m DESC`, ownerHumanID, n)
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
