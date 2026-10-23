-- 0034_endpoint_scope_widenings — the audit trail of an endpoint's additive scope changes.
--
-- An endpoint's scope is fixed when it is vended (ADR-0008), and it stays fixed for everything that
-- could shrink it: narrowing a grant or replacing a verb list can break a route that works today, so
-- that stays a revoke and a re-vend. What has no path at all is the change that cannot break anything:
-- ADDING a queue to an endpoint's allowed webhook queues, an ingest source type, a verb, or raising the
-- webhook ceiling. Today adding one lane forces a full re-vend — new endpoint, new webhook, new ingest
-- URL and signing secret, and an edit in the source system's hook configuration — because nothing else
-- can say "this endpoint may also deliver to Q".
--
-- ADR-0049 (proposed) lets the operator who already controls the endpoint widen it additively. A widen
-- changes no credential: the secret shown once at vend stays valid. It is also the first change to an
-- endpoint's grant that happens after the vend, so it needs a record of its own: which field moved,
-- what it held before, what it holds now, who did it and when. The endpoint row shows the current
-- scope and nothing about how it got there; revoked endpoints already keep their row as the revoke
-- audit record (ADR-0038), and this table is the same idea for the scope itself.
--
-- Additive-only, so `before` is always a subset of `after` for the array fields, and a webhook_max
-- record's before is never larger than its after. That is the store's invariant, not a database
-- constraint: computing the new value as a set union is what makes removal structurally impossible,
-- and the Go side refuses the one field (webhook_max) where an absolute value could express a
-- reduction. before/after are JSONB rather than text[] because the same table records an array field
-- and a scalar one.
--
-- Purely additive: no backfill, nothing existing reads it. Cascade on delete matches DeleteEndpoint,
-- which already drops the endpoint row: an operator who needs the trail keeps the endpoint.
--
-- Governing: ADR-0049 (proposed), ADR-0008 (scope is fixed at vend), ADR-0012 (the webhook ceiling),
-- SPEC-0006 REQ "Webhook Self-Management Within a Vended Ceiling".
--
-- @joestump-agent 10/21/2026 - Added for stump.wtf/switchboard#590.
CREATE TABLE endpoint_scope_widenings (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    endpoint_id    uuid        NOT NULL REFERENCES endpoints(id) ON DELETE CASCADE,
    -- The operator who widened, recorded by id: a human row is never deleted, so this never dangles,
    -- and an operator-facing history must be able to say who acted without joining anything secret.
    actor_human_id uuid        NOT NULL REFERENCES humans(id),
    field          text        NOT NULL CHECK (field IN ('webhook_queues', 'webhook_source_types',
                                                         'webhook_max', 'scope_verbs')),
    before         jsonb       NOT NULL,
    after          jsonb       NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now()
);

-- The history of one endpoint, oldest first, which is the only way it is ever read.
CREATE INDEX endpoint_scope_widenings_endpoint_idx
    ON endpoint_scope_widenings (endpoint_id, id);
