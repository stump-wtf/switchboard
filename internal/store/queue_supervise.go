// Operator queue supervision reads
//
// Two reads an operator lane needs and no consumer read provides: how live each queue is inside one
// human's tenant, and which leases are outstanding on it. "Is this pipeline stalled or just quiet?"
// is one question with two very different answers, and before this pair it took a page of listing and
// some guesswork to tell them apart.
//
// Both reads carry ownedByHuman, the same tenant predicate the Board's reads carry: a count is a
// disclosure, so per-queue liveness is scoped exactly as per-row listing is. QueueStats
// (queue_stats.go) stays what it always was — the fleet-wide scrape read behind the Prometheus
// gauges, served to the scrape credential and never to an endpoint credential.
//
// The queue spine is the caller's granted queue list, not the rows: a queue that is granted and empty
// reports as a row of zeros (the zero-value series QueueStats already keeps), because "nothing
// waiting" is the answer, not absence of an answer. A paused queue is likewise a row, with its
// pause carried from queue_pause (ADR-0048) rather than from a second switch.
//
// Governing: ADR-0048 (pause state), ADR-0022 and SPEC-0007 (the tenant boundary), ADR-0056
// (operator supervision over MCP), SPEC-0023 REQ-2 (the liveness pair).

package store

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
)

// SuperviseLimitMax is the ceiling on a held-lease listing: the same clamp every operator listing
// applies, so one call cannot stream an unbounded table into an agent's context.
const SuperviseLimitMax = 200

// QueueLiveness is one queue's liveness inside one human's tenant at the instant of the read.
type QueueLiveness struct {
	Queue        string
	Pending      int
	Claimed      int
	Done         int
	Failed       int
	Blocked      int
	DeadLettered int
	// OldestPendingSeconds is the age of the oldest pending row this human owns in this queue, or 0
	// when none is pending. With Paused it is what separates quiet from stalled.
	OldestPendingSeconds float64
	// Paused carries ADR-0048's state: work is waiting by decision, not for want of a consumer.
	Paused bool
}

// QueueLivenessForHuman returns one row per queue in queues, zero-filled, sorted by queue name,
// counting only the todos owned by ownerHumanID. Queues must not contain duplicates; an empty list
// reads nothing and returns nothing. It honours ctx, so a caller can bound it with a deadline.
func (s *Store) QueueLivenessForHuman(ctx context.Context, ownerHumanID string, queues []string) ([]QueueLiveness, error) {
	unique := dedupeSorted(queues)
	if len(unique) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		WITH counted AS (
			SELECT t.queue,
				count(*) FILTER (WHERE t.state = 'pending') AS pending,
				count(*) FILTER (WHERE t.state = 'claimed') AS claimed,
				count(*) FILTER (WHERE t.state = 'done')    AS done,
				count(*) FILTER (WHERE t.state = 'failed')  AS failed,
				count(*) FILTER (WHERE t.state = 'blocked') AS blocked,
				count(*) FILTER (WHERE t.state = 'failed' AND t.next_retry_at IS NULL
					AND t.queue <> 'quarantine')           AS dead_lettered,
				min(t.created_at) FILTER (WHERE t.state = 'pending') AS oldest_pending
			FROM todos t`+ownedByHuman+`$1
			WHERE t.queue = ANY($2::text[])
			GROUP BY t.queue
		)
		SELECT k.queue,
			COALESCE(c.pending, 0), COALESCE(c.claimed, 0), COALESCE(c.done, 0),
			COALESCE(c.failed, 0), COALESCE(c.blocked, 0), COALESCE(c.dead_lettered, 0),
			COALESCE(GREATEST(EXTRACT(EPOCH FROM now() - c.oldest_pending), 0), 0)::float8,
			(qp.queue IS NOT NULL)
		FROM unnest($2::text[]) AS k(queue)
			LEFT JOIN counted c    ON c.queue = k.queue
			LEFT JOIN queue_pause qp ON qp.queue = k.queue
		WHERE k.queue IS NOT NULL
		ORDER BY k.queue`, ownerHumanID, unique)
	if err != nil {
		return nil, fmt.Errorf("store: queue liveness: %w", err)
	}
	defer rows.Close()
	var out []QueueLiveness
	for rows.Next() {
		var q QueueLiveness
		if err := rows.Scan(&q.Queue, &q.Pending, &q.Claimed, &q.Done, &q.Failed, &q.Blocked,
			&q.DeadLettered, &q.OldestPendingSeconds, &q.Paused); err != nil {
			return nil, fmt.Errorf("store: scan queue liveness: %w", err)
		}
		out = append(out, q)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: queue liveness: %w", err)
	}
	return out, nil
}

// HeldLease is one live claim: who holds the todo, and until when.
type HeldLease struct {
	TodoID string
	Queue  string
	Owner  string
	// Attempt is the count the next claim will follow, so an operator can see a row that is about to
	// dead-letter on its own lease expiring.
	Attempt int
	// LeaseExpiresAt is nil on a claimed row with no lease written (an ingestion anomaly; the lease
	// reaper treats it as expired, and so does the read).
	LeaseExpiresAt *time.Time
}

// OutstandingLeasesForHuman lists the live claims inside one human's tenant over the given queues,
// soonest-expiring first, at most limit rows (SuperviseLimitMax when unset). It is the drain's
// progress read and the lease half of a stats snapshot: "what is still being worked, and by whom".
func (s *Store) OutstandingLeasesForHuman(ctx context.Context, ownerHumanID string, queues []string, limit int) ([]HeldLease, error) {
	unique := dedupeSorted(queues)
	if len(unique) == 0 {
		return nil, nil
	}
	if limit <= 0 || limit > SuperviseLimitMax {
		limit = SuperviseLimitMax
	}
	rows, err := s.pool.Query(ctx, `
		SELECT t.id, t.queue, COALESCE(t.owner, ''), t.attempt, t.lease_expires_at
		FROM todos t`+ownedByHuman+`$1
		WHERE t.state = 'claimed' AND t.queue = ANY($2::text[])
		ORDER BY t.lease_expires_at NULLS FIRST, t.id
		LIMIT $3`, ownerHumanID, unique, limit)
	if err != nil {
		return nil, fmt.Errorf("store: outstanding leases: %w", err)
	}
	defer rows.Close()
	var out []HeldLease
	for rows.Next() {
		var l HeldLease
		if err := rows.Scan(&l.TodoID, &l.Queue, &l.Owner, &l.Attempt, &l.LeaseExpiresAt); err != nil {
			return nil, fmt.Errorf("store: scan outstanding leases: %w", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: outstanding leases: %w", err)
	}
	return out, nil
}

// dedupeSorted returns the queue names that matter to a supervision read: de-duplicated, trimmed,
// sorted. Sorting is what lets the read's rows be ordered by queue name whatever order the caller's
// grant happened to be stored in.
func dedupeSorted(queues []string) []string {
	out := make([]string, 0, len(queues))
	for _, q := range queues {
		if q = strings.TrimSpace(q); q != "" {
			out = append(out, q)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}
