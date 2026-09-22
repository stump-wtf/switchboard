package store

// Queue liveness snapshot
//
// QueueStats is the single read behind the SPEC-0023 REQ-2 gauges: for every known queue, how many
// todos sit in each of the four lifecycle states and how old the head of the pending line is. The
// metrics collector calls it once per scrape; it is never maintained inline, so it cannot drift
// from the table it reads (design.md "Shape").
//
// The queue set is KnownQueues' definition — queues todos have ridden plus queues scoped onto
// vended endpoints — not whatever the count happens to return, so a queue nobody has ever enqueued
// onto still reports as a row of zeros (design.md "Zero-value series"). One statement does both:
// the grouped count scans todos once, and its own queue column supplies the todo half of the known
// set, which is then unioned with the endpoint scopes and left-joined back to the counts.
//
// The A2A states in the todos_state_check domain (canceled, rejected, input-required,
// auth-required; migration 0012) are deliberately not counted: REQ-2 enumerates exactly four.
//
// Governing: SPEC-0023 REQ-2 "Queue liveness — the mandatory pair"; design.md "Shape",
// "Zero-value series", "Cardinality control"; ADR-0028.
//
// @joestump-agent 09/21/2026 - Added for issue #273 (SPEC-0023 story 2).

import (
	"context"
	"fmt"
)

// QueueStat is one known queue's liveness snapshot at the instant of the read.
type QueueStat struct {
	Queue   string
	Pending int64
	Claimed int64
	Done    int64
	Failed  int64
	// OldestPendingSeconds is the age of the oldest pending todo, or 0 when none is pending.
	OldestPendingSeconds float64
}

// QueueStats returns a snapshot row for every known queue, zero-filled, sorted by queue name. It is
// one round trip and honours ctx, so the scrape-time collector can bound it with a deadline.
//
// Queues are a global namespace (SPEC-0003), like KnownQueues: the counts are fleet-wide and are
// served only to the operator scrape credential, never to an endpoint.
func (s *Store) QueueStats(ctx context.Context) ([]QueueStat, error) {
	// GREATEST clamps a row whose inserting transaction began after this statement's now() but
	// committed before its snapshot — a few microseconds of negative age, never a real reading.
	rows, err := s.pool.Query(ctx, `
		WITH counts AS (
			SELECT queue,
			       count(*) FILTER (WHERE state = 'pending') AS pending,
			       count(*) FILTER (WHERE state = 'claimed') AS claimed,
			       count(*) FILTER (WHERE state = 'done')    AS done,
			       count(*) FILTER (WHERE state = 'failed')  AS failed,
			       min(created_at) FILTER (WHERE state = 'pending') AS oldest_pending
			FROM todos
			GROUP BY queue
		), known AS (
			SELECT queue FROM counts
			UNION
			SELECT unnest(scope_queues) FROM endpoints
		)
		SELECT k.queue,
		       COALESCE(c.pending, 0), COALESCE(c.claimed, 0),
		       COALESCE(c.done, 0),    COALESCE(c.failed, 0),
		       COALESCE(GREATEST(EXTRACT(EPOCH FROM now() - c.oldest_pending), 0), 0)::float8
		FROM known k
		LEFT JOIN counts c ON c.queue = k.queue
		WHERE k.queue IS NOT NULL
		ORDER BY k.queue`)
	if err != nil {
		return nil, fmt.Errorf("store: queue stats: %w", err)
	}
	defer rows.Close()
	var out []QueueStat
	for rows.Next() {
		var q QueueStat
		if err := rows.Scan(&q.Queue, &q.Pending, &q.Claimed, &q.Done, &q.Failed, &q.OldestPendingSeconds); err != nil {
			return nil, fmt.Errorf("store: scan queue stats: %w", err)
		}
		out = append(out, q)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: queue stats: %w", err)
	}
	return out, nil
}
