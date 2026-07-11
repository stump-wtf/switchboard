-- 0006_endpoint_webhooks — agent self-managed webhooks within a human-vended ceiling.
-- An agent creates/rotates/deletes its own ingestion webhooks through the vended MCP endpoint, but
-- only within the ceiling columns already on `endpoints` (webhook_max, webhook_source_types,
-- webhook_queues). For a signed-type webhook (github/stripe/slack) switchboard MINTS the HMAC
-- signing secret, HOLDS it server-side here (the plaintext, not a hash — switchboard must recompute
-- the provider HMAC over each inbound body to verify it per SPEC-0003, which a one-way hash could
-- not do), and REVEALS it to the agent exactly once at create/rotate so the agent can configure the
-- producer. Trust mode is derived by switchboard from the source type, never supplied by the agent,
-- so self-management can never downgrade verification.
-- Governing: ADR-0012 (agents self-manage webhooks within a vended ceiling),
-- SPEC-0006 REQ "Webhook Self-Management Within a Vended Ceiling",
-- SPEC-0006 REQ "Switchboard Owns Secrets, Verification, and Idempotency", ADR-0003 (per-source trust).
CREATE TABLE endpoint_webhooks (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    endpoint_id    uuid NOT NULL REFERENCES endpoints(id) ON DELETE CASCADE,
    source_type    text NOT NULL,                     -- github|generic|… (within the endpoint's ceiling)
    target_queue   text NOT NULL,                     -- within the endpoint's webhook_queues grant
    trust_mode     text NOT NULL,                     -- signed|token — DERIVED by switchboard, never agent-supplied
    ingest_token   text NOT NULL,                     -- non-secret URL routing token (/webhooks/w/{token})
    signing_secret text,                              -- minted HMAC secret switchboard holds to verify signed deliveries; NULL for token/open
    created_at     timestamptz NOT NULL DEFAULT now(),
    rotated_at     timestamptz                        -- last secret/URL rotation, NULL until first rotate
);
-- The ingest token routes deliveries to exactly one webhook; it must be globally unique.
CREATE UNIQUE INDEX idx_endpoint_webhooks_token ON endpoint_webhooks (ingest_token);
-- Hot path: count/list an endpoint's webhooks for the ceiling check and list_webhooks.
CREATE INDEX idx_endpoint_webhooks_endpoint ON endpoint_webhooks (endpoint_id, created_at DESC);
