// Governing: ADR-0021 (A2A task-delegation transport), ADR-0046 (proposed: the interrupt-lease TTL the
// sweep at the end of this file applies), SPEC-0018 REQ "Task State Machine Extension",
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
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stump-wtf/switchboard/internal/lease"
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
			UPDATE todos SET state='canceled', owner=NULL, lease_expires_at=NULL, interrupted_at=NULL,
				next_retry_at=NULL, result=$2, completed_at=now(), updated_at=now()
			WHERE id=$1 AND queue <> 'quarantine' AND state IN ('pending', 'claimed', 'input-required', 'auth-required')
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
		WHERE id=$1 AND queue <> 'quarantine' AND state='pending'
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
// arbitrary state. `interrupted_at` is stamped here: it is the only start time this wait has, because
// the retained lease was set by the claim and a row in an interrupt state cannot be heartbeated
// (ADR-0046). Returns ErrConflict if the todo exists but is not a live claim owned by owner,
// ErrNotFound if absent.
func (s *Store) InterruptTodo(ctx context.Context, id, owner, state string, result []byte) (Todo, error) {
	if !IsInterruptState(state) {
		return Todo{}, errors.New("store: InterruptTodo requires an interrupt state (input-required|auth-required)")
	}
	row := s.pool.QueryRow(ctx, `
		UPDATE todos SET state=$3, result=$4, interrupted_at=now(), updated_at=now()
		WHERE id=$1 AND queue <> 'quarantine' AND state='claimed' AND owner=$2
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
// the todo is back in `claimed`. Clearing `interrupted_at` here is what stops the interrupt-TTL sweep
// from ever reclaiming a row somebody has come back for (ADR-0046). Returns ErrConflict if the todo
// exists but is not an interrupt state owned by owner, ErrNotFound if absent.
func (s *Store) ResumeTodo(ctx context.Context, id, owner string, result []byte) (Todo, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE todos SET state='claimed', result=$3, interrupted_at=NULL, updated_at=now()
		WHERE id=$1 AND queue <> 'quarantine' AND owner=$2 AND state IN ('input-required', 'auth-required')
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

// Interrupt-lease reaping — the deadline on a wait whose holder is gone.
//
// InterruptTodo deliberately retains owner and lease so the SAME holder resumes (SPEC-0018 scenario
// "Interrupt states return to claimed"), and while a row sits in an interrupt state that holder cannot
// heartbeat: HeartbeatTodo is guarded on state='claimed'. The retained lease is therefore a clock set by
// the claim, not by the wait, and it is the only clock there is.
//
// So when the holder dies mid-interrupt the row has no alarm left: the lease reaper's predicate needs
// state='claimed', and the claim predicate covers pending, lapsed claimed and due failed. The todo is
// neither reapable nor claimable and parks until someone calls resume — forever, which is the outcome
// SPEC-0018's design.md lists as an unresolved Open Question. 0017 dead-letters the same shape for a
// REVOKED endpoint; this is the live-endpoint half of that gap, which is #594.
//
// Governing: ADR-0046 (proposed — it sets the TTL, the past-TTL outcome and the override recorded here),
// SPEC-0018 REQ "Task State Machine Extension", ADR-0002 (reaper), SPEC-0003 background-sweep exemption.

const (
	// interruptLeaseTTLDefault is how long a todo may wait in an interrupt state before the sweep
	// concludes nobody is coming: 24h, which equals lease.MaxTTL, so the wait always outlives the
	// longest claim that could still be alive to resume it. A day is also long enough that a human
	// answering an input request from a weekday-evening phone is not racing it.
	interruptLeaseTTLDefault = 24 * time.Hour

	// settingInterruptLeaseTTLSeconds is the operator override, read the way sse_retry_ms is read: a
	// settings row, a documented default, and a malformed value that logs and keeps the default. Any
	// non-positive value switches the sweep off, which is SPEC-0018's other acceptable answer — "leaves
	// it interrupted indefinitely" — now an operator's choice instead of an accident.
	settingInterruptLeaseTTLSeconds = "interrupt_lease_ttl_seconds"
)

// interruptLeaseTTL resolves the override into a duration, or 0 when the sweep is disabled. A positive
// value below lease.MinDefaultSeconds is raised to that floor: an interrupt deadline shorter than the
// shortest lease an endpoint may be given would reap waits the lease itself would have honoured, which
// would make the interrupt state strictly worse than working through.
func (s *Store) interruptLeaseTTL(ctx context.Context) (time.Duration, error) {
	secs, err := s.SettingInt(ctx, settingInterruptLeaseTTLSeconds, int(interruptLeaseTTLDefault/time.Second))
	if err != nil {
		return 0, err
	}
	if secs <= 0 {
		return 0, nil
	}
	if secs < lease.MinDefaultSeconds {
		secs = lease.MinDefaultSeconds
	}
	return time.Duration(secs) * time.Second, nil
}

// ReapInterrupted returns to the queue (or dead-letters, at the attempt cap) every todo still sitting in
// an interrupt state past the interrupt TTL — the crash-safety counterpart of ReapExpired for a row whose
// lease was retained on purpose. Same shape as that sweep, on purpose: the open attempt closes reaped
// (requeued, or dead_lettered at the cap), the ready hook fires for whatever lands in pending, the
// transition hook fires per row, and each row counts one lease expiry plus one interrupt reclaim.
//
// The owner and lease are dropped and the row goes to `pending` rather than back to `claimed`: nobody may
// hold a row whose holder is gone, and the interrupt's detail stays in `result`, so the next claimant
// reads what the client was asked for instead of starting blind. A dead-letter names its cause in
// `result` the way migration 0017's backfill does, so a row found months later explains itself.
//
// DELIBERATELY NOT endpoint-scoped, for the reason ReapExpired and NudgeStalledQueues give: a timer-driven
// system sweep has no authenticated caller to scope to, and crash recovery must not depend on someone
// being logged in. Governing: SPEC-0003 REQ "Endpoint Ownership (Tenant Isolation)" (background-sweep
// exemption), ADR-0022.
//
// Returns the number of todos reclaimed; 0 with no error when the TTL is switched off.
func (s *Store) ReapInterrupted(ctx context.Context) (int64, error) {
	ttl, err := s.interruptLeaseTTL(ctx)
	if err != nil || ttl == 0 {
		return 0, err
	}
	rows, err := s.pool.Query(ctx, `
		WITH upd AS (
			UPDATE todos SET
				state = CASE WHEN attempt >= max_attempts THEN 'failed' ELSE 'pending' END,
				owner = NULL, lease_expires_at = NULL, interrupted_at = NULL,
				result = CASE WHEN attempt >= max_attempts
					THEN jsonb_build_object(
						'error', 'interrupt_expired',
						'detail', 'the todo waited past the interrupt TTL for input or auth that never arrived')
					ELSE result END,
				updated_at = now()
			WHERE state IN ('input-required', 'auth-required') AND queue <> 'quarantine'
			  AND interrupted_at < now() - $1::interval
			RETURNING todos.*
		),
		`+closedArmUnreported("reaped", `CASE WHEN upd.state = 'failed' THEN 'dead_lettered' ELSE 'requeued' END`)+`
		`+reapedSelect, ttl.String())
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var reclaimed []Todo
	var closed []bool
	var eligible []bool
	for rows.Next() {
		var f finalScan
		var pushEligible bool
		t, err := scanTodo(rows, append(f.dest(), &pushEligible)...)
		if err != nil {
			return 0, err
		}
		f.attach(&t) // reclaimed at the cap carries its final attempt (SPEC-0034 REQ-15)
		reclaimed = append(reclaimed, t)
		closed = append(closed, f.closed)
		eligible = append(eligible, pushEligible)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	s.fireRequeued(reclaimed, eligible)
	// One lease expiry per reclaimed row, like the lease reaper: the attempt close below is a `reaped`
	// close, and every such close has exactly one matching LeaseExpired increment (SPEC-0034 REQ-14), so
	// skipping it here would break the reconciliation. The second counter is the one that says WHY this
	// expiry was not a worker losing a race: an interrupt reclaim means a wait lost its holder
	// (ADR-0046). Governing: SPEC-0023 REQ-3 "Lifecycle counters", ADR-0028.
	for i, t := range reclaimed {
		s.fireTodoHook(t.State, t)
		s.fireDeadLettered(t, DeadLetteredInterrupt, eligible[i]) // issue #588
		s.metricsOrNop().LeaseExpired(t.Queue)
		s.metricsOrNop().InterruptLeaseReclaimed(t.Queue)
		if closed[i] {
			s.countAttemptClosed(t.Queue, "reaped")
		}
	}
	return int64(len(reclaimed)), nil
}
