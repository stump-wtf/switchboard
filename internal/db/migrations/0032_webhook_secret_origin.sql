-- 0032_webhook_secret_origin — how each webhook's signing secret originates, and the trail of the
-- writes that set one (SPEC-0032 REQ-1, REQ-2, REQ-13; ADR-0037; design.md "Schema").
--
-- Until now every signed webhook minted its own HMAC secret, which is wrong for a producer that
-- signs with a secret IT chose: Stripe shows a `whsec_…` value, and a webhook whose stored secret is
-- a value Stripe has never heard of fails every delivery with 401 while looking healthy. `secret_origin`
-- says, per row, who issues the secret — `minted` (switchboard generates and reveals it once, as
-- today), `provider` (the producer signs, so someone must paste its secret in), or `none` (the source
-- needs no secret). `awaiting_secret` is not a column: it is a provider-origin signed webhook whose
-- secret is still NULL (design.md "`awaiting_secret` is derived, not a new column").
--
-- `secret_supplied` marks the one case the spec allows an operator to opt into:
-- SWITCHBOARD_WEBHOOK_ALLOW_SUPPLIED_SECRET=true lets a minted-origin webhook take a caller-supplied
-- secret (REQ-1 scenario "Operator allows supplied secrets for forges"). It cannot be derived from
-- the row, because a minted webhook holds a secret either way, and `rotate_webhook` mints a fresh one
-- and clears it.
--
-- `webhook_secret_audit` is the answer to "who rolled our Stripe secret": one append-only row per
-- successful write of a caller-supplied secret (and, when a later story lands, per overlap expiry),
-- written inside the same transaction as the write so a refused or rolled-back write leaves no row.
-- It never holds the secret, a part of it, or the request body — only the 12-hex fingerprint.
--
-- Forward-safe, and one thing is NOT: existing `stripe` and `slack` rows carry a minted secret that
-- can never verify a provider delivery. They are marked `provider` and their unusable secret is
-- NULLed, which moves them to `awaiting_secret` — an honest 503 that asks the producer's secret in,
-- instead of a silent 401 on every delivery. Every other row keeps its behavior. Rollback: drop the
-- audit table and the new columns; minted-origin behavior is unchanged, and provider-origin webhooks
-- refuse deliveries as they effectively do today.
--
-- Governing: SPEC-0032 REQ-1, REQ-2, REQ-13; design.md "Schema", "Migration Plan"; ADR-0037.
--
-- @joestump-agent 10/21/2026 - Added for stump.wtf/switchboard#391.
ALTER TABLE endpoint_webhooks
    ADD COLUMN secret_origin text NOT NULL DEFAULT 'minted',  -- minted|provider|none
    ADD COLUMN secret_set_at timestamptz,                     -- when the current secret was written
    ADD COLUMN secret_supplied boolean NOT NULL DEFAULT false, -- opt-in supplied secret on a minted kind
    ADD COLUMN previous_signing_secret text,                  -- enc:v1 only; the overlap secret (SPEC-0032 REQ-4)
    ADD COLUMN previous_secret_expires_at timestamptz,        -- when that overlap ends (REQ-4)
    ADD COLUMN handshake_at timestamptz;                      -- Slack's url_verified moment (REQ-9)

-- Stripe and Slack sign with their own secret: a minted value there is dead weight that makes every
-- delivery fail. Mark them provider and drop the unusable secret, which puts them in awaiting_secret.
UPDATE endpoint_webhooks SET secret_origin = 'provider' WHERE source_type IN ('stripe', 'slack');
UPDATE endpoint_webhooks SET signing_secret = NULL
 WHERE source_type IN ('stripe', 'slack') AND signing_secret IS NOT NULL;

-- A webhook verified by its unguessable ingest URL rather than an HMAC holds no secret at all.
UPDATE endpoint_webhooks SET secret_origin = 'none' WHERE trust_mode <> 'signed';

CREATE TABLE IF NOT EXISTS webhook_secret_audit (
    id               bigserial PRIMARY KEY,
    webhook_id       uuid NOT NULL REFERENCES endpoint_webhooks(id) ON DELETE CASCADE,
    action           text NOT NULL CHECK (action IN ('set', 'replace', 'previous_erased')),
    actor_kind       text NOT NULL CHECK (actor_kind IN ('endpoint', 'human', 'system')),
    actor_id         uuid,                     -- the endpoint or human that wrote it; NULL for system
    path             text NOT NULL CHECK (path IN ('mcp', 'web', 'api', 'sweep')),
    fingerprint      text,                     -- first 12 hex of the secret's SHA-256; never the secret
    prev_fingerprint text,
    at               timestamptz NOT NULL DEFAULT now()
);

-- "Who set this secret, latest first" is read for every row of list_webhooks and every owner opening
-- the endpoint card; both are keyed on the webhook and ordered by time.
CREATE INDEX IF NOT EXISTS idx_webhook_secret_audit_webhook ON webhook_secret_audit (webhook_id, at DESC);
