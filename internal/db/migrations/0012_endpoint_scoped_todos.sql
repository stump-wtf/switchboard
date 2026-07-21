-- 0012_endpoint_scoped_todos — pin every todo to its owning vended endpoint, and add the
-- deterministic webhook_routes table for token-free N-target fan-out. ADR-0021.
--
-- Pre-ADR-0021 the todos table had a free-form `queue` text column and no link to endpoints,
-- agents, or humans. Queue names were global, so any two endpoints sharing a queue string
-- (e.g. both scoped to "github") silently shared visibility into each other's work — a
-- cross-tenant leak on both the push path (channel doorbell) and the pull path (list_todos,
-- claim). This migration closes that hole at the data layer.
--
-- Per Joe (2026-07-21): production data is disposable at this stage (single operator), so the
-- migration is a clean break, not a backward-compatible ALTER. Existing todos are dropped and
-- re-created with a non-null endpoint_id; existing events are dropped alongside (they are
-- meaningless without their todos and there is no production data to preserve).

-- Drop the old dedup index (keyed on global queue) and the pending scan index; both are rebuilt
-- below on the new endpoint-scoped key.
DROP INDEX IF EXISTS idx_todos_dedupe;
DROP INDEX IF EXISTS idx_todos_pending;

-- Truncate: this is the "nuke all data" path Joe approved. Existing rows have no endpoint_id
-- and cannot be backfilled without inventing an owner; a clean reset is simpler and safe at the
-- current (single-operator) stage.
TRUNCATE TABLE todos;
TRUNCATE TABLE events;

-- Pin every todo to exactly one vended MCP endpoint for its entire lifecycle. The endpoint's
-- owning human (endpoints → agents → humans) is the todo's tenant. ON DELETE CASCADE mirrors the
-- existing endpoint_webhooks cascade: deleting an endpoint removes its work. ADR-0021.
--
-- The column is NULLable for system-level todos that predate any endpoint — primarily
-- friend-approval todos, which target a human (not an endpoint) and are drained from the Board
-- UI. The agent-facing query path (ListTodos, ClaimTodo, GetTodo, etc.) ALWAYS predicates on a
-- non-null endpoint_id, so a NULL-endpoint todo is invisible to every agent regardless of queue.
-- Governing: ADR-0021 REQ "Endpoint Ownership" (the tenant boundary is the endpoint; system
-- todos live outside it and are operator-only).
ALTER TABLE todos ADD COLUMN endpoint_id uuid REFERENCES endpoints(id) ON DELETE CASCADE;

-- The dedup namespace moves from global (queue, idempotency_key) to per-endpoint
-- (endpoint_id, idempotency_key). Two endpoints that happen to share an idempotency key (e.g.
-- the same GitHub delivery id routed to two endpoints) each retain their own todo without
-- collapsing onto each other; within one endpoint, a redelivery during a backoff window still
-- collapses onto the parked retry. Governing: SPEC-0003 REQ "Per-Endpoint Idempotency and Dedup".
CREATE UNIQUE INDEX idx_todos_dedupe ON todos (endpoint_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL AND state <> 'done'
        AND (state <> 'failed' OR next_retry_at IS NOT NULL);

-- Rebuild the pending scan index on the endpoint-scoped key so the claim scan stays served by
-- the partial index and never scans terminal rows (SPEC-0003 REQ "Database Operation Standards").
CREATE INDEX idx_todos_pending ON todos (endpoint_id, queue, created_at) WHERE state = 'pending';

-- A secondary lookup index for the endpoint-scoped list_todos path (filter by endpoint, optional
-- queue, optional state; newest first).
CREATE INDEX idx_todos_endpoint_queue ON todos (endpoint_id, created_at DESC);

-- webhook_routes maps a webhook to N target endpoints for deterministic, token-free delivery
-- fan-out. At ingest time the self-managed receiver resolves the webhook's target endpoints and
-- creates one todo per target, each pinned to that target endpoint. When no rows exist for a
-- webhook the target set is the singleton {webhook.endpoint_id}. Routes are populated by
-- human-approved actions (friending, a future routing verb) — never by a per-delivery agent
-- decision. Governing: ADR-0021, SPEC-0001 REQ "Deterministic Route Fan-Out (Token-Free)".
CREATE TABLE webhook_routes (
    webhook_id          uuid NOT NULL REFERENCES endpoint_webhooks(id) ON DELETE CASCADE,
    target_endpoint_id  uuid NOT NULL REFERENCES endpoints(id) ON DELETE CASCADE,
    granted_by_human_id uuid NOT NULL REFERENCES humans(id) ON DELETE CASCADE,
    granted_at          timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (webhook_id, target_endpoint_id)
);
-- Hot path: resolve a webhook's target endpoints at delivery time.
CREATE INDEX idx_webhook_routes_webhook ON webhook_routes (webhook_id);
