---
status: approved
date: 2026-07-06
implements: [ADR-0014, ADR-0003]
requires: [SPEC-0001]
---

# SPEC-0002: Queue Adapters (Pull Ingestion)

## Overview

Queue adapters are Switchboard's **pull** ingestion family: long-running adapters that *consume*
messages from an external broker (Redis is the reference; SQS, NATS, AMQP later), normalize each
message, persist an event for history, enqueue a durable todo, and only then acknowledge the source
message. It realizes the pull family of
[ADR-0014](../../../adrs/ADR-0014-ingestion-adapters-push-pull.md) (push vs. pull adapters sharing one
back-half contract, plus the pull-only ack coupling) and the `queue` provider family / `trust_mode`
of [ADR-0003](../../../adrs/ADR-0003-per-provider-ingestion-and-trust-model.md).

A queue message is **not** a webhook. There is no inbound HTTP request and no per-message signature —
the app consumes, and trust is the **broker connection itself** (auth/ACL + TLS). Redis is
reclassified from "another webhook source" to "the reference pull adapter." The defining mechanic of
the pull family, which push does not have, is **store-then-ack**: the source message must not be
acked/removed until the resulting todo is durably stored, so a crash mid-ingest redelivers and dedups
to exactly one todo — no loss, no duplicate. This gives pull the same at-least-once + idempotent
contract as the push family (SPEC-0001), over a different transport.

This capability owns the adapter interface, the poll-loop lifecycle (startup + graceful shutdown),
the ack-coupling rule, and enqueue-as-todo for pull sources. It shares the back-half normalization and
todo-dedup contract with SPEC-0001 (`webhook-ingestion`) and does not re-specify it.

## Requirements

### Requirement: Store-Then-Ack Coupling

A pull adapter MUST NOT acknowledge or remove a source message until the resulting todo is durably
stored in PostgreSQL. On consume, the adapter MUST derive the idempotency key from the source message
id, create the todo (dedup), confirm the todo is durably persisted, and only THEN ack/remove the
source message. A crash between consume and durable todo-store MUST leave the source message un-acked
so the broker redelivers it. The default acknowledgment boundary MUST be ack-on-store (todo
durability); an adapter MAY defer ack until todo completion for stronger coupling, but this is
OPTIONAL.

#### Scenario: Crash before todo-store redelivers and dedups to one todo

- **WHEN** an adapter consumes a message and crashes after consume but before the todo is durably
  stored
- **THEN** the source message is left un-acked, the broker redelivers it on restart, and the derived
  idempotency key (from the source message id) dedups it to exactly one todo — no duplicate, no loss

#### Scenario: Ack happens only after durable store

- **WHEN** an adapter consumes a message and creates the todo
- **THEN** the source message is acked/removed only after the todo is confirmed durably in PostgreSQL

### Requirement: Idempotency Key From Source Message Id

A pull adapter MUST derive each message's idempotency key from the source message's native id where
one exists (e.g. a Redis stream entry id), and MUST fall back to `sha256(body)` where the transport
carries no per-message id (Redis list, Redis pub/sub). Todo creation MUST dedup on
`(queue, idempotency_key)` among non-terminal rows, returning the existing todo when a redelivery
matches. The key MUST namespace the adapter instance and source (e.g. `redis:{stream}:{entry-id}`) so
keys from different sources cannot collide.

#### Scenario: Redis stream entry id becomes the idempotency key

- **WHEN** a message is consumed from a Redis stream
- **THEN** the idempotency key is derived as `redis:{stream}:{entry-id}` and used to dedup redeliveries
  into one todo

#### Scenario: List/pub-sub message uses a body hash

- **WHEN** a message is consumed from a Redis list or pub/sub channel (no per-message id)
- **THEN** the idempotency key is derived as `redis:{name}:sha256(body)`

### Requirement: Adapter Interface and Trust Mode

Every pull adapter MUST expose a uniform interface that consumes from its broker and feeds messages
into the shared back half (`verify → derive idempotency key → normalize → create todo (dedup) →
persist event`), so a push delivery and a pull delivery of the same logical event produce the
identical todo shape. A pull-ingested event MUST persist with `family='queue'`, `trust_mode='queue'`,
`verified=false`, and a `verify_detail` naming the broker/ACL identity (e.g. `redis acl: deploy-bot`).
Pull adapters MUST be registered in the `adapters` table with `family='queue'` and MUST honor the
runtime `enabled` flag.

#### Scenario: Pull event carries queue trust metadata

- **WHEN** any pull adapter enqueues a consumed message
- **THEN** the persisted event has `family='queue'`, `trust_mode='queue'`, `verified=false`, and a
  `verify_detail` naming the broker/ACL identity

#### Scenario: Disabled adapter does not consume

- **WHEN** an adapter's `adapters.enabled` flag is false
- **THEN** the adapter's poll loop MUST NOT consume from its broker

### Requirement: Redis Reference Transport Modes

The Redis reference adapter MUST support consuming via an ack-capable mode for durable work and MUST
prefer it. Streams with consumer groups (`XREADGROUP` then `XACK` after store) is the RECOMMENDED
mode; a reliable list (`BRPOPLPUSH` onto a processing list, `LREM` after store) is an acceptable
alternative. Pub/sub MAY be supported but MUST be documented as fire-and-forget with no redelivery,
and MUST NOT be used for work where message loss is unacceptable.

#### Scenario: Stream consumer group acks after store

- **WHEN** the Redis adapter runs in streams + consumer-group mode
- **THEN** it reads via `XREADGROUP`, stores the todo, and issues `XACK` only after the todo is
  durably stored

#### Scenario: Pub/sub is loss-tolerant only

- **WHEN** the Redis adapter is configured for pub/sub
- **THEN** it is documented as having no redelivery, and MUST NOT be selected for durable work

### Requirement: Poll-Loop Lifecycle — Concurrency Safety

Each pull adapter MUST run as an explicitly-managed worker with a clean startup and a graceful
shutdown. The poll loop MUST propagate a context for cancellation/timeout: on shutdown signal the loop
MUST stop consuming new messages, MUST finish or safely abandon (leave un-acked) any in-flight
message, and MUST return without leaking goroutines. Shared state across the loop MUST be race-safe via
synchronization or message passing, and the race detector MUST be enabled in CI. A broker connection
error MUST NOT crash the process; the loop MUST back off and retry.

#### Scenario: Graceful shutdown stops consuming and abandons in-flight safely

- **WHEN** the adapter's context is cancelled (shutdown)
- **THEN** the poll loop stops consuming, leaves any in-flight-but-unstored message un-acked (so it
  redelivers), and returns without leaking goroutines

#### Scenario: Broker error backs off, not crashes

- **WHEN** the broker connection fails transiently
- **THEN** the poll loop logs the error, backs off, and retries rather than terminating the process

### Requirement: Enqueue Consumed Message as Todo — Database Operation Standards

An accepted pull message MUST create a durable todo via the shared back half using parameterized
queries only (no string interpolation). The event insert and todo creation that together define the
durability boundary MUST use explicit connection lifecycle with timeouts, and MUST be structured so a
partial failure never acks the source message. Where the design couples event-insert and todo-create
as one atomic unit, they MUST run in a single transaction; otherwise the ack MUST be gated strictly on
the todo's confirmed durable persistence.

#### Scenario: Todo persisted with parameterized queries before ack

- **WHEN** a consumed message is normalized
- **THEN** the event and todo are written with parameterized queries under a bounded-timeout
  connection, and the source ack is issued only after the todo write is confirmed committed

### Requirement: Error Handling Standards

Adapter errors MUST be wrapped with context at each boundary (consume, normalize, insert event, create
todo, ack) and MUST NOT be silently swallowed. Domain failures SHOULD use sentinel errors, and all
errors MUST be logged with structured context (adapter, broker, message id — never broker credentials).
A failure after consume but before ack MUST result in redelivery, never a silently dropped message.

#### Scenario: Store failure leaves message for redelivery

- **WHEN** todo creation fails after a message is consumed
- **THEN** the error is wrapped and logged with structured context, the source message is not acked,
  and the broker redelivers it
