---
status: draft
date: 2026-07-20
implements: [ADR-0021]
requires: [SPEC-0018, SPEC-0007]
related: [SPEC-0011]
---

# SPEC-0019: A2A Push Notification Webhooks

## Overview

This capability formalizes the second half of [ADR-0021](../../../adrs/ADR-0021-a2a-task-delegation-transport.md):
A2A's `PushNotificationConfig` mechanism — a caller-registered webhook that switchboard POSTs task
updates to, over authenticated HTTP, so an external A2A client does not have to hold an open connection or
poll `GetTask`. This is a genuinely new external attack surface (server-initiated HTTP requests to a
caller-supplied URL are a classic SSRF vector) and is deliberately specified separately from
[SPEC-0018](../a2a-tasks/spec.md) so its security requirements get first-class treatment rather than
being folded into the task RPC spec as an afterthought.

**This is not the same thing as Channels.** [SPEC-0011](../channels/spec.md) ("Channels Push Delivery")
is switchboard's existing internal, best-effort, MCP-native notification ("doorbell") that tells an
already-connected switchboard-hosted session "you have work" — no retry, no external HTTP call, no
caller-supplied URL. `PushNotificationConfig` is the opposite in every one of those respects: external,
durable/retried, authenticated HTTP POST to a URL the caller registers. Both are triggered by the same
underlying event (a todo's committed state transition), which is why this spec is `related` to SPEC-0011
rather than dependent on it — they share a trigger, not an implementation.

This spec **requires** [SPEC-0018](../a2a-tasks/spec.md) (a task must exist to have push notifications
configured against it) and [SPEC-0007](../vended-endpoints/spec.md) (the CRUD endpoints reuse the vended-
endpoint auth model).

## Requirements

### Requirement: PushNotificationConfig CRUD

Switchboard MUST implement `CreateTaskPushNotificationConfig`, `GetTaskPushNotificationConfig`,
`ListTaskPushNotificationConfigs`, and `DeleteTaskPushNotificationConfig`. A `PushNotificationConfig` MUST
have an `id` (server-assigned), `taskId`, `url` (the webhook target), an optional `token` (caller-supplied,
echoed back on delivery so the caller can correlate/verify), and an optional `authentication` descriptor
(the scheme switchboard MUST use when calling the caller's webhook — e.g. bearer token, API key). All four
operations MUST require the same vended-endpoint authorization as the task they reference
([SPEC-0018](../a2a-tasks/spec.md)'s auth model) — a caller may only manage push configs for tasks within
its own granted scope. `DeleteTaskPushNotificationConfig` MUST be idempotent.

#### Scenario: Config CRUD is scoped to the caller's own tasks

- **WHEN** a caller attempts to `GetTaskPushNotificationConfig` for a task outside its granted scope
- **THEN** the request MUST be rejected exactly as `GetTask` would reject it, and no config details MUST
  be leaked

#### Scenario: Delete is idempotent

- **WHEN** `DeleteTaskPushNotificationConfig` is called twice for the same config id
- **THEN** the second call MUST succeed without error

### Requirement: Webhook Target Validation (SSRF Guard)

Every `url` supplied to `CreateTaskPushNotificationConfig` MUST be validated before it is stored and
again immediately before each delivery attempt (to defend against DNS-rebinding between registration and
delivery). The scheme MUST be `https` unless an operator explicitly allows `http` for a documented
non-production scope. The resolved connection-time IP MUST NOT be a loopback, link-local, or
RFC 1918/private address, and MUST NOT resolve to switchboard's own listening address(es). A `url` that
fails validation MUST be rejected at creation time with a clear error, and a previously-valid `url` that
now resolves to a disallowed address at delivery time MUST cause that delivery attempt to fail closed
(no request sent) rather than silently connecting.

#### Scenario: Loopback/private target is rejected at creation

- **WHEN** `CreateTaskPushNotificationConfig` is called with a `url` resolving to a loopback or private
  address
- **THEN** the config MUST be rejected and MUST NOT be persisted

#### Scenario: DNS rebinding is caught at delivery time

- **WHEN** a previously-valid webhook `url`'s DNS record is changed to resolve to a disallowed address
  before a delivery fires
- **THEN** that delivery attempt MUST fail closed without connecting, and MUST be recorded as a failed
  attempt subject to the retry policy below

### Requirement: Authenticated, Retried Delivery

On every committed todo state transition for a task with an active `PushNotificationConfig`, switchboard
MUST POST a JSON body (the current `Task`, `TaskStatusUpdateEvent`, or `TaskArtifactUpdateEvent`, matching
A2A's `StreamResponse` shape) to the configured `url`, applying the configured `authentication` to the
outbound request. Delivery MUST be triggered from the same committed-transition hook
(`store.SetTodoDoorbellHook`) that drives both Channels ([SPEC-0011](../channels/spec.md)) and
[SPEC-0018](../a2a-tasks/spec.md)'s streaming — there MUST NOT be a second, independent transition-
detection path. Delivery MUST be retried with bounded exponential backoff on failure (connection error,
timeout, or non-2xx response) up to a configurable attempt cap; after the cap is exhausted, further
retries MUST stop and the failure MUST be recorded (not silently dropped) so it is diagnosable. Each
delivery attempt MUST have a bounded timeout; a slow or hanging receiver MUST NOT block other deliveries
or other switchboard work.

#### Scenario: Successful delivery on first attempt

- **WHEN** a task transitions state and its `PushNotificationConfig` webhook responds 2xx on the first
  attempt
- **THEN** no retry MUST occur and the delivery MUST be recorded as successful

#### Scenario: Retries are bounded and recorded

- **WHEN** a webhook is unreachable for every delivery attempt up to the configured cap
- **THEN** switchboard MUST stop retrying once the cap is reached, MUST NOT retry indefinitely, and MUST
  record the exhausted-retry failure

#### Scenario: A slow receiver does not block other deliveries

- **WHEN** one webhook target is slow to respond
- **THEN** delivery attempts to other tasks'/callers' webhooks MUST proceed independently and MUST NOT be
  blocked by the slow target

### Requirement: Delivery Deduplication

Each delivery attempt MUST carry a monotonically increasing per-task sequence number (or equivalent) so a
receiver that gets the same event more than once (e.g., a retry that actually succeeded but whose response
was lost) can detect and discard the duplicate. Switchboard MUST NOT rely on exactly-once delivery
semantics — the contract is at-least-once with receiver-side dedup support, matching A2A's own "no
guaranteed delivery; client must handle retries/timeouts" posture.

#### Scenario: Duplicate delivery is detectable by the receiver

- **WHEN** a delivery is retried after an ambiguous failure (request may have been received before the
  response was lost)
- **THEN** the retried delivery MUST carry the same sequence number as the original, so the receiver can
  identify it as a duplicate

### Requirement: Capability Gate

`CreateTaskPushNotificationConfig` (and the other three CRUD operations) MUST return
`PushNotificationNotSupportedError` for any persona whose Agent Card does not advertise
`capabilities.pushNotifications: true`. This capability flag MUST flip to `true` only once this spec's
delivery mechanism is implemented and operational for that persona — it MUST NOT be advertised before the
delivery path exists.

#### Scenario: Unsupported persona rejects config creation

- **WHEN** a caller attempts `CreateTaskPushNotificationConfig` against a persona whose card advertises
  `capabilities.pushNotifications: false`
- **THEN** the request MUST be rejected with `PushNotificationNotSupportedError` and no config MUST be
  created

### Requirement: Error Handling Standards

- Errors MUST be wrapped with contextual information at each layer boundary (e.g., "webhook delivery
  failed: dial tcp: connection refused").
- Sentinel errors MUST be defined for `PushNotificationNotSupportedError` and webhook-validation failures
  so callers receive the correct A2A error shape.
- Silent error swallowing MUST NOT occur — every delivery failure MUST be logged with sufficient context
  (config id, task id, attempt number, failure reason) or surfaced to the caller.
- Structured logging MUST be used for delivery failure reporting.

#### Scenario: Delivery failure is logged with context

- **WHEN** a delivery attempt fails
- **THEN** the failure MUST be logged with the config id, task id, attempt number, and failure reason —
  not swallowed silently

### Requirement: Concurrency Safety

- Context propagation MUST be used for the delivery HTTP request's timeout/cancellation.
- Delivery worker lifecycle MUST be explicitly managed — retry-scheduling goroutines/workers MUST have a
  clean startup and graceful shutdown.
- Race safety MUST be ensured for the per-task delivery sequence counter under concurrent transitions.
- Concurrent delivery tests MUST run with race detection enabled in CI.

#### Scenario: Sequence numbers stay monotonic under concurrent transitions

- **WHEN** two state transitions for the same task commit in rapid succession
- **THEN** their delivery sequence numbers MUST remain strictly increasing and MUST NOT collide

### Requirement: Database Operation Standards

- Transactions MUST be used for config creation/deletion alongside any related audit record.
- Connection lifecycle MUST be explicitly managed with configured timeouts.
- Query parameters MUST use parameterized queries.

#### Scenario: Config creation and audit record are atomic

- **WHEN** a `PushNotificationConfig` is created
- **THEN** the config row and its audit record MUST commit together or not at all

## Security Requirements

{/* Governing: ADR-0018 (Security-by-Default), SPEC-0016 REQ "Mandatory Security Section in Web Specs" */}

### Authentication

| Endpoint | Auth | Justification |
|----------|------|----------------|
| `CreateTaskPushNotificationConfig` | Required | Vended-endpoint bearer credential scoped to the task's queue. |
| `GetTaskPushNotificationConfig` | Required | Vended-endpoint bearer credential; caller may only read its own configs. |
| `ListTaskPushNotificationConfigs` | Required | Vended-endpoint bearer credential; results scoped to caller's own configs. |
| `DeleteTaskPushNotificationConfig` | Required | Vended-endpoint bearer credential scoped to the task's queue. |

There are no public endpoints in this capability. Outbound webhook delivery is not an "endpoint" switchboard
serves — it is a client request switchboard *originates*, governed by the SSRF-guard requirement above
rather than an inbound-auth table entry.

### Rate Limiting

`CreateTaskPushNotificationConfig` MUST be rate-limited per vended endpoint to bound the number of
registered webhooks (preventing a caller from registering a large number of configs to amplify
outbound-delivery load). Outbound delivery attempts MUST themselves be rate-limited per destination host
to avoid switchboard being used as a request amplifier against a third-party target.

### Security Headers

CRUD endpoints (JSON API) MUST include: `Content-Security-Policy: default-src 'none'`,
`X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff`,
`Referrer-Policy: strict-origin-when-cross-origin`. Outbound webhook delivery requests are not
browser-facing responses and are not subject to these header requirements (they are switchboard acting as
an HTTP client, not a server response).

### Request Body Size Limits

`CreateTaskPushNotificationConfig` bodies MUST be bounded with `http.MaxBytesReader`. Default limit: 8 KiB
(a `PushNotificationConfig` is a small structured object — url, token, auth descriptor — not a large
payload).

### CSRF Protection

This capability's endpoints are bearer-credential-authenticated API calls, not browser-form flows; CSRF
protection is not applicable in the browser-session sense. State-changing calls rely on possession of the
vended-endpoint bearer credential.

### Redirect Validation

Outbound webhook delivery MUST NOT follow HTTP redirects automatically — a redirect response from a
registered webhook target MUST be treated as a delivery failure (subject to retry), not silently followed,
because following it would bypass the SSRF-guard validation performed on the originally-registered `url`.

## More Information

* Why this is a separate spec from the core task RPC surface: [ADR-0021](../../../adrs/ADR-0021-a2a-task-delegation-transport.md).
* The tasks these push notifications are delivered for: [SPEC-0018](../a2a-tasks/spec.md).
* The internal, best-effort doorbell this spec runs alongside, not instead of: [SPEC-0011](../channels/spec.md).
* The vended-endpoint auth model this spec's CRUD operations reuse: [SPEC-0007](../vended-endpoints/spec.md).
