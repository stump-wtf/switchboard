-- 0031_interrupt_lease_deadline — when a todo entered an A2A interrupt state, so its wait can be aged.
--
-- InterruptTodo moves a claimed todo to 'input-required'/'auth-required' and deliberately RETAINS the
-- owner and the lease, so the same holder resumes when the client supplies the input or the auth
-- resolves (SPEC-0018 scenario "Interrupt states return to claimed"). Nothing was ever written down
-- about WHEN that wait started, and while the todo sits there the holder cannot heartbeat: the
-- heartbeat statement is guarded on state='claimed'. So the row's clock is silent — `lease_expires_at`
-- keeps ticking from the original claim, and `updated_at` is not the interrupt's start time the moment
-- any other path touches the row.
--
-- Without a start stamp there is no honest way to ask "has nobody come back for this one yet", which is
-- why SPEC-0018's design.md leaves the interrupt timeout as an open question and why an interrupted todo
-- whose holder died parks forever: invisible to the reaper (its predicate needs state='claimed') and
-- unclaimable (the claim predicate covers pending, lapsed claimed and due failed). ADR-0046 (proposed)
-- sets the policy; this column is the fact that makes any policy implementable.
--
-- Additive and nullable. The backfill stamps rows already parked in an interrupt state with their
-- `updated_at` — the closest available approximation of when they got there, and the conservative choice:
-- it can only make an already-stranded row due sooner, never hide one. A null means "not interrupted",
-- which is also what every row that has never been interrupted reads as, including pre-migration ones.
-- Rollback is ALTER TABLE todos DROP COLUMN interrupted_at; nothing else depends on it.
--
-- Governing: ADR-0046 (proposed), SPEC-0018 REQ "Task State Machine Extension", ADR-0002 (reaper),
-- SPEC-0006 REQ "Lease Lifecycle and Crash Safety".
--
-- @joestump-agent 10/19/2026 - Added for stump.wtf/switchboard#594.
ALTER TABLE todos ADD COLUMN interrupted_at timestamptz
    CONSTRAINT todos_interrupted_at_after_created CHECK (interrupted_at IS NULL OR interrupted_at >= created_at);

UPDATE todos SET interrupted_at = updated_at
 WHERE state IN ('input-required', 'auth-required') AND interrupted_at IS NULL;

-- The sweep's predicate: interrupt rows only, ordered by who has waited longest. Interrupt states are a
-- handful of rows on a table of terminal history, so a partial index keeps the tick an index probe
-- instead of a scan; it is keyed on the timestamp the sweep compares against, like idx_todos_pending is
-- keyed on created_at for the claim scan.
CREATE INDEX IF NOT EXISTS idx_todos_interrupted ON todos (interrupted_at)
    WHERE state IN ('input-required', 'auth-required');
