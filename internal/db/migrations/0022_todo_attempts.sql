-- Attempt History On Todos
--
-- One todo_attempts row per committed claim, closed by the transition that ends that lease
-- (SPEC-0034 REQ-1..3, ADR-0039). Rows are keyed by (todo_id, seq), cascade with their todo, and
-- carry no scope column of their own: every read reaches an attempt through its todo, so the
-- todo's owner scope is the attempt's (REQ-10). seq is todos.attempts_total after the claim's
-- increment, so it needs no sequence object and is never reset — a manual retry resets
-- todos.attempt, never attempts_total (REQ-12). attempts_pruned counts rows the per-todo cap
-- deleted (REQ-11).
--
-- "At most one open attempt per todo" is a DEFERRED exclusion constraint, not a unique partial
-- index. A lease takeover closes the old attempt and opens the new one in data-modifying CTEs of
-- one statement, and Postgres runs those in an unspecified order; a unique index is checked row by
-- row and could see the INSERT before the close. The deferred constraint checks the committed
-- result only (REQ-2).
--
-- Safe on live data: ADD COLUMN ... DEFAULT 0 is metadata-only, and the backfill touches only
-- in-flight leases (claimed or an interrupt state), opening one attempt each with
-- claimant 'migrated' so the holder's eventual complete/fail closes it (REQ-17). Terminal todos
-- get no invented history.
--
-- @joestump-agent 09/23/2026 - Added for #315 (SPEC-0034, epic #313).
CREATE TABLE todo_attempts (
    todo_id             text        NOT NULL REFERENCES todos(id) ON DELETE CASCADE,
    seq                 int         NOT NULL,
    attempt             int         NOT NULL,
    claimer_kind        text        NOT NULL CHECK (claimer_kind IN ('endpoint', 'owner')),
    claimer_endpoint_id uuid        REFERENCES endpoints(id) ON DELETE SET NULL,
    claimer_session     text,
    owner               text        NOT NULL,
    claimant            text        CHECK (octet_length(claimant) <= 128),
    claimed_at          timestamptz NOT NULL DEFAULT now(),
    last_heartbeat_at   timestamptz,
    lease_expires_at    timestamptz NOT NULL,
    ended_at            timestamptz,
    outcome             text CHECK (outcome IN ('completed', 'failed', 'released', 'lease_expired',
                                                'reaped', 'canceled', 'revoked')),
    disposition         text CHECK (disposition IN ('done', 'retry_scheduled', 'requeued',
                                                    'dead_lettered', 'canceled')),
    summary             text        CHECK (octet_length(summary) <= 2048),
    summary_truncated   boolean     NOT NULL DEFAULT false,
    artifact            text        CHECK (octet_length(artifact) <= 512),
    lease_token_hash    bytea       CHECK (octet_length(lease_token_hash) = 32),
    PRIMARY KEY (todo_id, seq),
    CHECK ((ended_at IS NULL) = (outcome IS NULL)),
    CHECK ((ended_at IS NULL) = (disposition IS NULL))
);

ALTER TABLE todo_attempts ADD CONSTRAINT todo_attempts_one_open
    EXCLUDE USING btree (todo_id WITH =) WHERE (ended_at IS NULL)
    DEFERRABLE INITIALLY DEFERRED;

-- Provenance lookups by endpoint (the SET NULL cascade) without a sequential scan.
CREATE INDEX idx_todo_attempts_claimer_endpoint ON todo_attempts (claimer_endpoint_id)
    WHERE claimer_endpoint_id IS NOT NULL;

ALTER TABLE todos ADD COLUMN attempts_total  int NOT NULL DEFAULT 0;
ALTER TABLE todos ADD COLUMN attempts_pruned int NOT NULL DEFAULT 0;

UPDATE todos SET attempts_total = 1
WHERE state IN ('claimed', 'input-required', 'auth-required');

INSERT INTO todo_attempts (todo_id, seq, attempt, claimer_kind, owner, claimant, claimed_at,
                           lease_expires_at)
SELECT id, 1, attempt, 'endpoint', COALESCE(owner, ''), 'migrated', COALESCE(claimed_at, now()),
       COALESCE(lease_expires_at, now())
FROM todos
WHERE state IN ('claimed', 'input-required', 'auth-required');
