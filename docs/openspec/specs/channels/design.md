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

The MVP is implemented in `internal/channel/adapter.go` as a **local stdio adapter**: Claude Code
spawns `switchboard channel` as an MCP stdio subprocess (declared in `.mcp.json`) with a vended
credential ([ADR-0008](../../../adrs/ADR-0008-human-principal-vended-endpoints.md)) in the
environment. The adapter is at once a channel (push) and a tool server (`list_todos`/`claim`/
`complete`/`fail`), bridging the harness to central switchboard's vended agent API over HTTP. The
push source is the agent API's SSE stream (`GET /agent/stream`, `internal/agentapi/agentapi.go`),
which fans newly-created todos to subscribers filtered by queue scope, dropping events for slow
subscribers. The prose contract this design folds in is `docs/specs/channel-delivery.md`.

## Goals / Non-Goals

### Goals

- Deliver push ergonomics (no polling lag when a session is live) without weakening ADR-0007
  durability.
- Reuse the vended MCP endpoint/credential as the channel transport — one transport, one credential,
  one scope.
- Make the failure mode automatic and lossless: no attached session ⇒ the todo waits for pull.
- Keep attribution and injection safety: only verified, human-attributed todos push; payloads cannot
  break out of the `<channel>` wrapper.

### Non-Goals

- Channels as the system of record (rejected option A in ADR-0013) — push never gates correctness.
- Two-way channels for the MVP: the **reply tool** and **permission relay**
  (`claude/channel/permission`) are designed in ADR-0013/`channel-delivery.md` but deferred; the MVP
  adapter is one-way.
- Choosing the final transport: HTTP-direct (Streamable HTTP) vs. stdio adapter is an open item; the
  MVP ships the stdio adapter.
- Guaranteeing ordering or delivery beyond what the queue provides.

## Decisions

### Notify layer over the durable queue (not the ledger)

**Choice**: A todo transition (create/assign) emits at most one `notifications/claude/channel`
carrying a summary + identifiers; the agent still claims/completes via the durable verbs. Push is a
doorbell; the durable todo is the work.
**Rationale**: The standard drops events silently when no session is attached, so push cannot be the
record. Backing push with the durable queue gives "fast *and* safe" — zero durability loss.
**Alternatives considered**:
- Channels as system of record: rejected — lossy/unacknowledged; work would vanish, the exact failure
  ADR-0007 exists to prevent.
- Pull-only: rejected — forfeits a now-available push path and pays polling latency on every idle
  drain even when a session is attached.

### stdio adapter transport for the MVP

**Choice**: Run `switchboard channel` as a local stdio MCP subprocess of the harness, bridging to
central switchboard over HTTP with the vended credential.
**Rationale**: Channels is spawned by Claude Code as a stdio subprocess today; a thin Go adapter maps
cleanly onto that while keeping the durable queue central. Same Go binary, no separate service.
**Alternatives considered**:
- HTTP-direct (Streamable HTTP) channel served alongside vended endpoints and the web UI's SSE:
  preferred if/when Channels supports that MCP transport; deferred as the open transport question.

### snake_case `meta` keys, no lease in the push

**Choice**: `meta` uses identifier-safe snake_case keys (`todo_id`, `queue`, `kind`, `source`); the
notification carries no lease.
**Rationale**: The standard silently drops `meta` keys that are not identifiers, so hyphenated keys
would vanish. Withholding the lease forces the agent through the idempotent `claim`, preserving
ownership/lease semantics.
**Alternatives considered**:
- Inlining the todo payload/lease into the push: deferred (an open question in `channel-delivery.md`)
  — notification-only by default keeps the injection surface and payload size small.

### Drop-on-full fan-out

**Choice**: The SSE hub buffers 32 todos per subscriber and drops on a full buffer rather than
blocking the publisher.
**Rationale**: The queue is the ledger; a dropped doorbell is recoverable by pull. Blocking the
publisher on a slow session would couple todo creation to session liveness.
**Alternatives considered**:
- Unbounded buffering: rejected — unbounded memory growth on a stalled consumer.

## Architecture

The adapter declares `claude/channel` + tools on `initialize`, starts a push goroutine on
`notifications/initialized`, subscribes to `GET /agent/stream`, and emits one
`notifications/claude/channel` per new todo. When no session is attached the push is dropped and the
todo is drained later by pull. All stdout writes are serialized behind a mutex; the push goroutine
honors context cancellation.

```mermaid
sequenceDiagram
    autonumber
    participant WH as Webhook / source (verified, attributed)
    participant Q as Durable todos (PostgreSQL — ledger)
    participant Hub as agentapi SSE Hub
    participant AD as Go channel adapter (stdio)
    participant CC as Live Claude Code session

    Note over AD,CC: Handshake
    CC->>AD: initialize
    AD-->>CC: capabilities.experimental["claude/channel"] + tools
    CC->>AD: notifications/initialized
    AD->>Hub: GET /agent/stream (Bearer vended credential)

    Note over WH,Q: Work arrives
    WH->>Q: create/assign todo (verified + attributed)
    Q->>Hub: publish todo (filtered by queue scope)

    alt Session attached
        Hub-->>AD: SSE event: todo {id,queue,kind,source,title}
        AD->>AD: neutralize </channel>; build content + snake_case meta
        AD-->>CC: notifications/claude/channel (meta.todo_id, ...)
        CC->>AD: tools/call claim {id}
        AD->>Q: POST /agent/todos/{id}/claim (Bearer)
        Q-->>AD: 200 (lease set)
        AD-->>CC: tool result
        CC->>AD: tools/call complete {id,result}
        AD->>Q: POST /agent/todos/{id}/complete (Bearer)
    else No session attached (lossy → degrade to pull)
        Hub--xAD: event dropped silently
        Note over Q: todo stays pending
        CC->>AD: (later) tools/call list_todos
        AD->>Q: GET /agent/todos (Bearer) — pull drain
        Q-->>AD: pending todos
    end
```

## Risks / Trade-offs

- **Push is lossy/unacknowledged** → Back every push with the durable queue; a missed doorbell is
  recovered by the pull worker loop. Push never gates correctness.
- **Two paths to maintain (adapter + queue)** → Accepted as the cost of "fast *and* safe"; the
  adapter is thin and the fallback is automatic.
- **Two-way channels widen the injection surface** → Mitigated by the existing verification +
  attribution sender gate and by neutralizing `</channel>` breakouts; two-way is deferred regardless.
- **Research-preview gates** (Claude Code v2.1.80+, permission relay v2.1.81+, Anthropic auth via
  claude.ai/Console only — not Bedrock/Vertex/Foundry, org `channelsEnabled`, allowlist /
  `--dangerously-load-development-channels`) → Documented as operational constraints; harnesses
  without Channels fall back to pull with no configuration.
- **Duplicate at-least-once notifies** → Idempotency-key dedup + idempotent `claim` make duplicates
  harmless.

## Migration Plan

Greenfield for this capability — the MVP stdio adapter is already built. The only forward migration is
the open transport question: if Channels adopts the Streamable HTTP MCP transport, switchboard can
serve channels HTTP-direct alongside the vended endpoints and web UI SSE, retiring the per-harness
stdio subprocess. That change would be additive (a new transport binding) and MUST preserve the
notify-shape, sender-gate, and degrade-to-pull semantics unchanged.

## Open Questions

- **Transport**: does Claude Code Channels support the Streamable HTTP MCP transport (enabling
  HTTP-direct channels), or does it remain a local stdio subprocess? The MVP ships the stdio adapter;
  HTTP-direct is the preferred end state.
- **Inline payload**: should a small payload be inlined into the push, or remain notification-only
  (summary + ids)? MVP is notification-only.
- **Two-way**: when to enable the reply tool and permission relay (`claude/channel/permission`), and
  how the first-verdict-wins consent maps onto durable approval-todos
  ([ADR-0010](../../../adrs/ADR-0010-a2a-discovery-human-vended-friending.md)).
- **Session liveness tracking**: how precisely to detect an attached/loaded channel so pushes are
  attempted only when they can land (today the fan-out simply drops on no subscriber).
