---
status: approved
date: 2026-07-06
implements: [ADR-0002]
---

# SPEC-0004: Persistence Layer

## Overview

This capability defines switchboard's system of record: a single **PostgreSQL** database
accessed through a thin raw-SQL data-access layer over `pgx` (no ORM). It realizes
[ADR-0002](../../../adrs/ADR-0002-postgres-persistence-and-retention.md), which chose PostgreSQL
over SQLite, a dedicated broker, and Redis-as-ledger because switchboard is a self-hosted,
multi-tenant service whose hot path is a **concurrent multi-worker job queue** — not an
append-mostly log — with transactional dedup, competing consumers, and store-then-ack coupling to
ingestion.

The persistence layer owns the schema (humans, sessions, agents, endpoints, events, todos,
adapters, settings), the connection pool lifecycle, the embedded migration mechanism, the partial
and expression indexes that keep the hot queue scan cheap, the hybrid age + row-cap retention
policy, and the optional `LISTEN`/`NOTIFY` wakeups. Every other capability — including the todo
work-queue ([SPEC-0003](../todo-queue/spec.md)) — builds on the primitives this capability
provides. It is the datastore contract the rest of the system is written against; it does not itself
serve HTTP.

The reference implementation is `internal/db/db.go` (pool + migrations), `internal/store/` (typed
data-access functions), and `internal/db/migrations/0001_init.sql` (schema DDL).

## Requirements

### Requirement: PostgreSQL as the Sole System of Record

Switchboard MUST use PostgreSQL as its single system of record for all durable state: humans,
sessions, agents, vended endpoints, events, todos, adapters, and settings. It MUST NOT use SQLite,
because single-writer serialization and a single-node file are the wrong shape for a multi-tenant,
multi-worker queue. It MUST NOT use Redis as the ledger — Redis appears only as an ingestion
transport, never as storage. A dedicated broker (Kafka/NATS/Temporal) MUST NOT be introduced unless
PostgreSQL is demonstrably insufficient at scale; such an addition REQUIRES a new ADR.

All durable state SHALL live in one database so that a vended endpoint's scope, an agent's owner,
and a todo's queue are joinable and enforceable within a single transaction.

#### Scenario: Multi-node app tier against one database

- **WHEN** more than one switchboard app node is deployed
- **THEN** every node MUST connect to the same PostgreSQL database, and availability/HA MUST be
  achievable through managed or streaming replication rather than being blocked by a single-host file

#### Scenario: Redis is transport, not storage

- **WHEN** a pull adapter consumes a message from a Redis stream
- **THEN** the durable record MUST be written to PostgreSQL, and Redis MUST NOT be treated as the
  authoritative store for that work

### Requirement: Connection Pool Lifecycle and Timeouts

Switchboard MUST obtain database connections from a single `pgx` connection pool created at startup
from a DSN. The DSN MUST be injected via environment/deployment configuration (`DATABASE_URL`) and
MUST NOT be committed to source. Startup MUST fail fast with a wrapped error when the DSN is empty or
when connectivity cannot be verified. The pool MUST verify connectivity (a `Ping`) before the
service accepts work, and MUST be closed cleanly on shutdown. Every query MUST accept a
`context.Context` so that cancellation and timeouts propagate to the database driver.

#### Scenario: Empty or unreachable DSN fails fast

- **WHEN** `DATABASE_URL` is empty, or the database cannot be reached at startup
- **THEN** pool construction MUST return a wrapped error naming the failure (`db: empty
  DATABASE_URL`, `db: connect`, or `db: ping`) and the service MUST NOT start serving

#### Scenario: Context governs every query

- **WHEN** a caller passes a context that is cancelled or exceeds its deadline mid-query
- **THEN** the in-flight database operation MUST observe the cancellation and return promptly rather
  than blocking indefinitely

### Requirement: Embedded, Ordered, Transactional Migrations

Schema evolution MUST be driven by plain `.sql` files embedded into the binary and applied on
startup in filename (lexical) order. Each migration MUST run inside its own transaction, and MUST be
rolled back atomically if either the DDL or its bookkeeping insert fails. Applied migrations MUST be
recorded in a `schema_migrations` table keyed by version, and a migration already recorded there
MUST be skipped on subsequent starts (idempotent, at-most-once application). No external migration
tool SHALL be required — switchboard MUST remain a single static binary.

#### Scenario: Migration applied exactly once

- **WHEN** the service starts and a migration file is not present in `schema_migrations`
- **THEN** its SQL MUST be executed in a transaction and, on success, its version recorded — and on
  every later start that same migration MUST be skipped

#### Scenario: Failed migration leaves no partial state

- **WHEN** a migration's DDL fails partway through
- **THEN** the enclosing transaction MUST be rolled back, the version MUST NOT be recorded, and
  startup MUST abort with a wrapped error naming the migration

### Requirement: Schema, Partial Indexes, and Rich Column Types

The initial migration MUST create the core tables — `humans`, `sessions`, `agents`, `endpoints`,
`events`, `todos`, `adapters`, `settings` — with `timestamptz` timestamps and first-class rich
types (`jsonb` for payloads/config/results, `text[]` for scope and source-type arrays, `inet` for
source IPs, `bytea` for raw event bodies). It MUST create the hot-path partial and unique indexes:

- `idx_todos_pending` on `todos (queue, created_at) WHERE state = 'pending'` — the claim scan MUST
  touch only live work, never the terminal backlog.
- `idx_todos_dedupe` UNIQUE on `todos (queue, idempotency_key) WHERE idempotency_key IS NOT NULL AND
  state <> 'done' AND state <> 'failed'` — at most one non-terminal todo per `(queue,
  idempotency_key)`.
- `idx_events_dedupe` UNIQUE on `events (source, external_id) WHERE external_id IS NOT NULL` —
  event-level delivery dedup.
- `idx_events_source_time` on `events (source, received_at DESC)` for read surfaces.

Switchboard-minted credentials MUST be stored hashed (e.g. `endpoints.credential_hash`); no signing
secret or plaintext credential SHALL be persisted in any row.

#### Scenario: Hot claim scan hits the partial index

- **WHEN** a worker asks for the next pending todo on a queue while many terminal rows exist
- **THEN** the query MUST be served by `idx_todos_pending` and MUST NOT scan terminal rows

#### Scenario: No plaintext secrets at rest

- **WHEN** a credential is minted for a vended endpoint
- **THEN** only its hash and a non-secret display prefix MUST be stored, and the plaintext MUST NOT
  be recoverable from the database

### Requirement: Parameterized Queries Only (Database Operation Standards)

Every SQL statement MUST be issued with bound parameters (`$1`, `$2`, …); user- or
producer-supplied values MUST NOT be concatenated or interpolated into SQL text. The data-access
layer MUST be a thin, centralized set of typed functions (`InsertEvent`, `CreateTodo`, `ClaimTodo`,
`CompleteTodo`, `ReapExpired`, `Prune`, …) so the hot queries stay auditable. Multi-step atomic
mutations MUST run inside a single transaction. Every database function MUST accept and honor a
`context.Context`. Domain lookups that match no row MUST return a sentinel error (`ErrNotFound`), and
lost state-transition races MUST return a distinct sentinel (`ErrConflict`), rather than being
silently swallowed.

#### Scenario: Producer input is always bound, never interpolated

- **WHEN** any value derived from a webhook, queue message, or API caller reaches a query
- **THEN** it MUST be passed as a bound parameter and MUST NOT be spliced into the SQL string

#### Scenario: Missing row versus lost race are distinguishable

- **WHEN** a conditional update affects zero rows
- **THEN** the layer MUST return `ErrNotFound` when the row is absent and `ErrConflict` when the row
  exists but was not in the expected state, never an ambiguous nil result

### Requirement: In-Database Wakeups via LISTEN/NOTIFY

The persistence layer SHOULD support `LISTEN`/`NOTIFY` so that an idle worker or the web UI can be
nudged when new work arrives instead of polling continuously. A `NOTIFY` on the `todo_ready` channel
(carrying the queue name) MAY be emitted when a pending todo is created. Because the durable queue
remains the source of truth, a missed notification MUST only cost latency and MUST NOT cause lost
work; correctness MUST NOT depend on delivery of any notification.

#### Scenario: Missed notification costs only latency

- **WHEN** a worker is not currently listening and a `todo_ready` notification is emitted and lost
- **THEN** the pending todo MUST still be claimable on the next poll or claim scan, with no work lost

### Requirement: Hybrid Retention and Bounded Growth

Switchboard MUST bound row growth with a **hybrid retention policy**: both a max-age limit and a
max-row cap. Defaults MUST be seeded into `settings` (`retention_max_age_days = 30`,
`retention_max_rows = 500000`). A periodic pruning task MUST delete events and terminal todos older
than the age limit and then trim the oldest rows beyond the row cap, performing the delete-then-trim
as one transaction. Non-terminal todos (pending/claimed) MUST NOT be pruned. Time-range partitioning
of the event log (turning retention into a cheap `DROP PARTITION`) is RECOMMENDED for high-volume
deployments and MAY be adopted without changing this contract.

#### Scenario: Prune bounds both age and size atomically

- **WHEN** the pruning task runs with rows exceeding the age limit and the total exceeding the row
  cap
- **THEN** it MUST delete over-age events and terminal todos and trim the oldest beyond the cap in a
  single transaction, leaving all pending and claimed todos intact
