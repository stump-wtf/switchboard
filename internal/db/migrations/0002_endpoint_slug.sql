-- 0002_endpoint_slug — per-endpoint MCP URL slug + last-seen stamp (SPEC-0014, ADR-0017).
-- Each vended endpoint is served at /mcp/{slug}. The slug is minted at vend time and is NOT a
-- secret: the URL alone grants nothing (auth is the bearer credential; SPEC-0014 REQ "Bearer
-- Authentication Bound to the Vended Endpoint"). last_seen_at is stamped on every successful
-- authentication so humans can see whether a vended credential is alive.

ALTER TABLE endpoints ADD COLUMN slug text;
ALTER TABLE endpoints ADD COLUMN last_seen_at timestamptz;

-- Backfill existing rows: derive from the owning agent's name (lowercased, non-alphanumeric runs
-- collapsed to '-') plus a short random suffix so slugs are unique and non-guessy. Agents whose
-- names contain no usable characters fall back to the literal base 'endpoint'.
UPDATE endpoints e
SET slug = CASE WHEN base.b = '' THEN 'endpoint' ELSE base.b END
           || '-' || substr(md5(random()::text || e.id::text), 1, 8)
FROM agents a,
     LATERAL (SELECT trim(both '-' from regexp_replace(lower(a.name), '[^a-z0-9]+', '-', 'g')) AS b) AS base
WHERE a.id = e.agent_id AND e.slug IS NULL;

ALTER TABLE endpoints ALTER COLUMN slug SET NOT NULL;
CREATE UNIQUE INDEX idx_endpoints_slug ON endpoints (slug);
