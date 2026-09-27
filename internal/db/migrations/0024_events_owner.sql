-- 0024_events_owner — every event carries its owner (SPEC-0033 REQ "Owner-Scoped History Reads",
-- REQ "Closing the Audited Surfaces" F1 and F14; ADR-0038; closes #194).
--
-- events.endpoint_id is the endpoint that owns a delivery: the resolved webhook's endpoint for a
-- self-managed delivery, the target endpoint for an operator push. It is written once at ingest and,
-- unlike webhook_id (ON DELETE SET NULL, 0018), survives the webhook being deleted, so a history
-- read never has to infer ownership through a row that may be gone. The history reads filter on it;
-- an event whose owner cannot be established stays NULL, is invisible to every agent, and is left to
-- retention. events.team_id and the one-owner CHECK arrive with team queues (#415), not here.
--
-- Backfill, in two parts:
--   1. through webhook_id → endpoint_webhooks.endpoint_id, as the design record specifies;
--   2. operator pushes (family 'operator', never a webhook) from the single todo they minted, whose
--      endpoint is also the prefix of their external_id ("<endpoint-id>:<key>", server/api.go). Both
--      must agree, so a row whose todo was re-pointed or deleted stays unowned rather than guessed.
-- Everything else with a NULL webhook_id (deliveries whose webhook was deleted before this
-- migration) has no provable owner and stays invisible.
--
-- F14: event dedup was unique on (source, external_id) across the whole instance, so one tenant's
-- delivery could be answered with another's event. The owner joins the key. NULLS NOT DISTINCT keeps
-- owner-less rows deduplicating among themselves exactly as before. Widening a unique key cannot
-- invalidate existing rows, so the swap is safe against live data.
--
-- Rollback: drop idx_events_owner, recreate idx_events_dedupe on (source, external_id), drop the column.
ALTER TABLE events ADD COLUMN endpoint_id uuid REFERENCES endpoints(id) ON DELETE CASCADE;

UPDATE events e
   SET endpoint_id = w.endpoint_id
  FROM endpoint_webhooks w
 WHERE e.webhook_id = w.id
   AND e.endpoint_id IS NULL;

UPDATE events e
   SET endpoint_id = t.endpoint_id
  FROM todos t
 WHERE t.event_id = e.id
   AND e.endpoint_id IS NULL
   AND e.webhook_id IS NULL
   AND e.family = 'operator'
   AND e.external_id LIKE t.endpoint_id::text || ':%';

DROP INDEX idx_events_dedupe;
CREATE UNIQUE INDEX idx_events_dedupe ON events (endpoint_id, source, external_id) NULLS NOT DISTINCT
    WHERE external_id IS NOT NULL;

-- The history reads: one owner's events, newest first, under the (received_at, id) keyset.
CREATE INDEX idx_events_owner ON events (endpoint_id, received_at DESC, id DESC) WHERE endpoint_id IS NOT NULL;
