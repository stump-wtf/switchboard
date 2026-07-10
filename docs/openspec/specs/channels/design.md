# Design: Channels Push Delivery

## Context

SPEC-0011 realizes [ADR-0013](../../../adrs/ADR-0013-channels-push-delivery.md): switchboard pushes
work *into* live harness sessions over the [Claude Code
Channels](https://code.claude.com/docs/en/channels-reference) standard, an MCP extension that adds a
server→session push. [ADR-0007](../../../adrs/ADR-0007-todos-as-core-primitive.md) chose a durable,
pull-based todo primitive precisely because the MCP request/response tool model has no push; Channels
adds a real push path, but its delivery is **best-effort and unacknowledged** by the standard's own
definition. ADR-0013 resolves the tension: Channels is a *notify layer* over the durable queue, never
the ledger. The durable todo is the delivery of record; the channel notification is a doorbell.

**Transport** is decided and owned elsewhere: [ADR-0017](../../../adrs/ADR-0017-mcp-streamable-http-only.md)
/ [SPEC-0014](../mcp-transport/spec.md) serve every vended endpoint exclusively over **Streamable
HTTP**, and the channel capability rides that same session — one transport, one credential, one
scope. The original MVP's local stdio adapter (`switchboard channel`, `internal/channel/adapter.go`)
and its `/agent/stream` SSE source are retired by that decision. What remains in this capability is
the **push semantics layer**: eligibility (sender gate), shape (summary + snake_case `meta`),
fan-out filtering by endpoint scope, and the lossy, degrade-to-pull delivery contract.

## Goals / Non-Goals

### Goals

- Deliver push ergonomics (no polling lag when a session is live) without weakening ADR-0007
  durability.
- Reuse the vended MCP session as the channel carrier — no second connection, credential, or scope.
- Make the failure mode automatic and lossless: no attached session ⇒ the todo waits for pull.
- Keep attribution and injection safety: only verified, human-attributed todos push; payloads cannot
  break out of the `<channel>` wrapper.

### Non-Goals

- Channels as the system of record (rejected option A in ADR-0013) — push never gates correctness.
- Two-way channels for the MVP: the **reply tool** and **permission relay**
  (`claude/channel/permission`) are designed in ADR-0013 but deferred; push is one-way.
- Transport, session, and tool-serving mechanics — governed by SPEC-0014.
- Guaranteeing ordering or delivery beyond what the queue provides.

## Decisions

### Notify layer over the durable queue (not the ledger)

**Choice**: A todo transition (create/assign) emits at most one `notifications/claude/channel` per
in-scope attached session, carrying a summary + identifiers; the agent still claims/completes via the
durable verbs. Push is a doorbell; the durable todo is the work.
**Rationale**: The standard drops events silently when no session is attached, so push cannot be the
record. Backing push with the durable queue gives "fast *and* safe" — zero durability loss.
**Alternatives considered**:
- Channels as system of record: rejected — lossy/unacknowledged; work would vanish, the exact failure
  ADR-0007 exists to prevent.
- Pull-only: rejected — forfeits a now-available push path and pays polling latency on every idle
  drain even when a session is attached.

### Doorbells ride the vended Streamable HTTP session (no adapter)

**Choice**: The notification hub publishes directly to attached SPEC-0014 sessions filtered by
endpoint queue scope; there is no client-side bridge process.
**Rationale**: ADR-0017 removed the stdio adapter and its REST/SSE plumbing; emitting on the session
the agent already holds eliminates a process, a credential hand-off, and a reconnection protocol.
**Alternatives considered**:
- Local stdio adapter bridging to central SSE (the original MVP): retired — required the binary on
  every agent host and duplicated transport code (ADR-0017).
- A dedicated notification WebSocket/SSE endpoint separate from MCP: rejected — second transport to
  authenticate and keep alive for no semantic gain.

### snake_case `meta` keys, no lease in the push

**Choice**: `meta` uses identifier-safe snake_case keys (`todo_id`, `queue`, `kind`, `source`); the
notification carries no lease.
**Rationale**: The standard silently drops `meta` keys that are not identifiers, so hyphenated keys
would vanish. Withholding the lease forces the agent through the idempotent `claim`, preserving
ownership/lease semantics.
**Alternatives considered**:
- Inlining the todo payload/lease into the push: deferred — notification-only keeps the injection
  surface and payload size small.

### Drop-on-full fan-out

**Choice**: The hub buffers a bounded number of doorbells per subscriber and drops on a full buffer
rather than blocking the publisher.
**Rationale**: The queue is the ledger; a dropped doorbell is recoverable by pull. Blocking the
publisher on a slow session would couple todo creation to session liveness.
**Alternatives considered**:
- Unbounded buffering: rejected — unbounded memory growth on a stalled consumer.

## Architecture

```mermaid
sequenceDiagram
    autonumber
    participant WH as Webhook / source (verified, attributed)
    participant Q as Durable todos (PostgreSQL — ledger)
    participant Hub as Notification hub (in-process)
    participant S as Vended MCP session (Streamable HTTP, SPEC-0014)
    participant CC as Live Claude Code session

    Note over S,CC: Session (SPEC-0014)
    CC->>S: initialize (Bearer vended credential)
    S-->>CC: capabilities: tools + experimental claude/channel

    Note over WH,Q: Work arrives
    WH->>Q: create/assign todo (verified + attributed)
    Q->>Hub: NOTIFY todo_ready → publish (filtered by endpoint queue scope)

    alt Session attached and in scope
        Hub-->>S: doorbell {id, queue, kind, source, title}
        S->>S: neutralize </channel>; build content + snake_case meta
        S-->>CC: notifications/claude/channel (meta.todo_id, …)
        CC->>S: tools/call claim {id}
        S->>Q: ClaimTodo (lease set)
        CC->>S: tools/call complete {id, result}
        S->>Q: CompleteTodo (ack)
    else No session attached (lossy → degrade to pull)
        Hub--xS: event dropped silently
        Note over Q: todo stays pending
        CC->>S: (later) tools/call list_todos — pull drain
        S->>Q: pending todos
    end
```

## Risks / Trade-offs

- **Push is lossy/unacknowledged** → Back every push with the durable queue; a missed doorbell is
  recovered by the pull worker loop. Push never gates correctness.
- **Harness support for channels over HTTP MCP may lag stdio channels** → tools work regardless;
  agents degrade to pull until harness support lands (tracked in SPEC-0014's open questions).
- **Two-way channels widen the injection surface** → Mitigated by the existing verification +
  attribution sender gate and by neutralizing `</channel>` breakouts; two-way is deferred regardless.
- **Research-preview gates** (Claude Code version and org policy for Channels) → Documented as
  operational constraints; harnesses without Channels fall back to pull with no configuration.
- **Duplicate at-least-once notifies** → Idempotency-key dedup + idempotent `claim` make duplicates
  harmless.

## Migration Plan

The push-semantics layer migrates with SPEC-0014's cutover: the hub's publish target changes from
the retired `/agent/stream` SSE subscribers to attached MCP sessions, preserving notify shape,
sender gate, scope filtering, and degrade-to-pull unchanged. No data migration; the stdio adapter's
deletion is governed by SPEC-0014's migration plan.

## Open Questions

- **Inline payload**: should a small payload be inlined into the push, or remain notification-only
  (summary + ids)? MVP is notification-only.
- **Two-way**: when to enable the reply tool and permission relay (`claude/channel/permission`), and
  how the first-verdict-wins consent maps onto durable approval-todos
  ([ADR-0010](../../../adrs/ADR-0010-a2a-discovery-human-vended-friending.md)).
- **Session liveness tracking**: how precisely to detect an attached/loaded channel so pushes are
  attempted only when they can land (today the fan-out simply drops on no subscriber).
