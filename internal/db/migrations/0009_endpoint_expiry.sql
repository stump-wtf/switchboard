-- 0009_endpoint_expiry — optional credential lifetime on vended endpoints.
-- `expires_at` is chosen at vend time (the SPEC-0015 wizard's lifetime step); NULL means the
-- endpoint stays valid until explicitly revoked. Expiry is enforced as REVOCATION, not a parallel
-- lifecycle: auth refuses an expired credential immediately (EndpointByCredHash), and the reaper
-- flips the row to state='revoked' (revoked_at stamped) and closes its live MCP sessions — reusing
-- SPEC-0007's instant-and-total revocation semantics for both credential shapes (the static sbk_
-- bearer and the coming OAuth tokens, whose lifetime is clamped to the endpoint's).
-- Governing: ADR-0019, SPEC-0016 REQ "Credential Lifetime", SPEC-0007 REQ "Instant, Total Revocation".
ALTER TABLE endpoints ADD COLUMN expires_at timestamptz;
-- Reaper scan: only active rows that carry an expiry are candidates; the partial index keeps the
-- 30s tick from scanning no-expiry and already-revoked rows.
CREATE INDEX idx_endpoints_expiry ON endpoints (expires_at)
    WHERE state = 'active' AND expires_at IS NOT NULL;
