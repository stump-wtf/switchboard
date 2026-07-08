---
status: approved
date: 2026-07-06
implements: [ADR-0013]
requires: [SPEC-0007, SPEC-0008]
---

# SPEC-0011: Channels Push Delivery

## Overview

Switchboard pushes messages *into* human-owned harnesses (Claude Code and other MCP
harnesses) over the **[Claude Code Channels](https://code.claude.com/docs/en/channels-reference)**
open standard, realizing [ADR-0013](../../../adrs/ADR-0013-channels-push-delivery.md). A *channel*
is an MCP server that declares the experimental `claude/channel` capability; the harness registers a
listener, and the server wakes the live session by emitting a `notifications/claude/channel`
JSON-RPC notification that lands in-session as a `<channel source="switchboard" …>…</channel>` event.

This capability is a **best-effort notify layer over the durable todo queue**, not a delivery system
of record. The durable todo ([ADR-0007](../../../adrs/ADR-0007-todos-as-core-primitive.md)) is the
work; the channel notification is only a doorbell. Delivery is lossy and unacknowledged by the
standard's own definition — a `notifications/claude/channel` resolves when written to the transport,
not when the session processes it, and events are dropped silently when no session is attached, the
channel is unloaded, or org policy blocks it. Therefore push MUST NOT gate correctness: when push is
unavailable the todo simply stays `pending` and is drained by the pull-based worker loop.

The MVP realizes the **local stdio adapter** transport (`internal/channel/adapter.go`): Claude Code
spawns `switchboard channel` as a stdio MCP subprocess with a vended credential
([ADR-0008](../../../adrs/ADR-0008-human-principal-vended-endpoints.md)) in its environment. The
adapter is simultaneously a channel (push) and a tool server (`list_todos`/`claim`/`complete`/`fail`)
over one connection, proxying tool calls to central switchboard's agent API over HTTP and
subscribing to that API's SSE stream (`GET /agent/stream`) to source the pushes.

## Requirements

### Requirement: Channel Capability Declaration

The stdio adapter MUST present itself to the harness as both a Channels server and a tool server over
a single MCP connection. On `initialize`, the adapter MUST return
`capabilities.experimental["claude/channel"] = {}` alongside `capabilities.tools = {}`, so the same
connection carries both server→session pushes and the durable work verbs. The adapter MUST echo the
client's requested `protocolVersion` when supplied, defaulting to `2024-11-05` otherwise, and MUST
identify itself with `serverInfo.name = "switchboard"`.

#### Scenario: Initialize advertises the channel capability

- **WHEN** the harness sends an `initialize` request
- **THEN** the adapter's result includes `capabilities.experimental["claude/channel"]` and
  `capabilities.tools`, `serverInfo.name = "switchboard"`, and human-readable `instructions`
  explaining that todos arrive as `<channel>` doorbell events and the durable queue is the record

#### Scenario: Protocol version is negotiated

- **WHEN** the `initialize` params carry a non-empty `protocolVersion`
- **THEN** the adapter MUST reply with that same `protocolVersion`; otherwise it MUST default to
  `2024-11-05`

### Requirement: Push Notification Shape

On a todo transition that should wake a channel-attached consumer (default: **create** and
**assign**), switchboard MUST emit exactly one `notifications/claude/channel` per todo. Its `content`
field MUST be a single legible one-line summary. Its `meta` field MUST carry routing identifiers and,
because the standard silently drops keys that are not identifier-safe, all `meta` keys MUST be
snake_case (letters, digits, underscore only): `todo_id` and `queue` are REQUIRED; `kind` and
`source` MUST be included when present on the todo. The notification MUST NOT itself carry a lease —
the agent reads `todo_id` and then claims via the durable verbs.

#### Scenario: New todo produces one identifier-safe notification

- **WHEN** the adapter receives a new todo with id, queue, kind, and source over the SSE stream
- **THEN** it MUST emit one `notifications/claude/channel` whose `meta.todo_id` equals the todo id,
  whose `meta.queue` equals the todo queue, and whose `meta.kind`/`meta.source` are set from the todo
  when present, using snake_case keys only

#### Scenario: Notification carries no lease

- **WHEN** a push notification is emitted for a todo
- **THEN** the notification MUST contain only a summary and identifiers, and the agent MUST claim the
  todo through the durable `claim` verb rather than treating the notification as delivery

### Requirement: Best-Effort Lossy Delivery and Degradation to Pull

Delivery MUST be treated as best-effort and unacknowledged. A push MUST NOT be a precondition for a
todo being worked. When no session is attached, the channel is unloaded, org policy blocks, or the
push fails for any reason, the todo MUST remain `pending` and MUST be drained by the pull-based worker
loop ([ADR-0007](../../../adrs/ADR-0007-todos-as-core-primitive.md)) with no loss. Notification is
at-least-once; duplicate notifications MUST be harmless, relying on idempotency-key dedup and an
idempotent `claim`. A slow or full subscriber channel MUST cause the push to be dropped (never
blocking the publisher), because the queue is the ledger and the push is only a doorbell.

#### Scenario: No attached session loses nothing

- **WHEN** a todo is created but no harness session is attached to receive the push
- **THEN** the push is dropped silently AND the todo MUST remain `pending` and MUST be delivered later
  when the harness reconnects and drains the queue by pull

#### Scenario: Duplicate notifications do not double-process

- **WHEN** the same todo is notified more than once
- **THEN** the agent's `claim` MUST be idempotent so that at most one worker takes the todo and no
  double-processing occurs

#### Scenario: Slow subscriber is dropped, not blocked

- **WHEN** a subscriber's buffered channel is full at publish time
- **THEN** the publisher MUST drop the event for that subscriber rather than block, and the todo
  remains recoverable by pull

### Requirement: SSE Source Stream and Reconnection

The adapter MUST source pushes by subscribing to the vended agent API's SSE stream at
`GET /agent/stream`, authenticated with the vended bearer credential and `Accept: text/event-stream`.
The adapter MUST parse `data:` lines into a JSON todo per SSE event boundary (blank line). When the
stream drops or errors, the adapter MUST reconnect with capped exponential backoff (starting at ~1s,
doubling to a ceiling of ~30s), resetting the backoff after a clean stream end. The adapter MUST stop
streaming when its context is cancelled or stdin closes.

#### Scenario: Stream reconnects with backoff after a drop

- **WHEN** the SSE stream connection drops
- **THEN** the adapter MUST retry the connection with exponential backoff capped at ~30s, and MUST
  reset the backoff to the base interval after a subsequent clean stream end

#### Scenario: Streaming begins only after the session is ready

- **WHEN** the harness sends `notifications/initialized`
- **THEN** the adapter MUST start the push goroutine exactly once (guarded so repeated
  `initialized` notifications do not start duplicate streams)

### Requirement: Tool Proxying to the Durable Queue

The adapter MUST expose the durable work verbs — `list_todos`, `claim`, `complete`, `fail` — as MCP
tools and MUST proxy each to the central agent API over HTTP with the vended credential as a bearer
token. `claim`, `complete`, and `fail` MUST require a todo `id` and MUST return a tool error when it
is absent. Tool-call responses MUST surface the agent API's body as text content and MUST mark
`isError: true` when the upstream HTTP status is >= 400. The adapter MUST bound the upstream response
body it reads (LimitReader) so a large or hostile response cannot exhaust memory.

#### Scenario: claim without an id is a tool error

- **WHEN** a `tools/call` for `claim`, `complete`, or `fail` arrives without an `id` argument
- **THEN** the adapter MUST return a tool error and MUST NOT issue the upstream request

#### Scenario: Upstream error is surfaced as isError

- **WHEN** the agent API responds to a proxied tool call with HTTP status >= 400
- **THEN** the tool result MUST set `isError: true` and include the upstream response body as text

### Requirement: Sender Gate and Injection Safety

Only **verified, human-attributed** todos MUST be eligible to push; switchboard's per-source
verification ([ADR-0003](../../../adrs/ADR-0003-per-provider-ingestion-and-trust-model.md)) and human
ownership ([ADR-0008](../../../adrs/ADR-0008-human-principal-vended-endpoints.md)) are the "gate on
the sender" the standard requires. Before emitting a notification, the adapter MUST neutralize any
`</channel>` sequence in payload-derived content so a webhook body cannot break out of the
`<channel>` wrapper. Secret-bearing values MUST NOT be inlined into a push; they MUST remain behind a
fetchable `secret-ref`.

#### Scenario: Payload cannot break out of the channel wrapper

- **WHEN** a todo title contains a literal `</channel>` sequence
- **THEN** the adapter MUST replace it with a safe substitute (e.g. `«/channel»`) before emitting the
  notification so the `<channel>` wrapper cannot be closed by attacker-controlled content

#### Scenario: Only verified, attributed todos are pushed

- **WHEN** a todo has not passed per-source verification and human attribution
- **THEN** it MUST NOT be eligible for a channel push

### Requirement: Concurrency Safety and Error Handling

The adapter runs a stdin dispatch loop and a background push goroutine sharing one stdout pipe. All
writes to stdout MUST be serialized (mutex) because stdout is the JSON-RPC framing pipe and nothing
else may interleave on it. The push goroutine MUST honor context cancellation for clean shutdown.
Errors crossing the HTTP boundary MUST be wrapped with actionable context and surfaced as tool errors
rather than silently swallowed; malformed inbound JSON-RPC lines and malformed SSE payloads MUST be
skipped without crashing the loop.

#### Scenario: Concurrent stdout writes are serialized

- **WHEN** the dispatch loop and the push goroutine both write to stdout concurrently
- **THEN** writes MUST be serialized so JSON-RPC frames never interleave

#### Scenario: Malformed input does not crash the adapter

- **WHEN** a stdin line or an SSE payload is not valid JSON (or lacks a todo id)
- **THEN** the adapter MUST skip it and continue processing subsequent messages

## Security Requirements

### Authentication

The channel adapter itself runs locally beside the harness and speaks stdio to the harness (no
network listener of its own). Every request it makes to central switchboard MUST carry the vended
bearer credential ([ADR-0008](../../../adrs/ADR-0008-human-principal-vended-endpoints.md)); the
central endpoints it calls are the vended agent API, which authenticates every request at the
boundary.

| Endpoint | Auth | Justification |
|----------|------|---------------|
| `GET /agent/stream` (SSE source, central) | Required | Bearer vended credential; resolved to endpoint + immutable scope |
| `GET /agent/todos` (proxied `list_todos`) | Required | Bearer vended credential; verb must be in scope |
| `POST /agent/todos/{id}/claim` | Required | Bearer vended credential; verb + todo queue must be in scope |
| `POST /agent/todos/{id}/complete` | Required | Bearer vended credential; verb + todo queue must be in scope |
| `POST /agent/todos/{id}/fail` | Required | Bearer vended credential; verb + todo queue must be in scope |
| stdio MCP transport (adapter ↔ harness) | Local process | Spawned as a child of the harness on the same host; credential injected via env, never exposed on the wire |

No endpoint in this capability is public. The stdio transport has no listening socket; trust derives
from the harness spawning the adapter locally with the human's vended credential.

### Rate Limiting

Push emission is intrinsically bounded by todo creation rate and by the SSE hub's per-subscriber
buffer (32); when the buffer is full, events are dropped rather than queued, providing natural
backpressure. Proxied tool calls inherit any rate limits enforced centrally by the agent API. A
dedicated per-adapter rate limit is deferred: the adapter is a single-user local process whose
outbound volume is capped by its own SSE input and the reconnection backoff ceiling (~30s).

### Security Headers

Not applicable to the stdio transport (no HTTP responses are served by the adapter). The central SSE
and agent-API HTTP responses this capability consumes are served by the vended agent API surface and
governed by that capability's security requirements. If a future HTTP-direct channel transport
(Streamable HTTP) is adopted per ADR-0013's open transport question, all HTTP responses MUST then
include `Content-Security-Policy`, `X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff`, and
`Referrer-Policy: strict-origin-when-cross-origin`.

### Request Body Size Limits

The adapter MUST bound every response body it reads from central switchboard with a limit reader
(current implementation: 1 MiB for tool-proxy responses) so a large or hostile upstream response
cannot exhaust memory. The stdin scanner MUST use a bounded buffer (current: 8 MiB max token) so an
oversized JSON-RPC line cannot exhaust memory. Should the HTTP-direct transport be adopted, all
request bodies MUST additionally be bounded with `http.MaxBytesReader` (default 1 MiB).

### CSRF Protection

Not applicable: the adapter serves no browser-facing, cookie-authenticated, state-changing HTTP
endpoints. State-changing operations (`claim`/`complete`/`fail`) are authenticated with a bearer
credential over a local stdio transport, which is not subject to browser cross-site request forgery.

### Redirect Validation

No user-supplied redirects exist in this capability. The adapter's only outbound target is the fixed
`SWITCHBOARD_URL` base from its environment; it MUST NOT follow attacker-controlled redirect targets
derived from todo payloads.
