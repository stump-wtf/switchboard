-- 0029_endpoint_default_lease — the lease a claim on this endpoint gets when the call names none.
--
-- A claim's lease was 300s unless every claim, claim_next and heartbeat passed lease_ttl_seconds,
-- and a heartbeat without it cut a long lease back to 300s. Workers whose jobs run 15 to 60
-- minutes lost their lease mid-run whenever they forgot the parameter. This column lets the
-- endpoint's human set the default for every claim on the endpoint: NULL keeps the server default
-- (300s); a value applies to claim, claim_next and heartbeat calls that name no lease_ttl_seconds.
-- An explicit per-call value still wins.
--
-- The bounds are internal/lease's MinDefaultSeconds and MaxSeconds (60 and 86400); a store test
-- pins this CHECK to those constants. The setting is not scope (SPEC-0007): editing it needs no
-- re-vend, and it changes only claims and heartbeats made after the edit.
--
-- Additive and nullable: existing rows read NULL and keep the 300s default. Rollback is
-- ALTER TABLE endpoints DROP COLUMN default_lease_ttl_seconds.
--
-- Governing: ADR-0043, SPEC-0007 REQ "Endpoint Default Lease", SPEC-0006 REQ "Lease Lifecycle and
-- Crash Safety".
ALTER TABLE endpoints ADD COLUMN default_lease_ttl_seconds integer
    CONSTRAINT endpoints_default_lease_ttl_seconds_check
    CHECK (default_lease_ttl_seconds IS NULL OR default_lease_ttl_seconds BETWEEN 60 AND 86400);
