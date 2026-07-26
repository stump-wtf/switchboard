-- Backfill webhook ceiling defaults on existing endpoints so self-managed webhooks work after
-- upgrading. Previously the vend INSERT never wrote webhook_max / webhook_source_types /
-- webhook_queues, leaving every endpoint at the column defaults (max=0, empty arrays). This
-- migration gives existing endpoints a permissive ceiling that mirrors their scope queues, so
-- agents that were vended before the fix get the same capability a fresh vend would grant when the
-- operator sets webhook_max > 0. Operators who want stricter ceilings should re-vend with the new
-- wizard step. Governing: ADR-0012, SPEC-0006 REQ "Webhook Self-Management Within a Vended Ceiling".

-- Set webhook_max=5 (a generous default) and copy scope_queues into webhook_queues for every
-- endpoint that still has the column defaults (webhook_max=0 AND empty webhook_queues). Endpoints
-- that were already vended with explicit ceiling values (none today, but future-proof) are left
-- alone.
UPDATE endpoints
   SET webhook_max         = 5,
       webhook_queues       = scope_queues,
       webhook_source_types = ARRAY['github', 'generic']
 WHERE webhook_max = 0
   AND webhook_queues = '{}';
