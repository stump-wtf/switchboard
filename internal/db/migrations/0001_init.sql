-- 0001_init — switchboard core schema.
-- Humans (OIDC principals) + agents + vended endpoints + the durable todo queue + events, all in one
-- database so a vended endpoint's scope, an agent's owner, and a todo's queue are joinable and
-- enforced transactionally (ADR-002, ADR-007, ADR-008).

-- Humans: the accountable principals. The IdP (Pocket ID) holds HUMANS ONLY — never agents. ADR-008/011.
CREATE TABLE humans (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    oidc_subject text UNIQUE NOT NULL,
    display_name text,
    email        text,
    created_at   timestamptz NOT NULL DEFAULT now()
);

-- Sessions: server-side, revocable web sessions. The cookie carries an opaque token; we store its hash.
CREATE TABLE sessions (
    token_hash text PRIMARY KEY,
    human_id   uuid NOT NULL REFERENCES humans(id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL
);
CREATE INDEX idx_sessions_human ON sessions (human_id);

-- Agents: lightweight owned records. Registration grants nothing; power comes only from a vend. ADR-008.
CREATE TABLE agents (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_human_id uuid NOT NULL REFERENCES humans(id) ON DELETE CASCADE,
    name           text NOT NULL,
    description    text,
    runtime_meta   jsonb,
    created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_agents_owner ON agents (owner_human_id);

-- Endpoints: the vended capability. URL + credential together ARE the grant; scope is immutable
-- (change access by revoke + re-vend). The plaintext credential is shown once; only its hash is stored. ADR-008.
CREATE TABLE endpoints (
    id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_id             uuid NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    persona_id           uuid,                                  -- ADR-009; null = agent-level endpoint
    credential_hash      text NOT NULL,                         -- SHA-256(token); never reversible
    credential_prefix    text NOT NULL,                         -- non-secret display hint, e.g. "sbk_ab12cd"
    scope_queues         text[] NOT NULL DEFAULT '{}',
    scope_verbs          text[] NOT NULL DEFAULT '{}',
    webhook_max          int NOT NULL DEFAULT 0,                -- ceiling (ADR-012)
    webhook_source_types text[] NOT NULL DEFAULT '{}',
    webhook_queues       text[] NOT NULL DEFAULT '{}',
    mutability           text NOT NULL DEFAULT 'immutable',     -- ADR-008 open question, proposed immutable
    state                text NOT NULL DEFAULT 'active',        -- active|revoked
    created_at           timestamptz NOT NULL DEFAULT now(),
    revoked_at           timestamptz
);
CREATE UNIQUE INDEX idx_endpoints_credhash ON endpoints (credential_hash);
CREATE INDEX idx_endpoints_agent ON endpoints (agent_id);

-- Events: one row per accepted inbound delivery. Headers are sanitized; payload kept for replay. ADR-002/003.
CREATE TABLE events (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    source        text NOT NULL,
    family        text NOT NULL,           -- webhook|queue
    event_type    text,
    external_id   text,                    -- provider delivery id, for dedupe
    trust_mode    text NOT NULL,           -- signed|token|open|queue
    verified      boolean NOT NULL,
    verify_detail text,
    content_type  text,
    headers       jsonb,                   -- SANITIZED (signature/secret headers redacted)
    payload       bytea,
    payload_size  int NOT NULL DEFAULT 0,
    source_ip     inet,
    received_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_events_source_time ON events (source, received_at DESC);
CREATE UNIQUE INDEX idx_events_dedupe ON events (source, external_id) WHERE external_id IS NOT NULL;

-- Todos: the durable work-queue. SQS-style visibility model: pending → claimed(lease) → done|failed. ADR-007/002.
CREATE TABLE todos (
    id               text PRIMARY KEY,
    queue            text NOT NULL,
    source           text,
    kind             text,
    title            text NOT NULL,
    payload          jsonb,
    event_id         bigint REFERENCES events(id) ON DELETE SET NULL,
    idempotency_key  text,
    assignee         text,
    state            text NOT NULL DEFAULT 'pending',   -- pending|claimed|done|failed
    owner            text,
    lease_expires_at timestamptz,
    attempt          int NOT NULL DEFAULT 0,
    max_attempts     int NOT NULL DEFAULT 5,
    result           jsonb,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    claimed_at       timestamptz,
    completed_at     timestamptz
);
-- Hot claim scan: pending rows per queue by age.
CREATE INDEX idx_todos_pending ON todos (queue, created_at) WHERE state = 'pending';
-- Idempotency: at most one non-terminal todo per (queue, idempotency_key).
CREATE UNIQUE INDEX idx_todos_dedupe ON todos (queue, idempotency_key)
    WHERE idempotency_key IS NOT NULL AND state <> 'done' AND state <> 'failed';

-- Adapters: ingestion adapter registry — runtime enable/disable + NON-SECRET config. ADR-014.
CREATE TABLE adapters (
    name       text PRIMARY KEY,
    family     text NOT NULL,               -- webhook|queue
    trust_mode text NOT NULL,               -- signed|token|open|queue
    enabled    boolean NOT NULL DEFAULT true,
    config     jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Settings: typed key/value app config.
CREATE TABLE settings (
    key   text PRIMARY KEY,
    value text NOT NULL
);
INSERT INTO settings (key, value) VALUES
    ('retention_max_age_days', '30'),
    ('retention_max_rows', '500000'),
    ('sse_retry_ms', '3000')
ON CONFLICT DO NOTHING;
