-- 0035_endpoint_provider_connection — where a webhook's provider actually lives, and the read
-- credential that lets this server look there (stump.wtf/switchboard#614, part C of the #587 split).
--
-- A reconcile pass (#616) has to answer "what does the provider hold for this webhook, updated since
-- the last event we recorded?". internal/provider (#619) grew the read clients for that, and they
-- take three things this server had nowhere to keep: which provider installation to dial, which
-- repository to list, and which token authenticates the read. Every webhook row carried the HMAC
-- signing secret and nothing else, so the read clients could only ever be constructed by a caller who
-- invented those three values somewhere outside the database. This migration moves them onto the row.
--
-- `provider_base_url` is an origin (scheme://host[:port]) that the write path validates and, when the
-- operator set SWITCHBOARD_PROVIDER_READ_ALLOWED_ORIGINS, an instance-wide allowlist confines. It is
-- not a full URL: an address with a path is a different API surface, and a value carrying userinfo or
-- a query is one where a credential could hide.
--
-- `provider_repo` is "owner/name" — the two halves provider.Reader takes, kept as one column because
-- they are only ever written and read together, and split at the boundary that builds the Repo.
--
-- `provider_read_token` is the read-only credential itself, stored exactly like a held signing secret
-- is: the `enc:v1:` envelope from internal/cred when SWITCHBOARD_SECRET_ENCRYPTION_KEY is set, so a
-- database-only compromise yields ciphertext. Reads never return it; the reconcile pass opens it
-- through one accessor. A write of a non-empty token with no at-rest key configured is refused rather
-- than stored in the clear (same doctrine as SPEC-0032 REQ-3 for a supplied signing secret).
--
-- `provider_connection_set_at` is when the address or the token was last written, so a listing can
-- say "configured 3 days ago" without holding anything secret.
--
-- A row with no address is the common state, not a broken one: most webhooks will never be reconciled,
-- and nothing dials a provider on their behalf. The address and the repo are constrained to travel
-- together because half an address is not a configuration: it is a reconcile pass that cannot run and
-- an operator who cannot tell why. A token without an address is allowed on purpose — provisioning
-- (#615) and addressing do not happen in one sitting — and the derived state says which half is
-- missing: an addressed webhook with no token is `awaiting_credential`, exactly the way
-- `awaiting_secret` works for a provider-origin signing secret (SPEC-0032 REQ-2).
--
-- Nothing here revives instance-wide pull ingestion: the registry migration 0021 removed was a global
-- list of provider instances, and these columns are per-webhook read addresses owned by the webhook's
-- human (ADR-0022: work is endpoint-scoped). Whether a synthesized delivery may re-drive intake is a
-- trust question this migration does not answer; ADR-0052 proposes it.
--
-- Rollback: drop the four columns and the CHECK. No row is rewritten, no data is derived from another
-- table, and minted-signing-secret behavior is untouched.
--
-- Governing: stump.wtf/switchboard#614 (the connection config), #613 (the read clients), #616 (the
-- reconcile pass that will consume this); ADR-0002 (PostgreSQL is the trust boundary), ADR-0037 (the
-- envelope), ADR-0022 (endpoint-scoped ownership); ADR-0052 (proposed: what the read may be used for).
--
-- @joestump-agent 10/25/2026 - Added for stump.wtf/switchboard#614.

ALTER TABLE endpoint_webhooks
    ADD COLUMN provider_base_url text,          -- provider origin, validated on write
    ADD COLUMN provider_repo text,              -- "owner/name" on that provider
    ADD COLUMN provider_read_token text,        -- enc:v1 envelope; never selected into a response
    ADD COLUMN provider_connection_set_at timestamptz;

-- An address without a repository is a configuration that cannot be used, so it cannot be stored.
ALTER TABLE endpoint_webhooks
    ADD CONSTRAINT provider_connection_address CHECK ((provider_base_url IS NULL) = (provider_repo IS NULL));
