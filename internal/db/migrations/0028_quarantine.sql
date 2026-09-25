-- 0028_quarantine — the reserved quarantine queue (ADR-0031, SPEC-0026 REQ-6, REQ-7).
--
-- A delivery the trust gate holds, one whose rules fault, or one a rule sends to quarantine
-- becomes exactly one todo on the receiving webhook's OWNER endpoint, with queue = 'quarantine'.
-- The store's default filter keeps that todo away from every agent path: no list, get, claim or
-- doorbell. It leaves quarantine only by release (routed again through the owner's rules), discard,
-- or expiry after 30 days.
--
--   quarantine_reason   untrusted_actor | rule_fault | rule_action (required on the quarantine queue,
--                       kept after release as history)
--   quarantine_detail   the actor verdict or the fault, as structured JSON
--   released_by         human:<id> | classifier:<slug>, set when released
--   released_at         when it was released
--
-- 'quarantine' becomes a reserved queue name. If any existing todo, endpoint scope or ceiling, or
-- webhook target already uses it, this migration ABORTS and names the count, rather than silently
-- hiding that work behind the new filter. Rename the queue and restart.
--
-- Additive otherwise: nullable columns, a CHECK that can only bite quarantine rows (there are none
-- yet, per the guard), and a partial index for the owner's quarantine list and the expiry sweep.
-- Rollback: drop the index, the constraint and the four columns.
--
-- Governing: ADR-0031, SPEC-0026 REQ-6 "Quarantine", REQ-7 "Release and Discard", REQ-13; design.md
-- "Quarantine is a reserved queue on the owner endpoint" (pre-migration check).
DO $$
DECLARE
    n_todos     int;
    n_endpoints int;
    n_webhooks  int;
BEGIN
    SELECT count(*) INTO n_todos FROM todos WHERE queue = 'quarantine';
    SELECT count(*) INTO n_endpoints FROM endpoints
     WHERE 'quarantine' = ANY(scope_queues) OR 'quarantine' = ANY(webhook_queues);
    SELECT count(*) INTO n_webhooks FROM endpoint_webhooks WHERE target_queue = 'quarantine';
    IF n_todos + n_endpoints + n_webhooks > 0 THEN
        RAISE EXCEPTION 'migration 0028_quarantine: the queue name "quarantine" is now reserved, but it is in use by % todo(s), % endpoint scope(s) or ceiling(s), and % webhook target(s). Rename that queue, then restart to apply this migration.',
            n_todos, n_endpoints, n_webhooks;
    END IF;
END
$$;

ALTER TABLE todos
    ADD COLUMN quarantine_reason text
        CONSTRAINT todos_quarantine_reason_check
        CHECK (quarantine_reason IN ('untrusted_actor', 'rule_fault', 'rule_action')),
    ADD COLUMN quarantine_detail jsonb,
    ADD COLUMN released_by       text,
    ADD COLUMN released_at       timestamptz;

ALTER TABLE todos ADD CONSTRAINT todos_quarantine_reason_required
    CHECK (queue <> 'quarantine' OR quarantine_reason IS NOT NULL);

CREATE INDEX idx_todos_quarantine ON todos (endpoint_id, created_at) WHERE queue = 'quarantine';
