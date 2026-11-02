-- 0037_oauth_device_codes — RFC 8628 device authorization requests for the operator API.
-- A headless CLI (no loopback callback can reach it) starts a device flow here instead of the
-- authorization-code flow: the row is the pending request — the requesting client, the short
-- human-typeable user code (stored hashed, like every other OAuth credential), the long device
-- code (stored hashed only; the plaintext rides the CLI's polling and dies there), and the clocks.
-- Until a human approves there is NO principal on the row: the device grant binds to the APPROVING
-- human (human_id set at approval, operator shape per 0014_operator_oauth), and the table has no
-- endpoint_id column at all — the device flow structurally cannot mint an endpoint credential,
-- only the operator-bound pair the code flow issues. last_polled_at/interval_seconds enforce the
-- RFC 8628 polling interval (slow_down). Entry rate limiting lives in oauth_device_entry_attempts
-- below, NOT on this row: a wrong user code hashes to no row at all, so a per-request counter
-- could never see a guess that missed, which is exactly the traffic worth counting.
-- Governing: ADR-0019 (hashed at rest), ADR-0023 (operator API resource binding),
-- ADR-0057 (proposed, device-grant parameters).

CREATE TABLE oauth_device_codes (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    device_code_hash text NOT NULL UNIQUE,
    user_code_hash text NOT NULL UNIQUE,
    client_id text NOT NULL,
    human_id uuid REFERENCES humans(id) ON DELETE CASCADE,
    expires_at timestamptz NOT NULL,
    authorized_at timestamptz,
    denied_at timestamptz,
    used_at timestamptz,
    last_polled_at timestamptz,
    interval_seconds integer NOT NULL DEFAULT 5 CHECK (interval_seconds > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    -- A row is spent only after a decision: used_at without authorized_at would mean the token
    -- endpoint minted from a request nobody approved. Denial leaves the row decided-but-never
    -- spendable (authorized_at stays NULL, so used_at can never follow).
    CONSTRAINT oauth_device_codes_spend_after_approval CHECK (used_at IS NULL OR authorized_at IS NOT NULL)
);

CREATE INDEX idx_oauth_device_codes_expires ON oauth_device_codes (expires_at);

-- oauth_device_entry_attempts is the user-code entry rate limiter, one row per acting principal.
-- Counting guesses against the pending request would be useless: a WRONG code hashes to no row,
-- so the misses — the whole point of a brute-force — would never be counted. Counting them per
-- actor works because the entry surface is session-gated: every lookup has a signed-in principal
-- to charge. Past the window's cap the store refuses the lookup whatever it hashes to, so grinding
-- the short code's keyspace takes centuries instead of hours, and the honest human who trips it by
-- accident just waits a minute. One row per human, so this table cannot grow unbounded; a stale
-- counter resets itself on the next lookup rather than needing a sweep.
CREATE TABLE oauth_device_entry_attempts (
    gate_key text PRIMARY KEY,
    guess_count integer NOT NULL DEFAULT 0 CHECK (guess_count >= 0),
    guessed_at timestamptz NOT NULL DEFAULT now()
);
