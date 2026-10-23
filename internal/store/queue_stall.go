// Queue-stall nudge: wake a consumer whose queue still holds claimable work nobody has claimed.
//
// Governing: ADR-0045 (proposed), SPEC-0024 REQ-6 (ready-hook trigger and sender gate), SPEC-0011
// REQ "Doorbell Heartbeat" (the session-attached half of the same gap).
//
// The wake chain so far is edge-triggered. A creation fires the ready hook on the instance that
// committed it, the reaper fires it when a lease or a retry returns a row to pending, and the doorbell
// heartbeat re-rings a pending todo — but only for an endpoint with a session attached to that
// instance (SPEC-0011's receiver filter). A consumer that claims one todo per invocation and is
// started by an outbound webhook (ADR-0029) has no session at all, so each of those edges is its only
// alarm. Spend one notification — or lose it to a restart, since the dispatcher persists nothing — and
// the queue can hold real, claimable work indefinitely with nothing left to prod it, because no new
// inbound webhook has arrived to fire an edge. That is #566.
//
// This is the level-triggered half: a periodic sweep that asks a question only the database can answer
// — "does this queue still have pending work older than the stall threshold, with nothing claimed
// since I last asked?" — and re-fires the ready hook for one todo in it. The queue stays the ledger end
// to end: the nudge carries no payload, loses nothing if delivery fails, and costs a stalled-but-
// healthy consumer nothing at all.
//
// Deliberately NOT endpoint-scoped, for the reason RequeueDueRetries and RingUnclaimed give: it is a
// system-wide maintenance sweep on a timer with no authenticated caller to scope to. It reads no todo
// out to anyone and moves no row; the wake it emits is scoped by the row's own endpoint_id, and the
// dispatcher re-checks the endpoint, its current queue scope and the hook's queue filter before every
// attempt. Governing: SPEC-0003 REQ "Endpoint Ownership (Tenant Isolation)" (background-sweep
// exemption), ADR-0022.

package store

import (
	"context"
	"fmt"
	"time"
)

const (
	// queueStallAfter is how long a todo must have sat `pending` before its queue counts as stalled.
	// It is a race guard, not a latency target: a consumer that is healthy and about to claim must not
	// be double-woken, so the queue's oldest pending row has to be older than any ordinary
	// claim-latency blip before the sweep concludes that nobody is coming.
	queueStallAfter = 2 * time.Minute

	// queueStallMaxNudges caps how many times one queue is nudged without making progress. It mirrors
	// ringMaxAttempts' reasoning: if five wakes spread over roughly a day have not produced a single
	// claim, the queue is not starved for a nudge — the consumer is broken, revoked, or never going to
	// drain it — and a sixth wake spends a model turn on the same bad news. A claim, or a drained
	// queue, resets the count, so the cap only ever binds a queue that is genuinely stuck.
	queueStallMaxNudges = 5

	// queueStallSweepLimit caps how many queues one tick may wake. Each wake can start a consumer,
	// which costs a model turn, so a fleet of stalled queues must not all fire at once; the least-
	// nudged queue goes first, so the cap still reaches every stalled queue, just spread over ticks.
	queueStallSweepLimit = 4
)

// queueStallBackoff is the wait before the Nth nudge of a queue since it last made progress (1-based,
// like ringBackoff). It widens for the same reason the doorbell heartbeat widens: a queue nobody is
// claiming is either going to be claimed or is telling us something is wrong, and in both cases the
// answer is not a wake every thirty seconds.
//
// The numbers are the heartbeat's on purpose: an operator reading a log line should not have to hold
// two schedules in their head, and a consumer sized for one is sized for both.
func queueStallBackoff(nudge int) time.Duration {
	switch {
	case nudge <= 1:
		return 5 * time.Minute
	case nudge == 2:
		return 20 * time.Minute
	case nudge == 3:
		return time.Hour
	default:
		return 6 * time.Hour
	}
}

// NudgeStalledQueues returns one todo per (endpoint, queue) whose oldest gated pending work has sat
// past queueStallAfter, that is past its backoff since the last nudge, and that still has an enabled
// notify hook able to receive the wake — and records the nudge in the same statement, so two instances
// (or two ticks) cannot both wake the same queue.
//
// Each returned todo fires the ready hook with reason ReadyStalled once the statement has committed,
// which is what carries it to the notify hooks. The channel doorbell is deliberately untouched:
// session-attached consumers are already covered by the doorbell heartbeat, and both paths share a
// per-todo gate whose one-minute window would suppress a nudge for exactly the pending rows a nudge
// targets. The wire format does not change either — a nudge is an ordinary todo.ready whose reason is
// `stalled` — so a consumer that does not know the new value still reads todo_id and claims.
//
// Loop guard (ADR-0045): the ledger row is the only thing that makes a periodic nudge safe. Its count
// widens the next delay; a claim anywhere in that queue both makes the queue due again and resets the
// count, because that queue is making progress and deserves the short cadence; and a queue with no
// pending rows left has its row deleted by this same statement, so the next backlog starts fresh
// instead of inheriting a spent budget. With no progress at all the cap stops the queue.
//
// SENDER GATE: the same predicate the create path and the heartbeat apply — the todo's delivery event
// exists and was verified, or arrived on a token-trust self-managed webhook. It is applied twice, to
// the queue's pending rows and to the representative todo picked to name the wake, so an unverified
// row can never have its title or source reach a notification body. It lives in the query rather than
// the publisher so it cannot be bypassed here either; an unverified queue ages quietly and stays
// pullable.
//
// RECEIVER FILTER: a queue with no enabled notify hook is not a candidate. RingUnclaimed learned this
// the hard way (SPEC-0011 REQ "Doorbell Heartbeat"): waking into an empty room spends pacing state the
// consumer needs when it comes back. Here an empty room is a queue whose work is reached by pull or by
// a session doorbell, so it must not burn the queue's nudge budget. Presence is left to the
// dispatcher's fire-time checks, as every other wake path leaves them.
//
// Concurrency: the ledger write re-checks the due predicate at lock time (ON CONFLICT ... WHERE), so a
// nudge another instance committed first neither double-counts nor double-delivers — narrower than
// RingUnclaimed's best-effort exclusivity, because the ledger is one keyed row rather than a window
// over many.
//
// Returns the todos the wakes name, oldest first, empty when nothing is due.
func (s *Store) NudgeStalledQueues(ctx context.Context) ([]Todo, error) {
	rows, err := s.pool.Query(ctx, `
		-- Drain resets first: a queue with nothing pending is not stalled, and its pacing state must
		-- not be inherited by the next backlog that lands on the same (endpoint, queue). The ledger is
		-- bounded by the number of queues with pending work, so this anti-join stays small; todos'
		-- partial pending index is keyed (endpoint_id, queue, created_at) and answers it.
		WITH drained AS (
			DELETE FROM queue_stall_nudges n
			 WHERE NOT EXISTS (SELECT 1 FROM todos t
				 WHERE t.endpoint_id = n.endpoint_id AND t.queue = n.queue
				   AND t.state = 'pending')
		),
		-- One row per queue that has any gate-passing pending work, with its oldest such row. The age
		-- test is a HAVING over that minimum, so a queue counts as stalled only when its OLDEST
		-- unclaimed todo is past the threshold — a just-landed todo is not evidence that nobody is
		-- coming.
		pairs AS (
			SELECT t.endpoint_id, t.queue, min(t.created_at) AS oldest_at
			FROM todos t
			-- A revoked endpoint's credential no longer authenticates, so nothing it is told about its
			-- own queues can be acted on. Same exclusion the heartbeat makes, for the same reason:
			-- revoked endpoints hold the oldest rows in the table, so an unguarded sweep reaches them
			-- first and spends the whole budget on work nobody can do.
			JOIN endpoints ep ON ep.id = t.endpoint_id AND ep.state = 'active'
			WHERE t.state = 'pending'
			  AND t.queue <> 'quarantine'
			  -- A paused queue is quiet on purpose (ADR-0048): nudging it would spend the queue's
			  -- wake budget on a consumer the operator has deliberately stilled, and the resume
			  -- re-rings the backlog anyway. Its ledger row waits, untouched.
			  AND NOT EXISTS (SELECT 1 FROM queue_pause qp WHERE qp.queue = t.queue)
			  AND t.event_id IS NOT NULL
			  AND EXISTS (SELECT 1 FROM events ev
			               WHERE ev.id = t.event_id AND (ev.verified OR ev.trust_mode = 'token'))
			  AND EXISTS (SELECT 1 FROM notify_hooks h
			               WHERE h.endpoint_id = t.endpoint_id AND h.enabled
			                 AND (cardinality(h.queues) = 0 OR t.queue = ANY(h.queues)))
			GROUP BY t.endpoint_id, t.queue
			HAVING min(t.created_at) < now() - $1::interval
		),
		due AS (
			SELECT p.endpoint_id, p.queue, p.oldest_at,
			       COALESCE(n.nudges, 0) AS nudges, n.last_nudged_at
			FROM pairs p
			LEFT JOIN queue_stall_nudges n ON n.endpoint_id = p.endpoint_id AND n.queue = p.queue
			WHERE COALESCE(n.nudges, 0) < $2
			  AND (n.last_nudged_at IS NULL
			       OR n.last_nudged_at < now() - (CASE
			              WHEN COALESCE(n.nudges, 0) <= 1 THEN $3::interval
			              WHEN n.nudges = 2 THEN $4::interval
			              WHEN n.nudges = 3 THEN $5::interval
			              ELSE $6::interval
			          END)
			       OR EXISTS (SELECT 1 FROM todos c
			                    WHERE c.endpoint_id = p.endpoint_id AND c.queue = p.queue
			                      AND c.claimed_at > n.last_nudged_at))
		),
		-- MATERIALIZED so the pick runs exactly once even though it feeds both the ledger write and
		-- the final read (the nested-loop rescan RingUnclaimed documents). Least-nudged queue first, so
		-- a queue that has been woken many times cannot absorb every tick.
		capped AS MATERIALIZED (
			SELECT endpoint_id, queue FROM due
			ORDER BY nudges, last_nudged_at NULLS FIRST, oldest_at
			LIMIT $7
		),
		-- The wake names ONE todo per queue rather than the queue alone: the notification shape already
		-- carries a todo_id (SPEC-0024 REQ-5), naming one row costs a consumer nothing to act on, and a
		-- 50-row backlog produces one POST rather than fifty. The gate is re-applied here so the row
		-- whose title and source travel in the body is a gate-passing one, oldest first.
		picked AS MATERIALIZED (
			SELECT c.endpoint_id, c.queue,
			       (SELECT t.id FROM todos t
			         WHERE t.endpoint_id = c.endpoint_id AND t.queue = c.queue
			           AND t.state = 'pending' AND t.queue <> 'quarantine'
			           AND t.event_id IS NOT NULL
			           AND EXISTS (SELECT 1 FROM events ev
			                          WHERE ev.id = t.event_id AND (ev.verified OR ev.trust_mode = 'token'))
			         ORDER BY t.created_at
			         LIMIT 1) AS todo_id
			FROM capped c
		),
		marked AS (
			INSERT INTO queue_stall_nudges (endpoint_id, queue, nudges, last_nudged_at)
			SELECT endpoint_id, queue, 1, now() FROM picked
			ON CONFLICT (endpoint_id, queue) DO UPDATE
				SET nudges = CASE
				        WHEN EXISTS (SELECT 1 FROM todos c
				                      WHERE c.endpoint_id = EXCLUDED.endpoint_id
				                        AND c.queue = EXCLUDED.queue
				                        AND c.claimed_at > queue_stall_nudges.last_nudged_at)
				        THEN 1
				        ELSE queue_stall_nudges.nudges + 1
				    END,
				    last_nudged_at = now()
				-- Re-checked at lock time: an instance that committed its nudge for this queue first
				-- leaves this row not due, so it returns nothing and the queue is woken once.
				WHERE queue_stall_nudges.last_nudged_at IS NULL
				   OR queue_stall_nudges.last_nudged_at < now() - (CASE
				          WHEN queue_stall_nudges.nudges <= 1 THEN $3::interval
				          WHEN queue_stall_nudges.nudges = 2 THEN $4::interval
				          WHEN queue_stall_nudges.nudges = 3 THEN $5::interval
				          ELSE $6::interval
				      END)::interval
				   OR EXISTS (SELECT 1 FROM todos c
				               WHERE c.endpoint_id = EXCLUDED.endpoint_id
				                 AND c.queue = EXCLUDED.queue
				                 AND c.claimed_at > queue_stall_nudges.last_nudged_at)
			RETURNING endpoint_id::text AS endpoint_id, queue
		)
		SELECT `+todoCols+` FROM todos
		 WHERE id IN (SELECT p.todo_id FROM picked p
		               WHERE (p.endpoint_id::text, p.queue) IN
		                     (SELECT m.endpoint_id, m.queue FROM marked m))
		 ORDER BY created_at`,
		queueStallAfter,
		queueStallMaxNudges,
		queueStallBackoff(1), queueStallBackoff(2), queueStallBackoff(3), queueStallBackoff(4),
		queueStallSweepLimit)
	if err != nil {
		return nil, fmt.Errorf("store: nudge stalled queues: %w", err)
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// The wake follows the record: the statement that charged these queues has committed by the time
	// its rows are drained, so a consumer woken here can never be told about a nudge the ledger does
	// not know about, and two instances cannot both be told about one charge.
	for _, t := range out {
		s.fireReady(t, ReadyStalled)
	}
	return out, nil
}
