-- 0010_oauth — OAuth 2.1 authorization-server storage, layered on vended endpoints.
-- Switchboard is the authorization server for its own MCP mounts (ADR-0019): MCP clients register
-- dynamically (RFC 7591), run authorization-code + PKCE, and hold opaque tokens that are
-- credentials ONTO EXISTING vended endpoints — never a parallel grant universe. Codes and tokens
-- therefore hang off endpoints(id) with ON DELETE CASCADE, so SPEC-0007's "revoke = instant and
-- total" doctrine survives byte-for-byte: deleting the endpoint row kills every OAuth credential in
-- the same stroke. Only SHA-256 hashes of codes/tokens are stored (internal/cred primitives); the
-- plaintext exists solely in flight. Clients are public (PKCE, no client secret), so oauth_clients
-- holds no secret material at all — client_id is an identifier, not a credential.
-- Governing: ADR-0019, SPEC-0016 REQ "Dynamic Client Registration", REQ "Token Issuance And
-- Refresh", REQ "Revocation Cascade"; design.md "Storage (one migration)".
CREATE TABLE oauth_clients (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    client_id     text NOT NULL UNIQUE,      -- public identifier handed back by RFC 7591 registration
    name          text NOT NULL DEFAULT '',  -- client_name, display-only (consent screen)
    redirect_uris text[] NOT NULL,           -- exact-match allowlist; validated again at authorize time
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE oauth_codes (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    code_hash      text NOT NULL,            -- SHA-256 of the single-use authorization code
    client_id      text NOT NULL REFERENCES oauth_clients(client_id) ON DELETE CASCADE,
    endpoint_id    uuid NOT NULL REFERENCES endpoints(id) ON DELETE CASCADE,
    pkce_challenge text NOT NULL,            -- S256 code_challenge the token exchange must satisfy
    redirect_uri   text NOT NULL,            -- the exact redirect_uri the code was issued for
    expires_at     timestamptz NOT NULL,
    used_at        timestamptz               -- set on redemption; a second redemption is replay
);
-- Redemption looks the code up by hash; it must resolve to exactly one grant.
CREATE UNIQUE INDEX idx_oauth_codes_hash ON oauth_codes (code_hash);
-- Revocation/expiry sweeps walk an endpoint's outstanding codes.
CREATE INDEX idx_oauth_codes_endpoint ON oauth_codes (endpoint_id);

CREATE TABLE oauth_tokens (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    token_hash   text NOT NULL,              -- SHA-256 of the opaque access token
    refresh_hash text NOT NULL,              -- SHA-256 of the rotating refresh token
    client_id    text NOT NULL REFERENCES oauth_clients(client_id) ON DELETE CASCADE,
    endpoint_id  uuid NOT NULL REFERENCES endpoints(id) ON DELETE CASCADE,
    expires_at   timestamptz NOT NULL,       -- clamped to the endpoint's own expiry at issuance
    revoked_at   timestamptz,                -- set on refresh rotation or code-replay revocation
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz
);
-- The resource server resolves a presented bearer by hash on every request: unique, hot.
CREATE UNIQUE INDEX idx_oauth_tokens_hash ON oauth_tokens (token_hash);
-- Refresh grants resolve the rotating refresh token the same way.
CREATE UNIQUE INDEX idx_oauth_tokens_refresh ON oauth_tokens (refresh_hash);
-- Revocation cascades and reaper sweeps walk an endpoint's live tokens.
CREATE INDEX idx_oauth_tokens_endpoint ON oauth_tokens (endpoint_id);
