-- 0012_endpoint_scoped_todos — pin every todo to its owning vended endpoint, and add the
-- deterministic webhook_routes table for token-free N-target fan-out. ADR-0022.
--
-- Pre-ADR-0022 the todos table had a free-form `queue` text column and no link to endpoints,
-- agents, or humans. Queue names were global, so any two endpoints sharing a queue string
-- (e.g. both scoped to "github") silently shared visibility into each other's work — a
-- cross-tenant leak on both the push path (channel doorbell) and the pull path (list_todos,
-- claim). This migration closes that hole at the data layer.
--
-- Per Joe (2026-07-21): existing todo/event data is disposable at this stage (single operator), so
-- the migration is a clean break, not a backward-compatible ALTER. Existing todos are DESTROYED,
-- not migrated — there is no backfill, because no surviving row can name an owner without
-- inventing one. Existing events are destroyed alongside them (an event is meaningless once the
-- todos it produced are gone).

-- Drop the old dedup index (keyed on global queue) and the pending scan index; both are rebuilt
-- below on the new endpoint-scoped key.
DROP INDEX IF EXISTS idx_todos_dedupe;
DROP INDEX IF EXISTS idx_todos_pending;

-- Truncate: this is the "nuke all data" path Joe approved. Existing rows have no endpoint_id
-- and cannot be backfilled without inventing an owner; a clean reset is simpler and safe at the
-- current (single-operator) stage.
--
-- todos and events MUST be truncated in a SINGLE statement. todos.event_id carries an FK to
-- events (0001_init.sql), and Postgres rejects truncating a table referenced by an FK unless
-- every referencing table is truncated in the same command:
--   ERROR: cannot truncate a table referenced in a foreign key constraint (SQLSTATE 0A000)
--
-- CASCADE, not an explicit table list. Naming todos and events alone was correct when this file
-- was written, but it silently assumed no OTHER table would ever reference them. Migrations apply
-- in lexical filename order, and 0012_a2a_push_notification_configs.sql (push_notification_configs
-- .task_id → todos.id) sorts BEFORE this file — so on a FRESH database that FK exists by the time
-- this statement runs and raises exactly the error above. Every already-migrated deployment stayed
-- green throughout, because schema_migrations records this version as applied and never re-runs
-- it; only new installs broke, which is why CI (no Postgres on the primary gate) never saw it.
-- CASCADE pulls in every referencing table automatically, so the next table to reference todos
-- cannot re-break it. Truncating those dependents is correct by construction: this migration
-- deliberately destroys all todo/event data, and a row referencing a destroyed todo cannot outlive
-- it (push_notification_configs.task_id is itself ON DELETE CASCADE for the same reason).
TRUNCATE todos, events CASCADE;

-- Pin every todo to exactly one vended MCP endpoint for its entire lifecycle. The endpoint's
-- owning human (endpoints → agents → humans) is the todo's tenant. ON DELETE CASCADE mirrors the
-- existing endpoint_webhooks cascade: deleting an endpoint removes its work. ADR-0022.
--
-- The column is NOT NULL, with no sentinel and no nullable escape hatch: ownership is the tenant
-- boundary, and a todo that belongs to no endpoint would sit outside it. There is no "system
-- todo" exception. Friend approvals — the one case that previously motivated a nullable column —
-- no longer live in this table at all: a friend request is already durable in `friend_edges`, and
-- the Board renders pending approvals directly from that edge rather than from a todo. Delegating
-- an approval to an agent later mints a normal endpoint-owned todo like any other work.
--
-- Adding a NOT NULL column without a default is safe here only because the TRUNCATE above leaves
-- the table empty; there is no backfill because there is no surviving data.
-- Governing: ADR-0022, SPEC-0003 REQ "Endpoint Ownership (Tenant Isolation)".
ALTER TABLE todos ADD COLUMN endpoint_id uuid NOT NULL REFERENCES endpoints(id) ON DELETE CASCADE;

-- The dedup namespace moves from global (queue, idempotency_key) to per-endpoint
-- (endpoint_id, idempotency_key). Two endpoints that happen to share an idempotency key (e.g.
-- the same GitHub delivery id routed to two endpoints) each retain their own todo without
-- collapsing onto each other; within one endpoint, a redelivery during a backoff window still
-- collapses onto the parked retry. Governing: SPEC-0003 REQ "Per-Endpoint Idempotency and Dedup".
--
-- Uniqueness is genuinely enforced across both columns: endpoint_id is NOT NULL, and the partial
-- predicate admits only rows with a non-null idempotency_key. No indexed row carries a NULL in
-- either column, so Postgres's default NULLS DISTINCT behaviour (which would otherwise let
-- duplicate pairs slip past a unique index whenever a column is NULL) cannot apply here.
-- Opting out of dedup still works: CreateTodo writes NULLIF(key,''), so an empty idempotency key
-- becomes NULL and the row falls outside the partial index entirely.
CREATE UNIQUE INDEX idx_todos_dedupe ON todos (endpoint_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL AND state <> 'done'
        AND (state <> 'failed' OR next_retry_at IS NOT NULL);

-- Rebuild the pending scan index on the endpoint-scoped key so the claim scan stays served by
-- the partial index and never scans terminal rows (SPEC-0003 REQ "Database Operation Standards").
CREATE INDEX idx_todos_pending ON todos (endpoint_id, queue, created_at) WHERE state = 'pending';

-- A secondary lookup index for the endpoint-scoped list_todos path (filter by endpoint, optional
-- queue, optional state; newest first). `queue` is deliberately NOT a leading column: ListTodos
-- matches `queue = ANY($2)` over the session's granted queues, so leading with queue would force a
-- sort to recover `created_at DESC` ordering across the matched values. Leading with
-- (endpoint_id, created_at DESC) lets the LIMIT-ed newest-first scan walk the index in order and
-- apply queue/state as filters. Named for the columns it actually indexes.
CREATE INDEX idx_todos_endpoint_created ON todos (endpoint_id, created_at DESC);

-- webhook_routes maps a webhook to N target endpoints for deterministic, token-free delivery
-- fan-out. At ingest time the self-managed receiver resolves the webhook's target endpoints and
-- creates one todo per target, each pinned to that target endpoint. When no rows exist for a
-- webhook the target set is the singleton {webhook.endpoint_id}. Routes are populated by
-- human-approved actions (friending, a future routing verb) — never by a per-delivery agent
-- decision. Governing: ADR-0022, SPEC-0001 REQ "Deterministic Route Fan-Out (Token-Free)".
CREATE TABLE webhook_routes (
    webhook_id          uuid NOT NULL REFERENCES endpoint_webhooks(id) ON DELETE CASCADE,
    target_endpoint_id  uuid NOT NULL REFERENCES endpoints(id) ON DELETE CASCADE,
    granted_by_human_id uuid NOT NULL REFERENCES humans(id) ON DELETE CASCADE,
    granted_at          timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (webhook_id, target_endpoint_id)
);
-- Hot path: resolve a webhook's target endpoints at delivery time.
CREATE INDEX idx_webhook_routes_webhook ON webhook_routes (webhook_id);
