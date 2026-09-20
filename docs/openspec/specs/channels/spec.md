---
status: amended
date: 2026-07-10
implements: [ADR-0013, ADR-0027]
requires: [SPEC-0007, SPEC-0008, SPEC-0014]
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

**Transport.** Channels ride the vended MCP endpoint's **Streamable HTTP** session
([SPEC-0014](../mcp-transport/spec.md), [ADR-0017](../../../adrs/ADR-0017-mcp-streamable-http-only.md)):
the same session that serves the durable work verbs advertises the channel capability and carries the
doorbell notifications on its server-to-client stream. The earlier local stdio adapter
(`switchboard channel`) is retired per ADR-0017; session mechanics, authentication, tool serving, and
stream lifecycle are governed by SPEC-0014. This spec governs **push semantics**: what may be pushed,
when, in what shape, and with what delivery guarantees.

> **Amended by [ADR-0027](../../../adrs/ADR-0027-endpoint-presence-clock-in-clock-out.md)
> (2026-09-17).** Doorbells are withheld while an endpoint is clocked out
> ([SPEC-0022](../endpoint-presence/spec.md)). A second notification kind, the **digest**, summarizes
> held work on return. The heartbeat sweep, previously unspecified, is specified below, and it no
> longer counts a ring that no session can receive.

## Requirements

### Requirement: Channel Capability on the Vended Session

Every vended MCP session (SPEC-0014) MUST advertise
`capabilities.experimental["claude/channel"] = {}` alongside `capabilities.tools = {}` on
`initialize`, MUST identify itself with `serverInfo.name = "switchboard"`, and MUST supply
human-readable `instructions` explaining that todos arrive as `<channel>` doorbell events while the
durable queue remains the record. Protocol-version negotiation is delegated to the SDK per
SPEC-0014.

#### Scenario: Initialize advertises the channel capability

- **WHEN** a harness sends `initialize` to a vended endpoint
- **THEN** the result includes `capabilities.experimental["claude/channel"]` and
  `capabilities.tools`, `serverInfo.name = "switchboard"`, and instructions describing
  doorbell-over-durable-queue semantics

### Requirement: Push Notification Shape

On a todo transition that should wake a channel-attached consumer (default: **create** and
**assign**), switchboard MUST emit exactly one `notifications/claude/channel` per todo per attached
session in scope. Its `content` field MUST be a single legible one-line summary. Its `meta` field
MUST carry routing identifiers and, because the standard silently drops keys that are not
identifier-safe, all `meta` keys MUST be snake_case (letters, digits, underscore only): `todo_id`
and `queue` are REQUIRED; `kind` and `source` MUST be included when present on the todo. The
notification MUST NOT itself carry a lease — the agent reads `todo_id` and then claims via the
durable verbs.

A **digest doorbell** (SPEC-0022 REQ "Clock-In Digest") is the one notification without a `todo_id`,
so the `todo_id`/`queue` requirement above applies in full to todo doorbells and is relaxed only
here. Its `meta` MUST carry `kind = "digest"`, `pending` (a decimal count), `queues` (comma-joined
queue names, sorted), and `reason` (`clock_in`, `operator`, `shift_start`, `override_end` or
`reconnect`). Its `content` MUST be one line of counts, queue names and the oldest pending age, and
MUST NOT contain any todo title, payload or other webhook-derived text. The session `instructions`
MUST describe both notification kinds: claim `meta.todo_id` for a todo doorbell, drain with
`claim_next` for a digest.

A todo's `kind` is copied from the source payload and is therefore **not** a closed vocabulary
(`internal/ingest/signed.go` sets it from a Stripe event type, for one), so a raw `kind` value MUST
NOT be the thing that distinguishes the two shapes. A consumer MUST treat a notification as a digest
only when `kind = "digest"` **and** no `todo_id` is present, and a producer MUST NOT emit a todo
doorbell whose `meta.kind` is `"digest"` — the todo's own kind MUST be carried under a distinct key
(or omitted) when it would collide. Otherwise a webhook whose payload type happened to be `digest`
would forge a digest frame inside a todo doorbell that also advertises a `todo_id`, and a consumer
branching on `kind` alone would drop a real todo.

#### Scenario: Digest carries counts, not content

- **WHEN** an endpoint clocks in with 7 pending todos across `reviews` (5) and `lane-m` (2)
- **THEN** its digest has `meta.kind = "digest"`, `meta.pending = "7"`,
  `meta.queues = "lane-m,reviews"`, no `meta.todo_id`, and content with no todo title

#### Scenario: A todo whose kind is "digest" is not forged into a digest

- **WHEN** a delivery creates a todo whose source payload type is `digest`
- **THEN** the notification it produces carries its `meta.todo_id`, and its `meta.kind` is not
  `"digest"` (the todo's own kind is carried under a distinct key or omitted), so a consumer
  branching on `kind` and `todo_id` still treats it as a todo doorbell

#### Scenario: New todo produces one identifier-safe notification

- **WHEN** a todo becomes ready in a queue within an attached session's scope
- **THEN** switchboard MUST emit one `notifications/claude/channel` on that session whose
  `meta.todo_id` equals the todo id, whose `meta.queue` equals the todo queue, and whose
  `meta.kind`/`meta.source` are set from the todo when present, using snake_case keys only

#### Scenario: Notification carries no lease

- **WHEN** a push notification is emitted for a todo
- **THEN** the notification MUST contain only a summary and identifiers, and the agent MUST claim the
  todo through the durable `claim` verb rather than treating the notification as delivery

### Requirement: Scope-Filtered Fan-Out

A doorbell MUST be delivered only to sessions whose vended endpoint scope (SPEC-0007) covers the
todo's queue. Sessions MUST NOT receive notifications for queues outside their grant, regardless of
verb allowlist. A todo doorbell MUST NOT be delivered to any session of an endpoint whose effective
presence is `out` (SPEC-0022 REQ "Held Doorbells"). The todo stays `pending` and pullable, and no
ring is recorded for it.

#### Scenario: Clocked-out endpoint is not rung

- **WHEN** a todo becomes ready on an endpoint that is clocked out and has a session with an open
  stream
- **THEN** that session receives no notification, and the todo's `ring_attempts` stays unchanged

#### Scenario: Out-of-scope todo is not pushed

- **WHEN** a todo becomes ready in a queue not covered by an attached session's endpoint scope
- **THEN** that session MUST receive no notification for it

### Requirement: Best-Effort Lossy Delivery and Degradation to Pull

Delivery MUST be treated as best-effort and unacknowledged. A push MUST NOT be a precondition for a
todo being worked. When no session is attached, the channel is unloaded, org policy blocks, or the
push fails for any reason, the todo MUST remain `pending` and MUST be drained by the pull-based worker
loop ([ADR-0007](../../../adrs/ADR-0007-todos-as-core-primitive.md)) with no loss. Notification is
at-least-once; duplicate notifications MUST be harmless, relying on idempotency-key dedup and an
idempotent `claim`. A slow or full subscriber MUST cause the push to be dropped (never blocking the
publisher), because the queue is the ledger and the push is only a doorbell. When a session opens
its notification stream, switchboard MUST ring that stream at once for a bounded number of the
oldest pending, push-eligible todos in its scope — charged to the same per-todo ring budget as the
heartbeat re-ring, and repeated for a given todo no more often than a cooldown — so a consumer that
restarted does not wait for the next sweep.

#### Scenario: No attached session loses nothing

- **WHEN** a todo is created but no harness session is attached to receive the push
- **THEN** the push is dropped silently AND the todo MUST remain `pending` and MUST be delivered later
  when the harness reconnects and drains the queue by pull

#### Scenario: Reconnecting session is digested for waiting work

- **WHEN** a session opens its notification stream while push-eligible todos in its scope are
  `pending`
- **THEN** switchboard MUST NOT ring those todos individually on the reconnect; the session receives
  the one reconnect digest (SPEC-0022, REQ "Clock-In Digest"), which sets `last_ringed_at` on the
  counted todos without spending ring budget, and delivery resumes through the sweep and push paths
  afterward — a reconnect that also charged rings would spend, before the agent knows what is
  waiting, exactly the turns the digest exists to save

#### Scenario: Duplicate notifications do not double-process

- **WHEN** the same todo is notified more than once
- **THEN** the agent's `claim` MUST be idempotent so that at most one worker takes the todo and no
  double-processing occurs

#### Scenario: Slow subscriber is dropped, not blocked

- **WHEN** a subscriber's buffered fan-out channel is full at publish time
- **THEN** the publisher MUST drop the event for that subscriber rather than block, and the todo
  remains recoverable by pull

### Requirement: Doorbell Heartbeat

A pending, push-eligible todo that nobody claims MUST be rung again on a widening backoff: about 5
minutes after creation, then 20 minutes, 1 hour, and 6 hours after each previous ring, for at most 5
rings. A sweep MUST ring at most a small fixed number of todos (3), round-robin across endpoints, and
only todos on active endpoints. The ring and its count MUST be recorded in the same statement that
selects the row, so two sweeps rarely ring the same todo.

The interval is measured from `last_ringed_at` (from creation when that is null), never selected from
`ring_attempts` alone: a digest sets `last_ringed_at` without spending an attempt, so a todo marked
at `ring_attempts = 0` must not fall into an arbitrary branch of the schedule. A digest mark advances
a todo's backoff exactly as a ring would for timing, but neither spends the ring budget nor counts
toward the 5-ring maximum.

Each instance's sweep MUST select only todos whose endpoint, at sweep time, has at least one session
attached to that instance and an effective presence of `in`. A todo whose endpoint has no such
session MUST NOT be selected, and its `ring_attempts` and `last_ringed_at` MUST be unchanged, so a
disconnected or clocked-out agent keeps its full ring budget for its return. A digest (REQ "Push
Notification Shape") sets `last_ringed_at` on the todos it counted without spending a ring attempt.

"Attached" MUST mean what the delivery path already means by it, not the narrower "holds an open
stream": `PublishTodoReady` rings a streaming session first but falls back to a session whose stream
is momentarily closed (`inflight = 0`), and the transport drops that push only if no stream is open
by the time it is written. An endpoint whose only session is in that state MUST therefore still be a
sweep receiver — otherwise its todo is counted by no sweep, and because nothing else re-arms it, the
one-ring-per-sweep backoff silently becomes no ring at all. A `receivers` list built from open
streams alone reintroduces exactly the lost-ring failure this requirement exists to prevent; the
distinction between "attached" and "streaming" belongs to session selection inside
`PublishTodoReady`, where the fallback already lives.

A ring whose push is then dropped (a buffer-full, or a stream that closed between selection and
write) still spends its attempt. That is the existing lossy contract and is accepted: the todo stays
`pending` and pullable, and the next backoff step still fires while a session is attached.

#### Scenario: Agent offline overnight

- **WHEN** a todo is created at 18:00 on an endpoint with no connected session, and the agent
  reconnects at 09:00
- **THEN** the todo's `ring_attempts` is still 0 at 09:00, and the sweep's first ring waits its
  backoff from the reconnect digest

#### Scenario: Ring budget exhausted while connected

- **WHEN** an endpoint stays connected and in, and a todo is rung 5 times without being claimed
- **THEN** it is not rung again and stays `pending` and visible

#### Scenario: An attached session with no open stream still receives sweeps

- **WHEN** an endpoint's only session on an instance is attached but has no open notification stream
  at sweep time (`inflight = 0`), and it has an unclaimed todo due for a re-ring
- **THEN** the sweep still selects that todo and the attempt is counted, because
  `PublishTodoReady` falls back to a streamless session and a push dropped at the transport
  (rather than one never attempted) is the contract this sweep exists to satisfy

### Requirement: Sender Gate and Injection Safety

Only **verified, human-attributed** todos MUST be eligible to push; switchboard's per-source
verification ([ADR-0003](../../../adrs/ADR-0003-per-provider-ingestion-and-trust-model.md)) and human
ownership ([ADR-0008](../../../adrs/ADR-0008-human-principal-vended-endpoints.md)) are the "gate on
the sender" the standard requires. Before emitting a notification, switchboard MUST neutralize any
`</channel>` sequence in payload-derived content so a webhook body cannot break out of the
`<channel>` wrapper. Secret-bearing values MUST NOT be inlined into a push; they MUST remain behind a
fetchable `secret-ref`. A todo the endpoint's own operator authored over the operator API
([ADR-0026](../../../adrs/ADR-0026-operator-authored-todos.md)) carries the strongest attribution
switchboard has — an OIDC-authenticated human, the principal that vended the endpoint — and MUST be
push-eligible, recorded as a verified delivery event of trust mode `operator`; its title and payload
remain untrusted content and MUST pass through the same neutralization as any other push.

#### Scenario: Payload cannot break out of the channel wrapper

- **WHEN** a todo title contains a literal `</channel>` sequence
- **THEN** switchboard MUST replace it with a safe substitute (e.g. `«/channel»`) before emitting the
  notification so the `<channel>` wrapper cannot be closed by attacker-controlled content

#### Scenario: Only verified, attributed todos are pushed

- **WHEN** a todo has not passed per-source verification and human attribution
- **THEN** it MUST NOT be eligible for a channel push

#### Scenario: Operator-authored todo is pushed

- **WHEN** the human who owns an endpoint hands it a todo over the operator API
- **THEN** the todo MUST be persisted with a verified delivery event of trust mode `operator`
  naming that human, and MUST ring the endpoint's doorbell exactly as a verified webhook delivery
  does — while a repeat of the same operator-supplied idempotency key MUST return the existing todo
  and ring nothing

### Requirement: Error Handling Standards

Push emission MUST follow structured error handling: publish failures MUST be logged with context
(endpoint slug, todo id) and dropped per the lossy contract — never retried into a blocking path and
never silently swallowed without a log line; structured key-value logging MUST be used; the bearer
credential MUST never appear in logs or notification content.

#### Scenario: Publish failure is logged and dropped

- **WHEN** writing a notification to a session stream fails
- **THEN** the failure MUST be logged with endpoint and todo context and the notification dropped,
  leaving the todo recoverable by pull

## Security Requirements

Transport authentication, rate limiting, headers, body limits, CSRF posture, and redirect posture
for the carrying session are governed by [SPEC-0014](../mcp-transport/spec.md) — channels introduce
**no additional endpoints**. This capability's own security obligations are the sender gate and
injection-safety requirements above, plus:

### Authentication

Doorbells MUST only ever be written to sessions that authenticated per SPEC-0014; there is no
unauthenticated notification path.

### Rate Limiting

Push emission is intrinsically bounded by todo creation rate and by the fan-out hub's per-subscriber
buffer; when the buffer is full, events are dropped rather than queued, providing natural
backpressure. No additional rate limit is required for emission itself.
