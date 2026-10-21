// Per-queue pause: quiesce delivery to a queue's consumers without stopping intake.
//
// Governing: ADR-0048 (proposed), ADR-0007 (todos are the durable queue), SPEC-0003 (queues are a
// global namespace).
//
// An operator draining a downstream consumer needs "stop handing out work from this queue for now",
// not "disable the endpoint": disabling drops or fails inbound webhooks, and stopping the consumer
// does not stop the doorbell and channel wakes that keep starting runs (#581). The queue is the one
// place both decisions are authoritative, so pause lives here: intake keeps persisting todos exactly
// as before, and every delivery path treats a paused queue as empty — the claim predicates skip its
// rows (todos.go), the doorbell reads skip them (RingUnclaimed, RingOnAttach, PendingDoorbellTodos),
// the stall sweep leaves its backlog quiet (queue_stall.go), and fireReady/fireDoorbell (store.go)
// hold back the in-process wakes. A paused queue loses nothing: its todos stay pending, keep their
// dedup slots, and spend no attempts.
//
// Pause state lives in Postgres, not in memory: delivery spans instances (the LISTEN loop wakes
// sessions on other processes), and an operator's decision must outlive a restart of the instance
// that received it. Enforcement at claim time reads the same database the claim locks, so there is
// no propagation window: pausing cannot race a claim past it.

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// wakeGateTimeout bounds the pause probe that guards each in-process wake. The wake paths are
// contractually never-allowed-to-block advisory paths; a probe that misses its deadline delivers
// anyway (see queuePausedQuiet), because a lost wake costs latency while claim-time enforcement
// still holds the line.
const wakeGateTimeout = 2 * time.Second

// QueuePause is one paused queue and why.
type QueuePause struct {
	Queue    string
	PausedBy string // operator human id; empty when unknown
	Reason   string
	PausedAt time.Time
}

// PauseQueue quiesces delivery for queue until ResumeQueue removes it. Upserting: re-pausing an
// already-paused queue refreshes the reason and who paused it without touching the original
// paused_at. Intake is untouched — webhooks keep creating todos on the queue the whole time.
// The quarantine queue cannot be paused: it is already never delivered, and a pause there would
// imply its contents are waiting to be handed out.
func (s *Store) PauseQueue(ctx context.Context, queue, pausedBy, reason string) error {
	queue = strings.TrimSpace(queue)
	if queue == "" {
		return errors.New("store: queue name must not be empty")
	}
	if queue == QueueQuarantine {
		return errors.New("store: the quarantine queue is never delivered, so it cannot be paused")
	}
	by := any(nil)
	if strings.TrimSpace(pausedBy) != "" {
		by = pausedBy
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO queue_pause (queue, paused_by, reason)
		VALUES ($1, $2, $3)
		ON CONFLICT (queue) DO UPDATE SET paused_by = EXCLUDED.paused_by, reason = EXCLUDED.reason`,
		queue, by, reason)
	if err != nil {
		return fmt.Errorf("store: pause queue %s: %w", queue, err)
	}
	return nil
}

// ResumeQueue lifts the pause and re-rings the backlog: the queue's pending todos become claimable
// the moment the delete commits, and this fires the same wake chain an enqueue uses — the
// todo_ready NOTIFY (which re-rings the channel doorbell on every instance) and the ready hook with
// reason ReadyResumed naming the oldest pending todo, which is what carries the wake to notify-hook
// consumers that have no session. A queue that was never paused resumes quietly (no wake: there is
// no backlog the consumers were kept from). The returned bool says whether a pause was lifted.
func (s *Store) ResumeQueue(ctx context.Context, queue string) (bool, error) {
	queue = strings.TrimSpace(queue)
	if queue == "" {
		return false, errors.New("store: queue name must not be empty")
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM queue_pause WHERE queue = $1`, queue)
	if err != nil {
		return false, fmt.Errorf("store: resume queue %s: %w", queue, err)
	}
	resumed := tag.RowsAffected() > 0
	if !resumed {
		return false, nil
	}
	// The delete has committed, so every wake path's pause gate now passes for this queue: the
	// resume wake cannot be swallowed by the state it exists to undo. A queue-only NOTIFY (no
	// endpoint prefix) is the documented legacy shape of the payload and reads the backlog across
	// endpoints — pause is queue-global, so its resume is too.
	_, _ = s.pool.Exec(ctx, `SELECT pg_notify('todo_ready', $1)`, queue)
	if t, ok := s.oldestPendingInQueue(ctx, queue); ok {
		s.fireReady(t, ReadyResumed)
	}
	return true, nil
}

// QueuePaused reports whether queue is currently paused. On a read failure it reports false and
// logs: the pause is advisory only on wake paths (it can cost one extra wake), and the claim
// predicates enforce delivery suppression in the same transaction the claim takes.
func (s *Store) QueuePaused(ctx context.Context, queue string) (bool, error) {
	var one int
	err := s.pool.QueryRow(ctx, `SELECT 1 FROM queue_pause WHERE queue = $1`, queue).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: pause check %s: %w", queue, err)
	}
	return true, nil
}

// queuePausedQuiet is the wake-path gate: it bounds the probe and fails open — an unreadable pause
// state costs at most one wake on a queue that might be paused, never a silently lost wake on one
// that is not. Claim-time enforcement in the SQL predicates is the authoritative half.
func (s *Store) queuePausedQuiet(queue string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), wakeGateTimeout)
	defer cancel()
	paused, err := s.QueuePaused(ctx, queue)
	if err != nil {
		if s.log != nil {
			s.log.Warn("store: queue pause probe failed; delivering wake anyway", "queue", queue, "err", err)
		}
		return false
	}
	return paused
}

// PausedQueues returns the paused subset of queues (order preserved), for surfacing "why nothing
// is moving" to whoever is allowed to see the queue (MCP list_todos, the board).
func (s *Store) PausedQueues(ctx context.Context, queues []string) ([]string, error) {
	if len(queues) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT queue FROM queue_pause WHERE queue = ANY($1)`, queues)
	if err != nil {
		return nil, fmt.Errorf("store: paused queues: %w", err)
	}
	defer rows.Close()
	set := map[string]bool{}
	for rows.Next() {
		var q string
		if err := rows.Scan(&q); err != nil {
			return nil, err
		}
		set[q] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []string
	for _, q := range queues {
		if set[q] {
			out = append(out, q)
		}
	}
	return out, nil
}

// ListQueuePauses returns every paused queue, oldest pause first — the operator's "which queues
// am I holding?" view.
func (s *Store) ListQueuePauses(ctx context.Context) ([]QueuePause, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT queue, COALESCE(paused_by::text, ''), reason, paused_at
		FROM queue_pause ORDER BY paused_at, queue`)
	if err != nil {
		return nil, fmt.Errorf("store: list pauses: %w", err)
	}
	defer rows.Close()
	var out []QueuePause
	for rows.Next() {
		var p QueuePause
		if err := rows.Scan(&p.Queue, &p.PausedBy, &p.Reason, &p.PausedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// oldestPendingInQueue picks the row to name a resume wake. The wake carries only a todo id (the
// consumer pulls), so one representative per queue is the whole signal — the backlog itself is the
// queue, and the doorbell half of the resume re-rings up to nudgeBatch pending rows per instance.
func (s *Store) oldestPendingInQueue(ctx context.Context, queue string) (Todo, bool) {
	row := s.pool.QueryRow(ctx, `SELECT `+todoCols+` FROM todos
		WHERE queue = $1 AND state = 'pending' ORDER BY created_at LIMIT 1`, queue)
	t, err := scanTodo(row)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) && s.log != nil {
			s.log.Warn("store: resume backlog read failed", "queue", queue, "err", err)
		}
		return Todo{}, false
	}
	return t, true
}
