-- 0018_event_routing — deterministic event routing (ADR-0024, SPEC-0020).
--
-- Each self-managed webhook gains an ordered list of jq routing rules and an optional default action.
-- Every event a self-managed webhook records now names the webhook it arrived on (so a dry-run by
-- event id can be scoped to the webhook's owner) and how it was routed, and every todo carries the
-- same trace so it can explain why it exists. A dropped delivery is an event row with a drop trace
-- and no todo: its (source, external_id) dedup slot is spent exactly as an accepted one's is.
--
-- Additive, with zero values meaning "route to target_queue as before": an empty rule list and a
-- NULL default change nothing for an existing webhook. Rollback = reset routing_rules to '[]' and
-- default_action to NULL.
--
-- Governing: ADR-0024, SPEC-0020 REQ "Rule Validation at Save Time", REQ "Drop Action Semantics",
-- REQ "Routing Trace".
ALTER TABLE endpoint_webhooks
    ADD COLUMN routing_rules  jsonb NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN default_action jsonb;

ALTER TABLE events
    ADD COLUMN webhook_id    uuid REFERENCES endpoint_webhooks(id) ON DELETE SET NULL,
    ADD COLUMN routing_trace jsonb;

ALTER TABLE todos
    ADD COLUMN routing_trace jsonb;

-- Hot path for "this webhook's recent deliveries" (dry-run by event id, per-webhook history).
CREATE INDEX idx_events_webhook ON events (webhook_id, received_at DESC) WHERE webhook_id IS NOT NULL;
