-- Queue-stall nudge ledger: one row per (endpoint, queue) that has been woken without anyone claiming.
--
-- A queue drains only as fast as something wakes a consumer to claim from it. A consumer that
-- deliberately claims one todo per invocation re-rings its own next invocation, and the doorbell
-- heartbeat (SPEC-0011) re-rings a pending todo — but only for an endpoint with a session attached
-- to this instance. A one-shot consumer started by an outbound webhook has no session, so once its
-- one notification is spent (or lost: the dispatcher keeps nothing across a restart, SPEC-0024
-- design.md "A per-instance bounded queue, with no persistence"), the queue keeps real, claimable
-- work and nothing is left to prod it. #566.
--
-- The ledger is the loop guard: it is what makes a periodic nudge safe to run. last_nudged_at plus
-- the nudge count pace a queue that is not making progress, a claim resets the count, and a drained
-- queue's row is deleted by the sweep that reads it, so the next backlog starts at the short delay.
-- ADR-0045 (proposed) fixes the schedule; nothing here depends on the interval chosen.
--
-- Additive and forward-safe: a new table with no backfill, read by nothing until the sweep lands.
-- Rollback is DROP TABLE queue_stall_nudges, which loses only pacing state — every todo stays
-- pending and claimable, exactly as before.
--
-- Governing: ADR-0045 (proposed), ADR-0029, SPEC-0024 REQ-6, SPEC-0011 REQ "Doorbell Heartbeat".
--
-- @joestump-agent 10/15/2026 - Added for stump.wtf/switchboard#566.
CREATE TABLE IF NOT EXISTS queue_stall_nudges (
    endpoint_id    uuid NOT NULL REFERENCES endpoints(id) ON DELETE CASCADE,
    queue          text NOT NULL,
    -- Nudges since the queue last made progress (a claim). Reset to 1 by the sweep that finds a
    -- claim after the previous nudge; capped, so a consumer that never claims stops costing wakes.
    nudges         integer NOT NULL DEFAULT 1,
    last_nudged_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (endpoint_id, queue)
);

-- The sweep's drain check ("does this pair still have a pending row?") runs for every ledger row on
-- a tick. todos' partial pending index is keyed (endpoint_id, queue, created_at), so the anti-join
-- is an index probe per ledger row; the ledger itself is bounded by the number of queues with
-- pending work, so this table stays small without a second index.
