-- Per-queue pause: one row per queue an operator has quiesced.
--
-- An operator draining a downstream consumer (upgrade, restart, incident) has no way to say
-- "stop handing out work from this queue for now" without disabling the whole endpoint (which
-- drops or fails inbound webhooks) or unbinding the consumer (which still leaves channel-
-- triggered runs firing). Disabling is the wrong lever: intake and delivery are separate
-- decisions, and only the queue is authoritative about both. #581.
--
-- A paused queue keeps accepting inbound webhooks and persisting them as pending todos —
-- nothing is lost, dedup still applies — while every delivery path (claim, claim_next, the
-- channel doorbell, the ready-hook wake, the stall nudge) treats it as empty. Resume deletes
-- the row and re-rings the backlog through the same wake chain an enqueue uses.
--
-- Queues are a global namespace (SPEC-0003), so the key is the queue name alone: pausing a
-- queue pauses every endpoint that drains it, which is exactly what quiescing a shared
-- consumer means. The board and metrics read this table to say why nothing is moving.
--
-- Additive and forward-safe: a new table read by nothing until the pause verbs land; an
-- unpaused fleet behaves exactly as before. Rollback is DROP TABLE queue_pause, which loses
-- only pause state — every todo stays pending and claimable, exactly as before.
--
-- Governing: ADR-0047 (proposed), ADR-0007, SPEC-0003 REQ "Endpoint Ownership".
--
-- @joestump-agent 10/20/2026 - Added for stump.wtf/switchboard#581.
CREATE TABLE IF NOT EXISTS queue_pause (
    queue     text PRIMARY KEY,
    -- The operator human who paused the queue, for the board and audit trail; NULL only if a
    -- future surface pauses without an authenticated human.
    paused_by uuid REFERENCES humans(id) ON DELETE SET NULL,
    reason    text NOT NULL DEFAULT '',
    paused_at timestamptz NOT NULL DEFAULT now()
);
