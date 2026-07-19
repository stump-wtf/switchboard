-- Provider registry (ADR-0020; SPEC-0017 REQ "Runtime Provider Registry"): the adapters table
-- grows into the registry BOTH ingestion families resolve at request/poll time — webhook providers
-- join the pull adapters already living here (ADR-0014), rather than a second model the Providers
-- view would have to merge forever.
--
--   kind   — the concrete provider implementation the row configures (github|stripe|slack|generic|
--            redis|…). family says which half of the system serves it; kind says which code path.
--            Default '' keeps pre-registry rows (and runner-registered pull adapters) valid.
--   secret — the provider's held secret (shared-secret token or HMAC signing secret), stored
--            encrypted via the internal/cred envelope (enc:v1: ciphertext) when an encryption key
--            is configured — never plaintext by policy, and never selected by list surfaces.
--            NULL for providers that need none (open, queue — the broker DSN stays in env).
ALTER TABLE adapters
    ADD COLUMN kind   text NOT NULL DEFAULT '',
    ADD COLUMN secret text;
