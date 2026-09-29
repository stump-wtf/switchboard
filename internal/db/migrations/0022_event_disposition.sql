-- 0022_event_disposition — every event records the outcome of intake (ADR-0031, SPEC-0026).
--
-- Routing now fails closed (#212). A rule fault stops evaluation, and the delivery is recorded with
-- no todo instead of falling through to the next rule or the default. That outcome must be visible
-- and filterable, not something a reader has to reconstruct from the trace. So each event names its
-- disposition:
--
--   routed       the delivery became todos (or collapsed onto existing ones)
--   dropped      a drop action recorded it without a todo
--   faulted      a rule fault stopped evaluation; recorded without a todo
--   quarantined  held on the quarantine queue (SPEC-0026 REQ-6; used once #386 lands)
--
-- All four values are allowed now, so the quarantine story needs no further change to this column.
--
-- Additive and forward-safe. The constant default makes ADD COLUMN a metadata-only change, with no
-- table rewrite. Every existing event was routed except the ones a drop rule recorded, and the drop
-- is in the stored trace, so the backfill is exact. No existing event is faulted, because before
-- this change a fault was treated as no-match and the delivery was routed. Rollback: drop the column
-- and the index. Nothing else reads them.
--
-- The partial index serves the per-webhook fault reads: the board card's 24-hour warning and the
-- operator's 7-day fault report. Faulted rows are the rare case, so the index stays small.
--
-- Governing: ADR-0031, SPEC-0026 REQ-1 "Faults Stop Evaluation" (disposition, list_webhook_events
-- filter, board warning).
ALTER TABLE events
    ADD COLUMN disposition text NOT NULL DEFAULT 'routed'
        CONSTRAINT events_disposition_check
        CHECK (disposition IN ('routed', 'dropped', 'quarantined', 'faulted'));

UPDATE events SET disposition = 'dropped'
 WHERE routing_trace IS NOT NULL
   AND COALESCE((routing_trace->'action'->>'drop')::boolean, false);

CREATE INDEX idx_events_faulted ON events (webhook_id, received_at DESC)
    WHERE disposition = 'faulted' AND webhook_id IS NOT NULL;
