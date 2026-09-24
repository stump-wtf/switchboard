// Governing: ADR-0021 (A2A task-delegation transport), SPEC-0018 REQ "Task State Machine Extension",
// SPEC-0018 REQ "Database Operation Standards".
//
// The store-level transition primitives for the four A2A states the 0012 migration added
// (canceled, rejected, input-required, auth-required). Each transition is a single atomic,
// parameterized UPDATE — the state change and the columns that record its outcome (result,
// completed_at, updated_at) commit together or not at all (SPEC-0018 "State transition and audit
// record are atomic") — exactly mirroring the existing CompleteTodo/FailTodo shape in todos.go. The
// committed transition then fires the same best-effort transition hook every other lifecycle move
// uses, so the Board and (downstream) A2A streaming observe these states through one path.
//
// These are the durable primitives; the A2A HTTP/JSON-RPC handlers (SendMessage/CancelTask/…,
// separate stories) are thin translators that call them. Keeping the transitions here means MCP and
// A2A share one state machine, one audit trail, one lease/retry implementation (ADR-0021).
package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// CancelTodo transitions a live (non-terminal) todo to the terminal `canceled` state — the durable
// backing for A2A's CancelTask. The cancelable set is every non-terminal state: `pending`,
// `claimed`, and the two interrupt states (`input-required`/`auth-required`), which are in-progress
// work paused on the client and MUST also be cancelable (SPEC-0018 REQ "CancelTask Is Idempotent":
// cancel transitions a todo "out of its claimable/in-progress states"). A `failed` todo is excluded:
// it is a resting/dead-letter state, not an in-flight one, and cancel is not its lifecycle move.
// Unlike `failed`, a canceled todo is never retried, never dead-lettered, and never re-enters
// `pending`: it is explicitly canceled, not retried out (SPEC-0018 REQ "Task State Machine
// Extension"). Owner and lease are cleared (any in-flight worker learns via the transition hook /
// doorbell path). The result payload records the cancellation reason as the transition's audit
// record, committed in the same UPDATE.
//
// CancelTask is specified idempotent (SPEC-0018 REQ "CancelTask Is Idempotent"): calling it on an
// already-`canceled` todo MUST succeed as a no-op. This primitive reports that case as ErrConflict
// (the row exists but is not in a cancelable state); the A2A handler translates an already-terminal
// `canceled` todo into a success, and any other non-cancelable state into the appropriate error.
// Returns ErrConflict if the todo exists but is terminal/not cancelable, ErrNotFound if absent.
func (s *Store) CancelTodo(ctx context.Context, id string, result []byte) (Todo, error) {
	// A claimed or interrupted todo's open attempt closes canceled/canceled in the same statement
	// (SPEC-0034 REQ-3); a pending todo has none.
	row := s.pool.QueryRow(ctx, `
		WITH upd AS (
			UPDATE todos SET state='canceled', owner=NULL, lease_expires_at=NULL,
				next_retry_at=NULL, result=$2, completed_at=now(), updated_at=now()
			WHERE id=$1 AND state IN ('pending', 'claimed', 'input-required', 'auth-required')
			RETURNING todos.*
		),
		`+closedArmUnreported("canceled", "'canceled'")+`
		`+closedSelect, id, result)
	var closed bool
	t, err := scanTodo(row, &closed)
	if errors.Is(err, pgx.ErrNoRows) {
		return Todo{}, s.classifyMiss(ctx, "", id)
	}
	if err == nil && closed {
		s.countAttemptClosed(t.Queue, "canceled")
	}
	if err == nil {
		s.fireTodoHook("canceled", t)
	}
	return t, err
}

// RejectTodo transitions a pending, never-claimed todo to the terminal `rejected` state — e.g. an
// intake policy check fails after the todo is durably enqueued. `rejected` is distinct from `failed`
// (SPEC-0018 REQ "Task State Machine Extension"): a rejected todo was never claimed and MUST NOT be
// eligible for the retry/backoff behavior `failed` todos get, so this sets next_retry_at NULL and
// leaves owner/attempt untouched (there was no worker and no attempt to record). The guard is
// state='pending' precisely because a rejection happens before any claim; a claimed todo that must
// stop is a cancel, not a reject. The result payload records the rejection reason as the audit
// record, committed in the same UPDATE. Returns ErrConflict if the todo exists but is not pending,
// ErrNotFound if absent.
func (s *Store) RejectTodo(ctx context.Context, id string, result []byte) (Todo, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE todos SET state='rejected', lease_expires_at=NULL, next_retry_at=NULL,
			result=$2, completed_at=now(), updated_at=now()
		WHERE id=$1 AND state='pending'
		RETURNING `+todoCols, id, result)
	t, err := scanTodo(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Todo{}, s.classifyMiss(ctx, "", id)
	}
	if err == nil {
		s.fireTodoHook("rejected", t)
	}
	return t, err
}

// InterruptTodo moves a claimed todo into an A2A interrupt state (`input-required` or
// `auth-required`) — the worker has paused, waiting on the client for input or on auth resolution.
// It is NOT terminal and NOT a release: the owner and lease are deliberately RETAINED (SPEC-0018
// scenario "Interrupt states return to claimed") so the same worker resumes when the requirement is
// supplied; nothing else may claim the todo in the meantime. Only the current lease owner may
// interrupt (guarded by state='claimed' AND owner), and it does not consume an attempt. The result
// payload carries the interrupt detail (e.g. the input schema the client must satisfy). state is a
// bound parameter validated against the interrupt set below, so this cannot be used to write an
// arbitrary state. Returns ErrConflict if the todo exists but is not a live claim owned by owner,
// ErrNotFound if absent.
func (s *Store) InterruptTodo(ctx context.Context, id, owner, state string, result []byte) (Todo, error) {
	if !IsInterruptState(state) {
		return Todo{}, errors.New("store: InterruptTodo requires an interrupt state (input-required|auth-required)")
	}
	row := s.pool.QueryRow(ctx, `
		UPDATE todos SET state=$3, result=$4, updated_at=now()
		WHERE id=$1 AND state='claimed' AND owner=$2
		RETURNING `+todoCols, id, owner, state, result)
	t, err := scanTodo(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Todo{}, s.classifyMiss(ctx, "", id)
	}
	if err == nil {
		s.fireTodoHook(t.State, t)
	}
	return t, err
}

// ResumeTodo returns an interrupted todo (`input-required` or `auth-required`) to `claimed` once the
// required input or auth is supplied (SPEC-0018 scenario "Interrupt states return to claimed"). It
// goes back to `claimed`, NOT `pending`, and MUST retain the existing owner and lease — so the guard
// is on the current owner and the UPDATE touches neither owner nor lease_expires_at nor attempt. The
// lease continues on its original deadline (the worker was expected to heartbeat across the
// interrupt); a lease that lapsed while interrupted is recovered by the normal reaper/claim path once
// the todo is back in `claimed`. Returns ErrConflict if the todo exists but is not an interrupt state
// owned by owner, ErrNotFound if absent.
func (s *Store) ResumeTodo(ctx context.Context, id, owner string, result []byte) (Todo, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE todos SET state='claimed', result=$3, updated_at=now()
		WHERE id=$1 AND owner=$2 AND state IN ('input-required', 'auth-required')
		RETURNING `+todoCols, id, owner, result)
	t, err := scanTodo(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Todo{}, s.classifyMiss(ctx, "", id)
	}
	if err == nil {
		s.fireTodoHook("claimed", t)
	}
	return t, err
}
