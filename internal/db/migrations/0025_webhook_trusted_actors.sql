-- 0023_webhook_trusted_actors — first-class trusted actors on self-managed webhooks (ADR-0031,
-- SPEC-0026 REQ-5).
--
-- "Who may start work here" becomes a column on the webhook instead of a jq allowlist inside the
-- rules. The receiver evaluates it in Go, from the verified body, before any rule runs. It is
-- required on every source with an actor projection (github, gitea, cairn) and NULL elsewhere.
-- Canonical shapes:
--
--   {"allow_all": true}                                  trust every verified sender (flagged)
--   {"logins": ["…"], "match": "sender|author|both"}     github, gitea
--   {"actor_ids": ["…"]}                                 cairn
--
-- Forward-safe against live data. Every EXISTING github, gitea and cairn webhook is backfilled
-- with {"allow_all": true}, so it keeps routing exactly as before, visibly flagged, until its
-- owner sets a list. New webhooks get an empty list, which trusts no one: the application writes
-- it at create time. No code path treats a missing value as "gate off". The CHECK makes a missing
-- value impossible on those sources from here on, and a value on any other source is refused by
-- the application (and would be ignored by the gate anyway).
--
-- The backfill UPDATE runs before the constraint, in the same transaction, so the constraint is
-- validated against backfilled rows. Rollback: drop the constraint and the column. Nothing else
-- reads them.
--
-- Governing: ADR-0031, SPEC-0026 REQ-5 "Trusted Actors" (scenario "Existing webhook after the
-- upgrade"), design.md "trusted_actors is a column, evaluated before rules".
ALTER TABLE endpoint_webhooks ADD COLUMN trusted_actors jsonb;

UPDATE endpoint_webhooks SET trusted_actors = '{"allow_all": true}'::jsonb
 WHERE source_type IN ('github', 'gitea', 'cairn');

ALTER TABLE endpoint_webhooks ADD CONSTRAINT trusted_actors_required
    CHECK (source_type NOT IN ('github', 'gitea', 'cairn') OR trusted_actors IS NOT NULL);
