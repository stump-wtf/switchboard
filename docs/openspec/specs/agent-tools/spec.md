---
status: approved
date: 2026-07-06
implements: [ADR-0012, ADR-0005]
requires: [SPEC-0005, SPEC-0003]
---

# SPEC-0006: Agent-Facing MCP Tool Set

## Overview

Switchboard vends a scoped MCP/HTTP endpoint to each agent
([ADR-0012](../../../adrs/ADR-0012-agents-self-manage-webhooks.md),
[ADR-0005](../../../adrs/ADR-0005-mcp-tool-and-resource-contract.md)). This capability defines the
agent-facing *work* surface exposed on that endpoint — the verbs an agent actually uses to do its
job: draining the durable todo queue (`list_todos`, `claim`, `complete`, `fail`, plus lease
`heartbeat`), and self-managing its own ingestion sources within a human-vended ceiling
(`create_webhook`, `list_webhooks`, `rotate_webhook`, `delete_webhook`).

Every request is authenticated by a bearer credential that resolves to an endpoint and its immutable
scope (`internal/agentapi/agentapi.go`). Scope is enforced at the boundary: a verb outside the
endpoint's `verbs` allowlist, or a queue outside its `queues` grant, is refused with `forbidden`
before any state changes. Webhook self-management operates strictly within a per-endpoint ceiling
(max count, allowed source types, allowed target queues); switchboard mints and hashes the signing
secret and owns verification and idempotency — the agent receives only the ingest URL, never the
secret. This surface is distinct from, but complements, the shared read-only event-history contract
of [SPEC-0005](../mcp-tools/spec.md); it depends on that contract's shapes/errors and on the trust
model of [SPEC-0003](../../../adrs/ADR-0003-per-provider-ingestion-and-trust-model.md).

## Requirements

### Requirement: Bearer-Scoped Endpoint Authentication

Every request to the vended agent surface MUST carry a bearer credential in the `Authorization`
header. A missing credential MUST be rejected with `unauthenticated` (HTTP 401). A credential that
does not resolve to a live endpoint (revoked or unknown) MUST be rejected with `unauthenticated`. The
resolved endpoint's scope (`queues`, `verbs`) MUST be treated as immutable for the life of the
request; the agent MUST NOT be able to widen its own scope through any verb.

#### Scenario: Missing credential is rejected

- **WHEN** a request arrives with no bearer credential
- **THEN** the server MUST respond `unauthenticated` and MUST NOT perform any read or mutation

#### Scenario: Revoked credential is rejected

- **WHEN** a request presents a credential whose endpoint has been revoked
- **THEN** the server MUST respond `unauthenticated`

### Requirement: Scope Enforcement at the Boundary

Before executing any verb, the server MUST verify the verb is in the endpoint's allowlist and, for
todo-targeting verbs, that the todo's queue is in the endpoint's granted queues. A verb outside the
allowlist MUST be refused with `forbidden`. A queue filter or a target todo whose queue is outside
the grant MUST be refused with `forbidden`. These checks MUST occur before any state-changing
operation.

#### Scenario: Verb not in allowlist

- **WHEN** an endpoint whose allowlist lacks `complete` calls `complete`
- **THEN** the server MUST respond `forbidden` and MUST NOT transition the todo

#### Scenario: Todo outside granted queues

- **WHEN** an endpoint calls `claim` on a todo whose queue is not in the endpoint's granted queues
- **THEN** the server MUST respond `forbidden` and MUST NOT claim the todo

### Requirement: Todo Drain Verbs

The surface MUST expose `list_todos`, `claim`, `complete`, and `fail`. `list_todos` MUST return
compact todo rows filtered to the endpoint's granted queues (optionally narrowed by a `queue` and/or
`state` filter within scope) with a bounded `limit` (default 50). `claim` MUST atomically transition
a `pending` todo to `claimed`, acquiring a lease with a caller-supplied or default TTL, and MUST fail
with `conflict` if the todo is not in the expected claimable state. `complete` MUST transition a
todo the endpoint holds to `done`. `fail` MUST transition a leased todo back to `pending` if attempts
remain, otherwise dead-letter it to `failed`. The todo owner MUST be recorded as the acting agent
identity (`agent:<agent_id>`). A todo returned or transitioned MUST carry at minimum `id`, `queue`,
`state`, and `attempt`.

#### Scenario: Claim races resolve to one winner

- **WHEN** two endpoints call `claim` on the same `pending` todo concurrently
- **THEN** exactly one MUST succeed (todo → `claimed`) and the other MUST receive `conflict`

#### Scenario: Fail retries until attempts are exhausted

- **WHEN** `fail` is called on a leased todo and retry attempts remain
- **THEN** the todo MUST return to `pending` with an incremented `attempt`; when no attempts remain
  it MUST transition to `failed`

#### Scenario: List is confined to granted queues

- **WHEN** `list_todos` is called with no `queue` filter
- **THEN** it MUST return only todos in the endpoint's granted queues

### Requirement: Lease Lifecycle and Crash Safety

`claim` MUST acquire a time-bounded lease. The surface SHOULD expose `heartbeat` to extend a lease
the endpoint holds and MAY expose `release` to voluntarily requeue. A todo whose lease expires
without completion MUST be reclaimable — a background reaper MUST requeue expired-lease todos (or
dead-letter them when attempts are exhausted) so that a crashed agent does not strand work. Lease
extension and completion MUST be permitted only to the endpoint that holds the lease.

#### Scenario: Expired lease is requeued

- **WHEN** an agent claims a todo and then crashes without completing it, and the lease TTL elapses
- **THEN** the reaper MUST return the todo to `pending` (or `failed` if attempts are exhausted) so
  another agent can claim it

#### Scenario: Heartbeat extends only the holder's lease

- **WHEN** an endpoint that does not hold a todo's lease calls `heartbeat` on it
- **THEN** the server MUST refuse it (`forbidden` or `conflict`) and MUST NOT extend the lease

### Requirement: Webhook Self-Management Within a Vended Ceiling

The surface MUST expose `create_webhook`, `list_webhooks`, `rotate_webhook`, and `delete_webhook`,
bounded by the endpoint's ceiling: maximum webhook count, allowed source types, and allowed target
queues ([ADR-0012](../../../adrs/ADR-0012-agents-self-manage-webhooks.md)). `create_webhook` MUST be
refused with `ceiling_exceeded` when it would exceed the max count, `forbidden_source_type` for a
disallowed source type, and `forbidden` for a target queue outside the grant. `create_webhook` and
`rotate_webhook` MUST return the ingest URL (and `trust_mode`) and MUST NOT return the signing secret.
`list_webhooks` MUST return webhook metadata plus the ceiling (max, allowed source types, allowed
queues, used) and MUST NOT return secret values. `delete_webhook` MUST tear down the webhook.

#### Scenario: Create beyond the count ceiling is refused

- **WHEN** an endpoint at its max webhook count calls `create_webhook`
- **THEN** the server MUST respond `ceiling_exceeded` and MUST NOT create a webhook

#### Scenario: Disallowed source type is refused

- **WHEN** an endpoint whose ceiling allows only `github`/`generic` calls `create_webhook` with
  source type `stripe`
- **THEN** the server MUST respond `forbidden_source_type` and MUST NOT create a webhook

### Requirement: Switchboard Owns Secrets, Verification, and Idempotency

For any self-created webhook, switchboard MUST mint the signing secret and store it **hashed** in
PostgreSQL; the agent MUST NOT receive or be able to read the secret. A self-created signed-type
webhook MUST be verified exactly as [SPEC-0003](../../../adrs/ADR-0003-per-provider-ingestion-and-trust-model.md)
mandates; the agent MUST NOT be able to downgrade its trust mode, disable signature checks, or alter
verification. Duplicate deliveries to a self-created webhook MUST dedup into a single todo on the
idempotency key. `rotate_webhook` MUST mint a new secret and retire the old one.

#### Scenario: Secret is never returned to the agent

- **WHEN** an agent calls `create_webhook` or `rotate_webhook`
- **THEN** the response MUST contain the ingest URL and MUST NOT contain the signing secret; the
  stored secret MUST be hashed

#### Scenario: Trust mode cannot be downgraded by the agent

- **WHEN** an agent attempts to create or alter a `signed`-type webhook to weaken its verification
- **THEN** switchboard MUST verify the webhook per SPEC-0003 regardless and MUST NOT honor any
  agent-supplied trust downgrade

### Requirement: Structured Output and Stable Error Shape

Every verb MUST accept a declared input shape and return structured output. Errors MUST use a stable
machine `code` plus a human `message` that MUST NOT contain secret material. The defined codes are:
`unauthenticated` (missing/invalid credential), `forbidden` (verb not in allowlist, or queue/target
outside grant), `conflict` (lost claim race / wrong state), `not_found` (unknown todo/webhook id),
`invalid_argument` (bad input), `ceiling_exceeded` / `forbidden_source_type` (webhook ceiling), and
`internal` (unexpected server-side failure).

#### Scenario: Unknown todo id raises not_found

- **WHEN** a transition verb targets a todo id that does not exist
- **THEN** the server MUST respond `not_found`

### Requirement: Error Handling Standards

Errors crossing a boundary (store, outbound HTTP, request decode) MUST be wrapped with context and
MUST NOT be silently swallowed. Domain failures MUST be surfaced as sentinel errors (e.g. not-found,
conflict) and mapped to the stable error codes above; unexpected failures MUST map to `internal` and
be logged with structured context. A store or transport failure MUST NOT leak internal detail or
secret material into the client-visible `message`.

#### Scenario: Store failure maps to internal and is logged

- **WHEN** a store operation returns an unexpected error during a verb
- **THEN** the server MUST respond `internal`, MUST log the underlying error with context, and MUST
  NOT expose the underlying error text to the client

### Requirement: Concurrency Safety

The endpoint MUST propagate the request context for cancellation and timeout into all store and
outbound operations. The new-todo fan-out stream MUST be a best-effort doorbell that never blocks a
producer and never gates correctness — the durable todo queue remains the ledger. The background
lease reaper MUST have an explicit lifecycle (clean startup, graceful shutdown on context
cancellation) and MUST be race-safe. Concurrent claims MUST be resolved atomically by the store, not
by application-level locking.

#### Scenario: Slow stream consumer never blocks producers

- **WHEN** a subscribed agent's stream buffer is full while new todos are created
- **THEN** the server MUST drop the pushed notification for that subscriber rather than block, and
  the todo MUST remain durably in the queue for the agent to claim

#### Scenario: Graceful shutdown stops the reaper

- **WHEN** the process receives a shutdown signal (request context cancelled)
- **THEN** the reaper MUST stop cleanly without leaking goroutines

### Requirement: Database Operation Standards

Multi-step atomic mutations (claim-with-lease, complete, fail-with-retry-or-dead-letter) MUST execute
under a transaction or an equivalent atomic conditional update so a todo cannot be left in an
inconsistent state. All queries MUST be parameterized; string interpolation of user-supplied values
into SQL MUST NOT be used. Connections MUST have explicit lifecycle and timeouts via the propagated
context.

#### Scenario: Claim is atomic

- **WHEN** `claim` transitions a todo from `pending` to `claimed`
- **THEN** the state change and lease assignment MUST be applied atomically so no partial state is
  observable

## Security Requirements

### Authentication

Every endpoint on this surface is served over HTTP and MUST require authentication by default via a
bearer credential resolved to a scoped endpoint. There are no public verbs on this surface.

| Endpoint | Auth | Justification |
|----------|------|---------------|
| `GET /agent/whoami` | Required | Returns the caller's identity + scope. |
| `GET /agent/todos` (`list_todos`) | Required | Exposes queue contents. |
| `POST /agent/todos/{id}/claim` (`claim`) | Required | State-changing lease acquisition. |
| `POST /agent/todos/{id}/complete` (`complete`) | Required | Terminal state transition. |
| `POST /agent/todos/{id}/fail` (`fail`) | Required | State transition / dead-letter. |
| `GET /agent/stream` (new-todo doorbell) | Required | Streams queue activity in scope. |
| `create_webhook` / `list_webhooks` / `rotate_webhook` / `delete_webhook` | Required | Manages ingestion sources within the vended ceiling. |

### Rate Limiting

Webhook self-management verbs (`create_webhook`, `rotate_webhook`, `delete_webhook`) MUST be
rate-limited per endpoint to prevent noisy churn within the ceiling. Todo drain verbs SHOULD be
rate-limited per endpoint to bound store load. A per-endpoint token bucket is RECOMMENDED; the ceiling
count cap is a hard secondary bound on webhook creation.

### Security Headers

All HTTP responses on this surface MUST include:
- `Content-Security-Policy`: `default-src 'none'` (JSON/SSE API, not browser assets)
- `X-Frame-Options`: DENY
- `X-Content-Type-Options`: nosniff
- `Referrer-Policy`: strict-origin-when-cross-origin

### Request Body Size Limits

All verbs accepting request bodies (claim/complete/fail, webhook create/rotate) MUST bound them with
`http.MaxBytesReader`. Default limit: **256 KiB** per request. Oversized bodies MUST be rejected
before decoding.

### CSRF Protection

This surface is a machine-to-machine bearer-authenticated JSON/SSE API, not a cookie-authenticated
browser session; classic form CSRF does not apply. State-changing verbs MUST authenticate via the
bearer credential and MUST NOT be reachable through ambient cookie authentication. No HTML forms are
served here.

### Redirect Validation

Agents supply webhook `target_queue` and `source_type`, not outbound redirect targets — this surface
issues no user-supplied outbound redirects. The `ingest_url` returned by `create_webhook` is minted by
switchboard, not supplied by the agent, so there is no open-redirect or SSRF vector on this surface.
(The read-only event-history surface's `replay` target validation lives in
[SPEC-0005](../mcp-tools/spec.md).)

## Cross-References

- Self-management split & ceiling: [ADR-0012](../../../adrs/ADR-0012-agents-self-manage-webhooks.md).
- Contract shape / structured output / errors: [ADR-0005](../../../adrs/ADR-0005-mcp-tool-and-resource-contract.md), [SPEC-0005](../mcp-tools/spec.md).
- Trust model switchboard enforces regardless of who created the webhook: [SPEC-0003 / ADR-0003](../../../adrs/ADR-0003-per-provider-ingestion-and-trust-model.md).
- Design & architecture for this capability: [design.md](design.md).
