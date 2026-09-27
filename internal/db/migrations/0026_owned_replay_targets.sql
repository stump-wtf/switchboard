-- Owned Replay Targets
--
-- replay_webhook_event used to fall back to the instance setting replay_default_target and treated
-- it, and every host in replay_allowed_targets, as "trusted": those targets skipped the SSRF guard
-- for every tenant's replay (audit F9). Replay destinations now belong to the endpoint that
-- replays (SPEC-0033 REQ "Owned Replay Targets", ADR-0038): endpoints.replay_targets is fixed at
-- vend time like the rest of the endpoint's scope, its first entry is the default when a replay
-- names no target, and every target, owned or not, passes the shared SSRF validator at call time
-- and again at dial time.
--
-- The two settings rows are deleted outright. Nothing reads them afterwards and there is no boot
-- warning; the CHANGELOG's Breaking entry and upgrade note name both. Existing endpoints start with
-- no owned targets, so a replay without target_url fails with replay_target_required until the
-- endpoint is re-vended with one.
--
-- NOT REVERSIBLE in place: the settings rows are deleted, not archived. The column add is
-- metadata-only (a constant default), so it takes no table rewrite on live data.
--
-- @joestump 09/25/2026 - Added for #421.
ALTER TABLE endpoints ADD COLUMN IF NOT EXISTS replay_targets text[] NOT NULL DEFAULT '{}';

DELETE FROM settings WHERE key IN ('replay_default_target', 'replay_allowed_targets');
