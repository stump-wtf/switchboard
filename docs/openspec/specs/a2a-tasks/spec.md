---
status: draft
date: 2026-07-20
implements: [ADR-0021]
requires: [SPEC-0003, SPEC-0009]
extends: [SPEC-0010]
related: [SPEC-0011]
---

# SPEC-0018: A2A Task Delegation Transport

## Overview

This capability formalizes [ADR-0021](../../../adrs/ADR-0021-a2a-task-delegation-transport.md): switchboard
implements the native [A2A](https://a2a-protocol.org/latest/specification/) task RPC surface —
`SendMessage`, `GetTask`, `ListTasks`, `CancelTask`, `SendStreamingMessage`, `SubscribeToTask`, and
`GetExtendedAgentCard` — as a second wire protocol over the *same* authorized relationship MCP tools
already use. An A2A **Task is a projection of a Todo** ([SPEC-0003](../todo-queue/spec.md)), not a new
parallel domain object: `SendMessage` creates a todo via the same path as `create_for`, and every
durability property of the todo model (ownership, lease, dedup, retry, dead-letter) applies unchanged.
Internally the primitive keeps its existing name, `Todo` — "Task" is A2A's wire-boundary vocabulary for
the same object, not a rename of the Go type, database schema, or MCP tool names.

This spec **extends [SPEC-0010](../friending/spec.md)**: SPEC-0010's "Work Flows as Todos, Not A2A Tasks"
requirement is amended (see that spec's changelog note) to permit native A2A task intake — but only for a
caller that already holds a vended endpoint minted by SPEC-0010's friend-request-and-human-approval flow.
Nothing about how a grant is *acquired* changes; this spec only expands what a grant-holder can *do* with
it once acquired. It **requires** [SPEC-0009](../personas/spec.md) (Agent Cards, whose `capabilities.streaming`
flag this spec now sets `true`) and [SPEC-0003](../todo-queue/spec.md) (the Todo model these tasks project
onto). It is **related to** [SPEC-0011](../channels/spec.md): both fire from the same committed-transition
event, but Channels remains the internal, best-effort, MCP-native "doorbell" for switchboard-hosted
sessions, while this spec's streaming (`SubscribeToTask`) serves external A2A callers over SSE. Neither
replaces the other. The separate `PushNotificationConfig` webhook mechanism (external, authenticated,
durable HTTP delivery) is out of scope here and is formalized in its own spec.

> **Premise note.** This spec is `draft`, so its own requirements plainly describe work that is not
> built. What the status does *not* convey is that the **existing** path it repeatedly refers to is
> also unbuilt: MCP's `create_for` has a store backend but is registered by no MCP tool, so no vended
> endpoint can call it. Phrasing below such as "the same authorization path `create_for` already uses"
> describes an *intended* path, not a live one. Anything this spec inherits from that path has to be
> built rather than assumed.

## Requirements

### Requirement: SendMessage Requires a Vended Endpoint

`SendMessage` MUST create a task only when the caller presents a valid vended-endpoint bearer credential
([ADR-0008](../../../adrs/ADR-0008-human-principal-vended-endpoints.md)) scoped to the target queue —
the same authorization path MCP's `create_for` already uses. A caller without a valid credential MUST be
rejected identically to an unauthenticated MCP call. `SendMessage` MUST NOT create a task for a caller
that has not been through SPEC-0010's friend-request-and-approval flow (or an equivalent future onboarding
path minting the same kind of scoped credential); there MUST be no anonymous or cold-open task-creation
path.

#### Scenario: Unauthenticated SendMessage is rejected

- **WHEN** a caller invokes `SendMessage` without a valid vended-endpoint credential
- **THEN** the request MUST be rejected (401/403, matching A2A's authentication/authorization error
  codes) and no todo MUST be created

#### Scenario: Authorized SendMessage creates a todo

- **WHEN** a caller holding a vended endpoint scoped to queue `Q` invokes `SendMessage`
- **THEN** switchboard MUST create a todo in `Q` via the same path as `create_for`, and the response MUST
  be a `Task` object whose `id` is the todo's id and whose status is `submitted`

### Requirement: A Task Is a Todo, Not a Parallel Object

Every A2A task-shaped response (`Task`, `TaskStatus`) MUST be a direct projection of a single underlying
todo row; switchboard MUST NOT maintain a second, independent record of task state. A todo created via
`SendMessage` and a todo created via MCP's `create_for` MUST be indistinguishable in the store — same
table, same lease/retry/dedup/dead-letter semantics ([ADR-0007](../../../adrs/ADR-0007-todos-as-core-primitive.md)).

#### Scenario: A2A-created and MCP-created todos are indistinguishable

- **WHEN** one todo is created via `SendMessage` and another via `create_for` into the same queue
- **THEN** both MUST be claimable, leaseable, retryable, and auditable through the exact same store code
  path, with no field or behavior that distinguishes their origin

### Requirement: GetTask and ListTasks

`GetTask` MUST return the current projected state of a todo by id, and MUST support a `historyLength`
parameter: unset means no limit, `0` means no history is returned, and a positive value `N` means at most
the `N` most recent messages are returned. `ListTasks` MUST support filtering by status and by a
`contextId`-equivalent grouping key, and MUST use cursor-based pagination rather than offset pagination.
Both MUST be read-only and MUST NOT mutate todo state.

#### Scenario: historyLength bounds returned messages

- **WHEN** `GetTask` is called with `historyLength: 2` on a task with 5 recorded messages
- **THEN** the response MUST include at most the 2 most recent messages

#### Scenario: ListTasks paginates by cursor

- **WHEN** `ListTasks` is called with a page size smaller than the number of matching todos
- **THEN** the response MUST include a cursor for the next page, and repeated calls following the cursor
  MUST NOT skip or repeat a todo

### Requirement: CancelTask Is Idempotent

`CancelTask` MUST transition a todo out of its claimable/in-progress states into a terminal `canceled`
state. Calling `CancelTask` on an already-`canceled` or otherwise-terminal todo MUST succeed without error
and MUST NOT change the todo's terminal state (idempotent). `CancelTask` on a todo already claimed by a
worker MUST signal cancellation to that worker via the same commit-hook path used for doorbell/streaming
delivery.

#### Scenario: Cancel is a no-op on an already-terminal task

- **WHEN** `CancelTask` is called twice in a row on the same task
- **THEN** the second call MUST succeed and MUST leave the task's terminal state unchanged

### Requirement: Task State Machine Extension

The todo state machine ([ADR-0007](../../../adrs/ADR-0007-todos-as-core-primitive.md)) MUST gain four new
states to represent A2A semantics that `pending`/`claimed`/`done`/`failed` do not cover: `canceled`,
`rejected`, `input-required`, and `auth-required`. The existing states MUST map onto A2A's `TaskState`
as: `pending`→`submitted`, `claimed`→`working`, `done`→`completed`, `failed`→`failed`. `input-required`
and `auth-required` MUST be interrupt states that a claimed todo can enter and MUST return to `claimed`
when the required input or auth is supplied; `canceled` and `rejected` MUST be terminal states distinct
from `failed` (a `rejected` todo was never claimed; a `canceled` todo was explicitly canceled, not retried
out).

#### Scenario: Interrupt states return to claimed

- **WHEN** a claimed todo transitions to `input-required` and the required input is later supplied
- **THEN** the todo MUST transition back to `claimed`, not to `pending`, and MUST retain its existing
  owner and lease

#### Scenario: Rejected is distinct from failed

- **WHEN** a todo is rejected before ever being claimed (e.g., a policy check fails at intake)
- **THEN** it MUST transition to `rejected`, not `failed`, and MUST NOT be eligible for the retry/backoff
  behavior that applies to `failed` todos

### Requirement: Streaming via SendStreamingMessage and SubscribeToTask

`SendStreamingMessage` MUST establish an SSE stream that returns the initial `Task` or `Message`, followed
by zero or more status/artifact update events driven by the same committed-transition hook that feeds the
Channels doorbell (`store.SetTodoDoorbellHook`). `SubscribeToTask` MUST open the same kind of stream for
an existing task, MUST emit the current state immediately, and MUST close the stream when the task reaches
a terminal state. Both MUST return `UnsupportedOperationError` if the caller's endpoint scope does not
advertise the `streaming` capability.

#### Scenario: Stream closes on terminal state

- **WHEN** a subscribed task transitions to `done`, `failed`, `canceled`, or `rejected`
- **THEN** the stream MUST deliver that final event and then close; no further events MUST be sent for
  that task

#### Scenario: Streaming requires the capability flag

- **WHEN** a caller invokes `SubscribeToTask` against an endpoint whose scope does not include streaming
- **THEN** switchboard MUST return `UnsupportedOperationError` and MUST NOT open a stream

### Requirement: GetExtendedAgentCard

`GetExtendedAgentCard` MUST return an authenticated, richer variant of the persona's Agent Card
([SPEC-0009](../personas/spec.md)) to callers holding a valid vended-endpoint credential, and MUST NOT be
served to unauthenticated callers (unauthenticated callers get only the public discovery card).

#### Scenario: Extended card requires authentication

- **WHEN** an unauthenticated caller requests `GetExtendedAgentCard`
- **THEN** switchboard MUST reject the request and MUST NOT include any fields beyond what the public
  discovery Agent Card already exposes

### Requirement: Agent Card Advertises Streaming

A persona's Agent Card `capabilities.streaming` flag MUST be `true` once this capability is implemented,
replacing the prior hard-coded `false`. `capabilities.pushNotifications` is out of scope for this spec and
remains governed by the separate push-notifications spec.

#### Scenario: Card reflects real streaming support

- **WHEN** a persona's Agent Card is served after this capability is implemented
- **THEN** `capabilities.streaming` MUST be `true`, and `SubscribeToTask`/`SendStreamingMessage` MUST be
  callable by any endpoint holder that capability advertisement implies

### Requirement: Task-Creation Volume Rate Limiting

`SendMessage` MUST be rate-limited per vended endpoint using a token-bucket (or equivalent) strategy,
independent of and in addition to SPEC-0010's friend-request quota (which bounds *acquiring* a grant, not
*using* one). Exceeding the limit MUST return a rate-limit error (429 / A2A's rate-limit error shape) and
MUST NOT create a todo. The specific bucket size and refill rate are an operational tuning parameter, not
fixed by this spec, but a default MUST exist and MUST be documented in the design.

#### Scenario: Excess SendMessage calls are throttled

- **WHEN** a single vended endpoint exceeds its `SendMessage` rate limit
- **THEN** further `SendMessage` calls from that endpoint MUST be rejected with a rate-limit error until
  the window resets, and no todo MUST be created for the rejected calls

### Requirement: Error Handling Standards

All error-producing operations in this capability MUST follow structured error handling:

- Errors MUST be wrapped with contextual information at each layer boundary (e.g., "SendMessage failed:
  todo creation failed: constraint violation").
- Sentinel errors MUST be defined for A2A-specific failure modes (`UnsupportedOperationError`,
  `PushNotificationNotSupportedError` for calls that touch capability-gated behavior, rate-limit errors)
  so callers can distinguish them programmatically and map them to the correct A2A error codes.
- Silent error swallowing MUST NOT occur in the SendMessage/GetTask/ListTasks/CancelTask/streaming code
  paths — every error MUST be returned to the caller or logged with sufficient context.
- Structured logging MUST be used for error reporting (key-value pairs, not string interpolation).

#### Scenario: Unsupported operation maps to the correct A2A error

- **WHEN** a caller invokes a capability-gated operation their endpoint does not advertise
- **THEN** switchboard MUST return the specific A2A sentinel error for that condition, not a generic
  failure

### Requirement: Concurrency Safety

Streaming and event delivery in this capability MUST follow safe concurrency patterns:

- Context propagation MUST be used for cancellation and timeout signaling across the SSE connection
  lifecycle.
- Stream worker lifecycle MUST be explicitly managed — each `SubscribeToTask`/`SendStreamingMessage`
  connection MUST have a clean startup and a graceful shutdown that unregisters it from the
  committed-transition hook.
- Race safety MUST be ensured between concurrent streams on the same task — all subscribers MUST receive
  events in the same generation order; shared subscriber state MUST be protected by appropriate
  synchronization or eliminated via message passing.
- Concurrent tests for this capability MUST run with race detection enabled in CI.

#### Scenario: Multiple subscribers see identical event order

- **WHEN** two callers concurrently subscribe to the same task
- **THEN** both MUST receive the same sequence of status/artifact events in the same order

### Requirement: Database Operation Standards

Todo state transitions driven by this capability MUST follow structured data access patterns:

- Transactions MUST be used for any multi-step mutation (e.g., a state transition plus its audit record).
- Connection lifecycle MUST be explicitly managed — connections MUST be returned to the pool after use,
  with timeouts configured.
- Query parameters MUST use parameterized queries — string interpolation in queries MUST NOT occur.

#### Scenario: State transition and audit record are atomic

- **WHEN** a todo transitions state as a result of an A2A call
- **THEN** the state change and its audit record MUST commit together or not at all

## Security Requirements

{/* Governing: ADR-0018 (Security-by-Default), SPEC-0016 REQ "Mandatory Security Section in Web Specs" */}

### Authentication

| Endpoint | Auth | Justification |
|----------|------|----------------|
| `SendMessage` | Required | Vended-endpoint bearer credential scoped to the target queue; identical gate to `create_for`. |
| `GetTask` | Required | Vended-endpoint bearer credential scoped to the task's queue. |
| `ListTasks` | Required | Vended-endpoint bearer credential; results scoped to the caller's granted queue(s). |
| `CancelTask` | Required | Vended-endpoint bearer credential scoped to the task's queue. |
| `SendStreamingMessage` / `SubscribeToTask` | Required | Vended-endpoint bearer credential; capability-gated on `streaming: true`. |
| `GetExtendedAgentCard` | Required | Unauthenticated callers get only the public discovery card (SPEC-0009); this endpoint returns nothing extra without a valid credential. |

There are no public endpoints in this capability. (The public discovery Agent Card itself is SPEC-0009's
concern, not this spec's.)

### Rate Limiting

`SendMessage` MUST be rate-limited per vended endpoint (see "Task-Creation Volume Rate Limiting" above),
independent of SPEC-0010's friend-request quota. `GetTask`/`ListTasks` inherit the general per-endpoint
request rate limit already applied to vended MCP endpoints ([SPEC-0007](../vended-endpoints/spec.md));
this spec does not introduce a separate read-path limit. Streaming connections MUST be capped per endpoint
to bound concurrent SSE connection count and prevent resource exhaustion.

### Security Headers

All HTTP responses (including SSE stream responses) MUST include:
- `Content-Security-Policy`: `default-src 'none'` (JSON/SSE API, no embedded content)
- `X-Frame-Options`: DENY
- `X-Content-Type-Options`: nosniff
- `Referrer-Policy`: strict-origin-when-cross-origin

### Request Body Size Limits

All endpoints accepting bodies (`SendMessage`, `CancelTask`) MUST bound them with `http.MaxBytesReader`.
Default limit: 256 KiB, matching the largest reasonable A2A `Message`/`Part` payload this capability
accepts; oversized bodies MUST be rejected before parsing.

### CSRF Protection

This capability's endpoints are bearer-credential-authenticated API calls, not browser-form flows; CSRF
protection is not applicable in the same way it is for SPEC-0010's human-approval web UI. State-changing
calls (`SendMessage`, `CancelTask`) rely on possession of the vended-endpoint bearer credential, which is
never stored in a browser cookie.

### Redirect Validation

No user-supplied redirects exist in this capability.

## More Information

* Task↔Todo mapping, why this is gated by a vended endpoint rather than open peer delegation:
  [ADR-0021](../../../adrs/ADR-0021-a2a-task-delegation-transport.md).
* The durable object tasks project onto: [SPEC-0003](../todo-queue/spec.md).
* What is discovered and the Agent Card this capability extends: [SPEC-0009](../personas/spec.md).
* The friend-request-and-approval flow that is the sole path to a grant this spec does not change:
  [SPEC-0010](../friending/spec.md).
* The internal doorbell mechanism this spec's streaming runs alongside, not instead of:
  [SPEC-0011](../channels/spec.md).
* The separate spec formalizing `PushNotificationConfig` webhook delivery: a2a-push-notifications
  (to follow).
