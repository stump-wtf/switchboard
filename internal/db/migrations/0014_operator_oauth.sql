-- 0014_operator_oauth — Human-bound OAuth grants for the operator API and the CLI.
-- Until now every OAuth token was a credential ONTO one vended endpoint (0010_oauth). The operator
-- surface needs the other half of the model: a CLI or API client that acts AS the signed-in human —
-- registering agents and vending endpoints — holds a grant bound to that HUMAN, not to any
-- endpoint. oauth_codes/oauth_tokens gain a nullable human_id alongside the (now nullable)
-- endpoint_id: exactly one of the two is set on any row, enforced by CHECK constraints so a grant
-- can never be both, neither, or silently migrate between the two shapes. Revocation cascade is
-- unchanged for endpoint grants (endpoint deletion kills its tokens via the existing FK); a human
-- grant dies when the human row does, and replay revocation revokes by the code's own
-- (client, endpoint, human) triple.
-- Governing: ADR-0023 (operator API + CLI ride the same OAuth model as MCP, ADR-0019).
ALTER TABLE oauth_codes
    ALTER COLUMN endpoint_id DROP NOT NULL,
    ADD COLUMN human_id uuid REFERENCES humans(id) ON DELETE CASCADE;

ALTER TABLE oauth_tokens
    ALTER COLUMN endpoint_id DROP NOT NULL,
    ADD COLUMN human_id uuid REFERENCES humans(id) ON DELETE CASCADE;

-- A grant binds to exactly one principal: one endpoint (agent credential) or one human
-- (operator credential) — never both, never neither.
ALTER TABLE oauth_codes ADD CONSTRAINT oauth_codes_one_principal
    CHECK (num_nonnulls(endpoint_id, human_id) = 1);
ALTER TABLE oauth_tokens ADD CONSTRAINT oauth_tokens_one_principal
    CHECK (num_nonnulls(endpoint_id, human_id) = 1);

-- Replay revocation and the CLI's bearer resolution look up human-bound rows by hash.
CREATE INDEX idx_oauth_codes_human ON oauth_codes (human_id);
CREATE INDEX idx_oauth_tokens_human ON oauth_tokens (human_id);
