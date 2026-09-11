-- 0019_handoff_lanes — work orders and difficulty lanes on top of event routing (ADR-0025).
--
-- routing_params holds the owner-set values rules read as $params (trusted-actor allowlists and the
-- like), next to routing_rules and changed only through the same owner-gated verbs. todos.work_order
-- is the switchboard-authored work order a routed todo carries. routing_once records which
-- (webhook, subject, queue) keys have already produced a work order, so a relabel or a second
-- delivery about the same issue or artifact never mints a second one.
--
-- routing_once.event_id is deliberately not a foreign key: event retention deletes old events, and a
-- claimed key must outlive the event that claimed it or the work order could be minted again.
--
-- Additive; every new column is nullable and nothing reads routing_once unless a rule asks for once.
--
-- Governing: ADR-0025, SPEC-0020 REQ "At-Most-Once Work Orders", REQ "Work Orders".
ALTER TABLE endpoint_webhooks
    ADD COLUMN routing_params jsonb;

ALTER TABLE todos
    ADD COLUMN work_order jsonb;

CREATE TABLE routing_once (
    webhook_id uuid        NOT NULL REFERENCES endpoint_webhooks(id) ON DELETE CASCADE,
    once_key   text        NOT NULL,
    event_id   bigint,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (webhook_id, once_key)
);
