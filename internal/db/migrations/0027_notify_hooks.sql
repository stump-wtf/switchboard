-- Notify Hooks
--
-- A notify hook is an outbound HTTPS URL an endpoint registers for itself; when a push-eligible todo
-- it owns becomes ready, Switchboard POSTs a signed, payload-free notification there (SPEC-0024).
-- This migration only adds the table. Nothing reads it until the store, verbs and dispatcher land,
-- so it ships dark.
--
-- The table has no owner columns: a hook inherits its endpoint's owner scope through endpoint_id,
-- and the cascade takes a hook with its endpoint, so revoking and deleting an endpoint needs no
-- second cleanup (design.md "A table of its own, keyed to the endpoint").
--
-- secret and prev_secret hold the internal/cred envelope (enc:v1:...), never plaintext: the store
-- refuses to write a hook secret without SWITCHBOARD_SECRET_ENCRYPTION_KEY. prev_secret exists only
-- for the 24h dual-signing grace after a rotation (SPEC-0024 REQ-4).
--
-- Additive and forward-safe: a new table with no backfill. Rollback is DROP TABLE notify_hooks,
-- which loses only hook registrations; every todo stays pending and claimable.
--
-- Governing: ADR-0029, SPEC-0024 REQ-1 "Hook Ownership and Scope", REQ-4 "Signing", REQ-8 "Hook
-- Health and Auto-Disable", design.md "Schema".
--
-- @joestump 09/24/2026 - Added for #351.
CREATE TABLE notify_hooks (
    id                     uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    endpoint_id            uuid NOT NULL REFERENCES endpoints(id) ON DELETE CASCADE,
    url                    text NOT NULL CHECK (length(url) <= 2048),
    secret                 text NOT NULL,
    prev_secret            text,
    prev_secret_expires_at timestamptz,
    queues                 text[] NOT NULL DEFAULT '{}',
    ignore_presence        boolean NOT NULL DEFAULT false,
    enabled                boolean NOT NULL DEFAULT true,
    disabled_reason        text CHECK (disabled_reason IN ('consecutive_failures', 'operator')),
    disabled_at            timestamptz,
    consecutive_failures   integer NOT NULL DEFAULT 0,
    last_attempt_at        timestamptz,
    last_status            integer,
    last_error             text,
    created_at             timestamptz NOT NULL DEFAULT now(),
    rotated_at             timestamptz
);

-- Postgres does not index a foreign key's referencing column, so this is the only index on
-- endpoint_id. It is deliberately NOT partial: the dispatcher's fire-time lookup ("enabled hooks of
-- this endpoint") uses it with a cheap filter over at most the ceiling's rows, and the predicates
-- that name only endpoint_id (the verbs' list, CreateNotifyHook's ceiling count, and the ON DELETE
-- CASCADE when an endpoint is deleted) also need it; a WHERE enabled index would leave those as
-- sequential scans of the whole table.
CREATE INDEX idx_notify_hooks_endpoint ON notify_hooks (endpoint_id);
