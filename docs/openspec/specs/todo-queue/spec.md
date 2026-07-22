---
status: amended
date: 2026-07-21
amends: [ADR-0007]
implements: [ADR-0007, ADR-0022]
requires: [SPEC-0004]
---

# SPEC-0003: Todo Work-Queue

## Overview

This capability defines switchboard's core agent-facing primitive: the **todo**, a durable
work-item with an explicit lifecycle (`pending → claimed → done | failed`, with `retry` as a
transition back to `pending`). It realizes
[ADR-0007](../../../adrs/ADR-0007-todos-as-core-primitive.md), which established that the object an
agent interacts with is not a message it reads once but a durable unit of work that survives crashes,
deduplicates at-least-once ingestion, and is safe under concurrent consumers.

The queue mechanics — atomic claim, SQS-style visibility windows, lease heartbeat, lease-expiry
requeue, bounded retries, and idempotent enqueue — are backed by the PostgreSQL persistence layer
([SPEC-0004](../persistence/spec.md)); this capability depends on it. The model is deliberately an
exact analogue of Amazon SQS's visibility-timeout mechanism: `claim` ≈ `ReceiveMessage`,
`heartbeat` ≈ `ChangeMessageVisibility`, `complete` ≈ `DeleteMessage`, and lease expiry ≈ the
message reappearing on the queue.

The reference implementation is `internal/store/todos.go` (the `Todo` type, `CreateTodo`,
`ClaimTodo`, `ClaimNext`, `CompleteTodo`, `FailTodo`, `ListTodos`, `GetTodo`, `ReapExpired`,
`RequeueDueRetries`) over the `todos` table in `internal/db/migrations/0001_init.sql` (plus the
`next_retry_at` retry-window column from `0008_todo_retry_backoff.sql`).

## Requirements

### Requirement: Todo Lifecycle State Machine

A todo MUST occupy exactly one of four states: `pending`, `claimed`, `done`, `failed`. `done` and
`failed` are terminal (a `failed` todo with an open retry window is terminal-unless-retried: it
re-enters `pending` only through the scheduled backoff elapsing or an explicit retry). `retry` is a
transition, not a state — a failed or lease-expired todo transitions back to `pending` with
`attempt` incremented on the next claim. The permitted transitions are:

- `create → pending`
- `pending → claimed` (atomic claim, sets owner + lease + `claimed_at`, `attempt++`)
- `claimed → claimed` (heartbeat extends the lease)
- `claimed → done` (complete)
- `claimed → failed` (fail: below `max_attempts` the todo parks in `failed` with a scheduled
  retry window (`next_retry_at`, exponential backoff); at the cap it dead-letters with no window)
- `claimed → pending | failed` (lease expiry: requeue while `attempt < max_attempts`, else
  dead-letter)
- `failed → pending` (scheduled-backoff retry once `next_retry_at` elapses — via the retry
  scheduler or a due claim — or an explicit operator/agent retry)

Any transition not in this set MUST be rejected. Terminal todos MUST persist as audit records subject
to retention ([SPEC-0004](../persistence/spec.md)).

#### Scenario: Only defined transitions are honored

- **WHEN** a caller attempts to complete a todo that is `pending` (never claimed)
- **THEN** the store MUST NOT mark it `done`; the conditional update matches no row and the caller
  MUST receive a conflict signal (`ErrConflict`)

#### Scenario: Terminal states are final

- **WHEN** a todo is `done`, or `failed` with no open retry window
- **THEN** it MUST NOT be claimed again by a normal claim, and it MAY only re-enter `pending` through
  an explicit operator/agent retry of a dead-lettered (`failed`) todo (a `failed` todo whose
  `next_retry_at` window is open additionally re-enters `pending` when that window elapses)

### Requirement: Atomic Claim with FOR UPDATE SKIP LOCKED

Claiming a todo MUST be atomic and single-owner. When many workers drain the same pool queue, each
concurrent claimant MUST receive a *different* pending row and MUST NOT block on another — the store
MUST select the next pending row with `FOR UPDATE SKIP LOCKED` ordered by `created_at`, then set
`state='claimed'`, `owner`, `lease_expires_at = now() + ttl`, `claimed_at = now()`, and increment
`attempt`. A claim MUST respect assignment: a directly-assigned todo (`assignee` set) MUST be
claimable only by that assignee; a pool todo (`assignee IS NULL`) MAY be claimed by any worker whose
scope covers the queue. Claiming a specific todo that exists but is not claimable (already claimed, or
wrong assignee) MUST return `ErrConflict`; claiming a nonexistent todo MUST return `ErrNotFound`;
claiming when no pending work exists MUST return `ErrNotFound`.

#### Scenario: Concurrent claimants get distinct todos

- **WHEN** N workers call claim on the same queue simultaneously and N pending todos exist
- **THEN** each worker MUST receive a distinct todo, no todo MUST be claimed twice, and the claims
  MUST NOT serialize behind a global lock

#### Scenario: Direct assignment is respected

- **WHEN** a todo has `assignee` set and a worker that is not that assignee attempts to claim it
- **THEN** the claim MUST fail (the row is not selected / `ErrConflict`), and only the named assignee
  MUST be able to claim it

### Requirement: Visibility Window, Lease, and Heartbeat

Claiming MUST behave as an SQS-style receive with a visibility timeout: for the duration of the
lease (`lease_expires_at`), the todo MUST be invisible to every other consumer. A worker that needs
more time than its window MUST be able to extend the lease via a heartbeat
(`ChangeMessageVisibility`) that updates `lease_expires_at`; only the current lease owner MAY
heartbeat. `complete` and `fail` MUST likewise be permitted only for the current lease owner
(guarded by `state='claimed' AND owner=$owner`). The lease TTL MUST be set per claim, defaulting to
a configurable server value.

#### Scenario: Claimed todo is invisible to other consumers

- **WHEN** a todo is `claimed` with a live (unexpired) lease
- **THEN** no other consumer MUST be able to claim it until the lease expires or the owner releases
  it

#### Scenario: Only the lease owner may complete or fail

- **WHEN** a worker that is not the current `owner` attempts to complete or fail a claimed todo
- **THEN** the transition MUST NOT apply, and the caller MUST receive `ErrConflict` (row exists but
  not owned)

### Requirement: Lease-Expiry Requeue and Reaper (Crash Safety)

A todo whose lease lapses before completion MUST become re-claimable — this is the crash-safety
guarantee. A periodic reaper MUST scan for todos in `state='claimed'` with `lease_expires_at < now()`
and requeue them: if `attempt < max_attempts` the todo MUST return to `pending` with `owner` and
`lease_expires_at` cleared; if `attempt >= max_attempts` it MUST dead-letter to `failed`. Requeue
MUST NOT be lost merely because no reaper has run yet — a lapsed lease MUST also be safe to detect on
the next claim scan. Because a crashed worker's todo is re-delivered, processing is **at-least-once**
and consumers MUST be idempotent.

#### Scenario: Crashed worker's todo is re-claimable

- **WHEN** a worker claims a todo and dies without completing it, and the lease then expires
- **THEN** the reaper MUST return the todo to `pending` (below the attempt cap) so another worker can
  claim it, and no work MUST be silently dropped

#### Scenario: Repeated expiry dead-letters at the cap

- **WHEN** a todo has been claimed and expired such that `attempt >= max_attempts`
- **THEN** the reaper MUST move it to `failed` (dead-letter) rather than requeuing it forever

### Requirement: Bounded Retries via max_attempts

Every todo MUST carry an `attempt` counter (incremented on each claim) and a `max_attempts` cap
(default 5). On `fail`, if `attempt < max_attempts` the todo MUST transition to `failed` with a
scheduled retry window: `next_retry_at = now() + backoff(attempt)`, where the backoff grows
exponentially from a 30-second base, doubling per attempt and capped at 15 minutes (attempt 1 →
30s, 2 → 1m, 3 → 2m, 4 → 4m, 5 → 8m, 6+ → 15m). While the window is open the todo MUST NOT be
claimable. Once `next_retry_at` elapses the todo MUST return to `pending`: a background retry
scheduler MUST re-queue due retries (clearing `owner`/`lease_expires_at`/`next_retry_at`), and the
claim scan MAY claim a due retry directly so re-queue latency never depends on the scheduler tick.
If `attempt >= max_attempts` the todo MUST transition to `failed` with no retry window
(dead-letter). Retries MUST NOT loop unbounded. A dead-lettered todo MAY be re-queued only by an
explicit operator/agent retry; the same explicit retry MAY also override an open retry window,
re-queuing immediately.

#### Scenario: Fail below cap schedules a backoff retry, at cap dead-letters

- **WHEN** the owner fails a claimed todo whose `attempt < max_attempts`
- **THEN** the todo MUST park in `failed` with `lease_expires_at` cleared and `next_retry_at`
  stamped, and MUST NOT be claimable before `next_retry_at`; **but WHEN** `attempt >=
  max_attempts`, it MUST transition to `failed` with `next_retry_at` NULL

#### Scenario: Elapsed backoff re-queues

- **WHEN** a failed todo's `next_retry_at` elapses
- **THEN** the retry scheduler MUST return it to `pending` with `owner`, `lease_expires_at`, and
  `next_retry_at` cleared, and a claim arriving after the window MAY take it directly

### Requirement: Idempotent Enqueue and Dedup

Creating a todo MUST be idempotent on `(endpoint_id, idempotency_key)` among LIVE rows (see
"Per-Endpoint Idempotency and Dedup" for the full per-endpoint semantics; this requirement
retains the original lifecycle definition of a LIVE row). A row is live
when it is not `done` and not a true dead-letter — i.e. `state <> 'done' AND (state <> 'failed' OR
next_retry_at IS NOT NULL)`: a parked retry (failed with an open backoff window) keeps its dedup
slot, so a redelivery during the window collapses onto it rather than minting a duplicate active
todo (which the re-queue transition would then collide with under the unique index). When a
producer creates a todo whose `(queue, idempotency_key)` already matches a live todo, the store
MUST NOT create a second row — it MUST return the existing todo and signal that no new row was
created. This MUST be implemented with an atomic `INSERT … ON CONFLICT … DO NOTHING` against the
partial unique dedupe index (whose predicate MUST match the live-row definition above), so that
at-least-once ingestion (e.g. a webhook delivered twice with the same provider delivery id)
collapses into exactly one work-item. A null `idempotency_key` MUST NOT participate in dedup.

#### Scenario: Duplicate delivery collapses to one todo

- **WHEN** two create calls arrive with the same `queue` and `idempotency_key` while the first todo
  is still live (pending, claimed, or a parked retry)
- **THEN** exactly one todo MUST exist, the second call MUST return that same todo, and the call MUST
  report that no new row was created

#### Scenario: A terminal todo does not block a new one

- **WHEN** a create arrives with a `(queue, idempotency_key)` that matches only a `done` todo or a
  dead-lettered `failed` todo (no retry window)
- **THEN** a new `pending` todo MUST be created (terminal rows are excluded from the dedupe index)

#### Scenario: Redelivery during a backoff window does not duplicate

- **WHEN** a create arrives with a `(queue, idempotency_key)` matching a `failed` todo whose
  `next_retry_at` window is open
- **THEN** no new row MUST be created — the parked retry MUST be returned as the existing live
  todo, and its later re-queue (scheduler, due claim, or manual retry) MUST succeed without a
  uniqueness violation

### Requirement: Concurrency Safety of Queue Workers

The reaper and any background pollers MUST propagate a `context.Context` for cancellation and
timeout, MUST have an explicit lifecycle (clean startup and graceful shutdown), and MUST achieve race
safety through the database's atomic transitions (`FOR UPDATE SKIP LOCKED`, conditional `UPDATE …
WHERE state=… AND owner=…`) rather than in-process locking of shared todo state. The race detector
MUST be enabled in CI for the queue packages.

#### Scenario: Graceful shutdown drains cleanly

- **WHEN** the service receives a shutdown signal while workers hold leases
- **THEN** the reaper/pollers MUST stop on context cancellation, in-flight leased todos MUST simply
  expire and be requeued by crash safety, and no todo MUST be left in an inconsistent state

### Requirement: Error Handling Standards

Queue operations MUST surface domain failures as sentinel errors — `ErrNotFound` for an absent todo,
`ErrConflict` for a lost state-transition race — and MUST NOT silently swallow them. After a
conditional update that affects zero rows, the store MUST distinguish "row absent" (`ErrNotFound`)
from "row present but not in the expected state / not owned" (`ErrConflict`). Errors crossing the
store boundary MUST be wrapped with context or returned as typed sentinels, and MUST be
observable via structured logging.

#### Scenario: Zero-row update is classified, not swallowed

- **WHEN** a claim/complete/fail conditional update affects no rows
- **THEN** the store MUST look up whether the todo exists and return `ErrConflict` if it does or
  `ErrNotFound` if it does not — never a nil error with an empty todo

### Requirement: Endpoint Ownership (Tenant Isolation)

Governing: ADR-0022. Every todo MUST carry a non-null `endpoint_id` foreign key to the
`endpoints` table, pinning it to exactly one vended MCP endpoint for its entire lifecycle.
The endpoint's owning human ([ADR-0008](../../../adrs/ADR-0008-human-principal-vended-endpoints.md))
is the todo's tenant. Every CALLER-FACING todo query — `ListTodos`, `ClaimTodo`,
`ClaimNext`, `GetTodo`, `CompleteTodo`, `FailTodo`, `Heartbeat`, `ReleaseTodo`,
`RetryTodo`, `PendingDoorbellTodos` — MUST predicate on `endpoint_id`, accepting it from
the authenticated endpoint context. A query scoped to endpoint A MUST NEVER return a todo
owned by endpoint B, even when both endpoints share a queue name (e.g. both target
`"github"`). Queue name is a human-readable label and a secondary intra-endpoint filter;
it is NOT a tenant boundary.

An absent or malformed `endpoint_id` MUST behave as a scope that owns nothing —
`ErrNotFound` for single-row and mutation paths, an empty result for list paths — and MUST
NOT surface as a database type error. Otherwise the error shape itself distinguishes a
broken scope from an empty one on the very paths whose purpose is to make foreign and
nonexistent indistinguishable.

**Operator twins.** Each caller-facing function MAY have an explicitly named
`*AnyEndpoint` counterpart (`GetTodoAnyEndpoint`, `ClaimTodoAnyEndpoint`,
`ReleaseTodoAnyEndpoint`, …) for the operator Board, which is authenticated by the owning
human's session rather than an endpoint credential and legitimately sees every tenant. The
agent-facing path MUST NOT call an `*AnyEndpoint` variant.

**Background sweeps are exempt.** `ReapExpired` and `RequeueDueRetries` MUST NOT predicate
on `endpoint_id`. They are timer-driven, system-wide maintenance with no authenticated
endpoint context to scope TO — no caller, no credential, no tenant. They return no todo to
any principal and move each row only within its own lifecycle (expired lease → pending or
dead-letter; elapsed backoff → pending), so they can neither disclose nor transfer work
across tenants; the wakeups they emit are already scoped by each row's own `endpoint_id`.
Requiring a scope would make crash recovery and retry depend on someone being logged in,
and scoping by iteration would reproduce an identical result set more slowly. The tenant
boundary is enforced where work is READ and CLAIMED, not where it ages.

The channel doorbell (`PublishTodoReady`, SPEC-0011) MUST filter sessions by `endpoint_id`
as the primary scope check: a session minted under endpoint A MUST NEVER receive a
doorbell for a todo owned by endpoint B. Queue membership remains as a secondary filter
within an endpoint's grant, preserving SPEC-0011 "Scope-Filtered Fan-Out" at a finer grain.

#### Scenario: Cross-endpoint queue collision is isolated

- **WHEN** human A's endpoint and human B's endpoint are both scoped to queue `"reviews"`
  and a delivery creates a todo pinned to human A's endpoint
- **THEN** human B's endpoint's `list_todos` MUST NOT return that todo, human B's agent
  MUST NOT be able to `claim` it, and no doorbell for it MUST reach human B's session

#### Scenario: Doorbell never crosses endpoints

- **WHEN** a todo owned by endpoint A transitions to ready and a session for endpoint B
  (same queue name) is attached
- **THEN** endpoint B's session MUST receive no notification for that todo

#### Scenario: A foreign todo id is indistinguishable from a nonexistent one

- **WHEN** endpoint B calls a scoped transition (`claim`, `release`, `retry`, …) with the
  id of a todo owned by endpoint A, and separately with an id that was never minted
- **THEN** both MUST return `ErrNotFound` — never `ErrConflict` for the foreign id, which
  would let B probe which ids A owns

#### Scenario: Background sweep spans tenants without a scope

- **WHEN** the lease reaper or retry scheduler runs while todos of several endpoints are
  eligible
- **THEN** every eligible todo MUST be swept regardless of endpoint, and each resulting
  wakeup MUST name the owning endpoint of the row that moved

### Requirement: Per-Endpoint Idempotency and Dedup

Governing: ADR-0022. Creating a todo MUST be idempotent on `(endpoint_id, idempotency_key)`
among LIVE rows — `state <> 'done' AND (state <> 'failed' OR next_retry_at IS NOT NULL)`.
The dedup namespace is per-endpoint, not global, so two endpoints that happen to share an
idempotency key (e.g. the same GitHub delivery id routed to two endpoints) each retain
their own todo without collapsing onto each other. Within one endpoint, a redelivery
during a backoff window still collapses onto the parked retry, preserving the existing
contract. A null `idempotency_key` MUST NOT participate in dedup.

#### Scenario: Same delivery routed to two endpoints yields two todos

- **WHEN** a single webhook delivery fans out to endpoints A and B with the same
  idempotency key
- **THEN** exactly two todos are created (one pinned to A, one pinned to B), each
  independently deduped on its own `(endpoint_id, idempotency_key)` namespace

#### Scenario: Redelivery to one target dedups only within that target

- **WHEN** a webhook with routes to A and B receives the same delivery id twice
- **THEN** A and B each still hold exactly one todo (two total, not four); the second
  delivery collapses per-target onto the existing live row

### Requirement: Database Operation Standards

All queue state transitions MUST use parameterized SQL (no string interpolation of caller input) and
MUST be atomic single-statement conditional updates or transactions. The claim scan MUST be served by
the partial index on pending rows so it never scans the terminal backlog. Multi-step mutations MUST
be transactional. Every store function MUST accept and honor a `context.Context`.

#### Scenario: Claim never scans terminal rows

- **WHEN** the queue holds a large backlog of `done`/`failed` todos and a few `pending` ones
- **THEN** the claim scan MUST be served by the partial pending index and MUST NOT scan terminal rows
