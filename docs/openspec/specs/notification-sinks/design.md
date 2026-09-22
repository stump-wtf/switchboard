---
status: draft
date: 2026-09-22
implements: [ADR-0034]
---

# Design: Notification Sinks and Queue Digests

## Context

[SPEC-0029](spec.md) lets an owner be told about dead letters, quarantine, relay exhaustion, rule
matches and queue health through Gotify and Apprise. What it builds on:

* **Dead-letter paths** already exist in `internal/store/todos.go`: `FailTodo` (and
  `FailTodoOperatorOwned`) at the attempt cap, `ReapExpired` at the cap, and `deadLetterEndpointTodos` in the revocation cascade. Each runs in a
  transaction, which is where the outbox row is written.
* **Routing actions** are `routing.Action` (`internal/routing/routing.go`): `queue`, `drop`,
  `endpoints`, `exclusive`, `once`, `work_order`. `notify` is one more field, validated like the others.
* **The connection vault** from SPEC-0028 (`outbound_connections`, in flight) holds the credentials.
  If SPEC-0028's vault story has not landed when this work starts, its migration and store wrapper are
  the first story here; the two specs share it by design.
* **`internal/push.Validator`** guards every dial, as it does for ADR-0029's hooks.
* **SPEC-0022's shift grammar** and embedded zone database give the digest schedule its parser.

## Goals / Non-Goals

### Goals

- Per-owner, per-team alerting on a shared instance, with no instance-wide sink.
- Never lose the message that matters (a dead letter), and never flood.
- One place for all outbound credentials.
- A digest that is honest about what it could not measure.

### Non-Goals

- Human work tracking of any kind (REQ-1).
- Rich per-service formatting. Messages are plain text; Apprise renders per service.
- Operator alerting. Operators use ADR-0028 metrics and Alertmanager.
- Claude Code permission relay (ADR-0034, future work).
- An agent verb that pages a human (deferred in ADR-0034).

## Decisions

### A transactional outbox, not the notify-hook worker's in-memory queue

**Choice**: `notification_outbox`, written in the triggering transaction, drained by a worker with
`FOR UPDATE SKIP LOCKED`.

**Rationale**: ADR-0029's doorbell can be lossy because the queue still holds the todo. A dead letter
notice cannot: the dead letter is the thing nobody is looking at. The outbox also makes dedup windows,
budgets and "exactly one instance delivers" correct across replicas without coordination.

**Alternatives considered**:
- In-process queue: rejected, loses messages on restart and multiplies budgets by instance count.
- A trigger or `LISTEN/NOTIFY`: rejected as the primary path; `NOTIFY` MAY wake the worker early, but
  polling the outbox remains the source of truth.

### Dedup by folding into the pending row

**Choice**: a partial unique index on `(sink_id, dedup_key) WHERE state = 'pending'`; an insert that
conflicts does `count = count + 1, last_event_at = now()`.

**Rationale**: the storm case (hundreds of quarantines) becomes one row and one message with a count,
with no separate dedup cache.

### Budgets computed from delivery records

**Choice**: before sending, count this sink's `sent` rows in the last hour; if over budget, leave the row
`pending` with `held = true`. When budget returns, the worker claims all held rows for the sink at once
and sends one coalesced message.

**Rationale**: correct across instances and restarts, and no token-bucket state to persist.

### Sinks reference vault connections

**Choice**: `notification_sinks.connection_id` references `outbound_connections`, with the same owner
scope enforced by the store on insert and by the worker on every delivery.

**Rationale**: one set of credential rules (SPEC-0028 REQ-3), one rotate button, one test.

### `relay.exhausted` replaces `todo.dead_lettered` for relay todos

**Choice**: the dead-letter hook asks the attempt history (SPEC-0034) whether any attempt was a relay
attempt; if so, it enqueues `relay.exhausted` instead.

**Rationale**: one message per failure, carrying the attempt count and last Cairn handle a human needs.
Until SPEC-0034 exists, every dead letter is `todo.dead_lettered`.

### The digest is a query, not a counter

**Choice**: the builder runs one grouped query per section at send time, each with a timeout; a
section that fails or whose source does not exist is marked unavailable.

**Rationale**: the same reason SPEC-0023 computes its gauges at scrape time: a query cannot disagree
with the table it reads, and a flat zero from a broken counter is exactly the false calm this exists to
prevent.

### Apprise stateful mode is the recommended path

**Choice**: the UI offers "configuration key" first and "Apprise URLs" second, with the scheme
allowlist enforced on the second.

**Rationale**: with a key, service tokens never reach Switchboard, and the Apprise server's own
`APPRISE_ALLOW_SERVICES` governs what it may call.

## Schema

The migration number is the next free one when the story lands.

```sql
-- Connection kinds gotify and apprise are rows in SPEC-0028's outbound_connections.

CREATE TABLE notification_sinks (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_human_id  uuid REFERENCES humans(id) ON DELETE CASCADE,
    owner_team_id   uuid,                         -- FK to teams(id) once SPEC-0033 lands
    name            text NOT NULL,
    kind            text NOT NULL,                -- gotify|apprise
    connection_id   uuid NOT NULL REFERENCES outbound_connections(id) ON DELETE CASCADE,
    options         jsonb NOT NULL DEFAULT '{}',  -- priority/type maps, config_key, tag
    enabled         boolean NOT NULL DEFAULT true,
    created_by_human_id uuid NOT NULL REFERENCES humans(id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    CHECK (num_nonnulls(owner_human_id, owner_team_id) = 1)
);
CREATE UNIQUE INDEX notification_sinks_name
    ON notification_sinks (COALESCE(owner_human_id, owner_team_id), name);

CREATE TABLE notification_subscriptions (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    sink_id       uuid NOT NULL REFERENCES notification_sinks(id) ON DELETE CASCADE,
    event_types   text[] NOT NULL,
    queues        text[] NOT NULL DEFAULT '{}',
    endpoint_ids  uuid[] NOT NULL DEFAULT '{}',
    webhook_ids   uuid[] NOT NULL DEFAULT '{}',
    min_severity  text NOT NULL DEFAULT 'low'
);

CREATE TABLE notification_outbox (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    sink_id        uuid NOT NULL REFERENCES notification_sinks(id) ON DELETE CASCADE,
    owner_scope_id uuid NOT NULL,
    event_type     text NOT NULL,
    severity       text NOT NULL,
    dedup_key      text NOT NULL,
    count          int  NOT NULL DEFAULT 1,
    subject        jsonb NOT NULL,   -- todo id, queue, cause, title (truncated), links; never payload
    state          text NOT NULL DEFAULT 'pending',  -- pending|sent|undeliverable|partial
    held           boolean NOT NULL DEFAULT false,
    attempts       int  NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    last_error_class text,
    created_at     timestamptz NOT NULL DEFAULT now(),
    last_event_at  timestamptz NOT NULL DEFAULT now(),
    sent_at        timestamptz
);
CREATE UNIQUE INDEX notification_outbox_dedup
    ON notification_outbox (sink_id, dedup_key) WHERE state = 'pending';
CREATE INDEX notification_outbox_due
    ON notification_outbox (next_attempt_at) WHERE state = 'pending';

CREATE TABLE queue_digests (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_human_id   uuid REFERENCES humans(id) ON DELETE CASCADE,
    owner_team_id    uuid,
    name             text NOT NULL,
    schedule         text NOT NULL,       -- TZ=… Mon-Fri 09:00,17:00
    queues           text[] NOT NULL DEFAULT '{}',
    sink_ids         uuid[] NOT NULL DEFAULT '{}',
    cairn_connection_id uuid REFERENCES outbound_connections(id) ON DELETE SET NULL,
    cairn_ttl        interval NOT NULL DEFAULT '7 days',
    skip_when_quiet  boolean NOT NULL DEFAULT true,
    age_threshold    interval NOT NULL DEFAULT '1 hour',
    last_sent_at     timestamptz,
    CHECK (num_nonnulls(owner_human_id, owner_team_id) = 1)
);
```

`routing.Action` gains:

```go
Notify         string `json:"notify,omitempty"`          // sink name in the webhook's owner scope
NotifySeverity string `json:"notify_severity,omitempty"` // low|normal|high
```

## Message Shapes

Gotify:

```json
{"title": "Dead letter · forge", "priority": 8,
 "message": "“PR #482 opened in stump.wtf/switchboard” failed 5 of 5 attempts.\nBoard: https://sb.example.net/todos/td_…\nOrigin: gitea stump.wtf/switchboard#482"}
```

Apprise (stateful):

```json
POST /notify/ops
{"title": "Quarantine · inbox", "type": "warning", "format": "text",
 "body": "3 deliveries from an untrusted sender on webhook “forge” (and 2 more).\nBoard: https://sb.example.net/events?cause=quarantine"}
```

Digest (sink text when a Cairn artifact is linked):

```
Switchboard digest · Mon 09:00 · 2 need attention
forge: 3 dead letters · oldest pending 2h 10m · 0 claimed
inbox: quiet
Unheard doorbells: unavailable (not measured on this instance)
Full digest: https://cairn.example.net/a/abc123
```

## Operator API

```
GET|POST          /api/v1/sinks
PATCH|DELETE      /api/v1/sinks/{id}
POST              /api/v1/sinks/{id}/test
GET|POST          /api/v1/sinks/{id}/subscriptions
DELETE            /api/v1/sinks/{id}/subscriptions/{sid}
GET|POST          /api/v1/digests
PATCH|DELETE      /api/v1/digests/{id}
POST              /api/v1/digests/{id}/preview      -> the digest text, not sent
GET               /api/v1/notifications?state=undeliverable
```

The web UI mirrors these under **Settings → Notifications**, and on a team's settings page once
SPEC-0033 adds one. Digest preview is how an owner checks honest absence before scheduling.

## Architecture

```mermaid
sequenceDiagram
  autonumber
  participant ST as store (FailTodo / ReapExpired / revoke / ingest)
  participant OB as notification_outbox
  participant W as outbox worker (any instance)
  participant V as push.Validator
  participant G as Gotify / Apprise API
  ST->>ST: BEGIN; dead-letter the todo
  ST->>OB: match subscriptions; INSERT or fold into pending row
  ST->>ST: COMMIT (both, or neither)
  loop every few seconds
    W->>OB: SELECT due rows FOR UPDATE SKIP LOCKED
    W->>W: budget check from sent rows (held rows coalesce)
    W->>V: validate host at dial
    V-->>W: ok
    W->>G: POST message (token in header)
    G-->>W: 2xx / 424 / error
    W->>OB: sent / retry with backoff / undeliverable
  end
```

## Package Layout

```
internal/notify/            subscriptions matcher, outbox writer (called inside store txns), worker
internal/notify/sink/       gotify.go apprise.go (Send, Test)
internal/notify/digest/     schedule parser (reuses the SPEC-0022 grammar), section queries, renderer
internal/routing/           Action.Notify, validation, trace field
internal/web/notifications.go, internal/server/api.go
```

The outbox writer takes the store's transaction (`querier`) so the dead-letter update and the outbox
insert share it; it never opens its own.

## Risks / Trade-offs

- **Tenant-run relays on private networks.** → The operator's private-host allowlist bounds reach; the
  docs show the Apprise and Gotify hosts an operator must list.
- **Apprise as an SSRF relay.** → Scheme allowlist for stored URLs, stateful mode recommended, and a
  documented `APPRISE_ALLOW_SERVICES` setting for the Apprise server.
- **Outbox growth under a storm.** → Folding into pending rows keeps one row per dedup key; delivered
  rows are pruned after 30 days.
- **Digest cost.** → A handful of grouped queries per digest at most a few times a day per scope, each
  with a timeout.
- **Dependence on parallel specs.** → Every dependent section degrades to "unavailable" until its source
  exists, which is the honest-absence rule doing its job.

## Migration Plan

Additive tables and one new optional routing field; existing rules parse unchanged. Rollback: disable
the worker (sinks go quiet, the outbox holds rows), then drop the tables.

## Open Questions

- Should a sink be able to subscribe to ADR-0030's unheard doorbells in real time, not only in the
  digest? Proposed: digest only for v1; a real-time unheard alert is likely to be noisy until SPEC-0025's
  thresholds are measured.
- Should owners be allowed a budget above the operator default? Proposed: yes, within an operator
  ceiling.
- Gotify markdown (`extras.client::display`) is left out for v1 so every sink receives the same text.
