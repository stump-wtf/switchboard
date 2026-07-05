# switchboard — Channel Delivery (push into a live session)

How switchboard pushes todo notifications into a live harness session over the **[Claude Code
Channels](https://code.claude.com/docs/en/channels-reference)** standard, as a best-effort **notify
layer over the durable todo queue**. Decisions are in
[ADR-013](../adr/ADR-013-channels-push-delivery.md); the durable primitive is
[todos.md](todos.md)/[ADR-007](../adr/ADR-007-todos-as-core-primitive.md); the transport is the vended
MCP endpoint ([ADR-008](../adr/ADR-008-human-principal-vended-endpoints.md)).

## Principle

> The **durable todo is the delivery of record.** A channel notification is a *doorbell* — it tells an
> attached session "you have work." If no session is attached, the notification is dropped by the
> standard (silently, unacknowledged); the todo stays `pending` and is drained by pull. **Push never
> gates correctness** ([ADR-013](../adr/ADR-013-channels-push-delivery.md)).

## The channel is the vended endpoint

A Channels server is an MCP server that (per the standard) Claude Code spawns as a **stdio
subprocess** and that declares `capabilities.experimental['claude/channel'] = {}`. switchboard's vended
MCP endpoint ([ADR-008](../adr/ADR-008-human-principal-vended-endpoints.md)) carries this capability, so
the *same* endpoint that exposes the todo/webhook/friending verbs
([agent-mcp-tools spec](agent-mcp-tools.md)) also receives pushes.

Because Channels requires a local subprocess but switchboard is a central service, the endpoint runs as
a thin **local channel adapter**: the harness spawns it; it authenticates to central switchboard with
the vended credential and bridges both directions —

```
harness (Claude Code / other)
   │ spawns stdio subprocess
   ▼
local channel adapter  ──auth: vended credential──▶  central switchboard  ──▶ durable todos (PostgreSQL)
   ▲  notifications/claude/channel (server→session push)      │
   └──────────────────── todo created/assigned ◀──────────────┘
```

*(The adapter/auth handshake shape is a code-session detail; see Open questions in
[docs/README.md](../README.md).)*

## Todo → notification mapping

On a todo transition that should wake a channel-attached consumer (default: **create** and **assign**),
switchboard emits one `notifications/claude/channel`:

| Notification field | Value |
|--------------------|-------|
| `content` (string) | A legible one-line summary. E.g. `switchboard: todo #4213 on queue "reviews" — PR #482 opened in joestump/switchboard`. |
| `meta` (object)    | Routing identifiers, **snake_case only** (the standard silently drops keys with hyphens/other chars): `todo_id`, `queue`, `kind`, `source`, and optionally `idempotency_key`. |

The event arrives in the session as:

```text
<channel source="switchboard" todo_id="4213" queue="reviews" kind="pull_request" source_type="github">
switchboard: todo #4213 on queue "reviews" — PR #482 opened in joestump/switchboard
</channel>
```

The agent reads `todo_id` and then **claims and completes via the durable verbs**
(`claim`/`complete`, [agent-mcp-tools spec](agent-mcp-tools.md)) — the notification does not itself carry
a lease. Inline payload is **notification-only by default** (summary + ids); whether to inline a small
payload is an [open question](../README.md).

## Delivery semantics

| Property | Behavior |
|----------|----------|
| **Acknowledgement** | None. `notifications/claude/channel` resolves on transport write, not on processing (per standard). |
| **Loss** | Dropped silently if no session is attached / channel unloaded / org policy blocks. Non-fatal — the todo stays `pending`. |
| **Fallback** | Degrade to **pull**: the worker loop drains the todo when a session returns ([ADR-007](../adr/ADR-007-todos-as-core-primitive.md)). |
| **Duplication** | At-least-once notify. Harmless: idempotency-key dedup + idempotent `claim` ([todos spec](todos.md)). |
| **Ordering** | The standard queues events into the session and delivers those that arrived while busy together, in order. switchboard makes no stronger guarantee — the queue is the source of truth. |

## Two-way (optional)

Channels supports two-way; switchboard maps it onto existing durable operations:

* **Reply tool** — for a chat-sourced todo, expose a standard MCP reply tool so the agent can answer
  inline; the reply is recorded against the todo (e.g. via `complete` `result`), not sent out-of-band.
* **Permission relay** (`claude/channel/permission`) — Claude Code forwards a tool-approval prompt
  (`notifications/claude/channel/permission_request`: `request_id`, `tool_name`, `description`,
  `input_preview`); switchboard can route a **human consent** decision — e.g. a friend approval
  ([ADR-010](../adr/ADR-010-a2a-discovery-human-vended-friending.md)) — to the human's channel and return
  `notifications/claude/channel/permission` (`request_id`, `behavior: allow|deny`). The first verdict
  wins. Consent still lands as a durable approval-todo; relay is the fast path, **not** a bypass of
  human vending ([ADR-008](../adr/ADR-008-human-principal-vended-endpoints.md)).

Two-way is opt-in per endpoint and **should be deferred** past the one-way MVP (see Open questions).

## Safety

* **Sender gate = verification + attribution.** Only **verified, human-attributed** todos are eligible
  to push. switchboard's per-source verification ([ADR-003](../adr/ADR-003-per-provider-ingestion-and-trust-model.md))
  and human ownership ([ADR-008](../adr/ADR-008-human-principal-vended-endpoints.md)) satisfy the
  standard's "gate on the sender, not the room" requirement.
* **Injection break rejection.** A payload containing a `</channel>` sequence is rejected upstream so a
  webhook body cannot break out of the `<channel>` wrapper.
* **Secrets never cross.** Secret-bearing header values are withheld behind a fetchable `secret-ref`
  (localhost-only), consistent with [ADR-004](../adr/ADR-004-secrets-management-openbao-approle.md) and
  the MCP surface's no-secrets rule ([mcp-tools.md](mcp-tools.md)).

## Requirements & limits (operational)

Channels is a **research preview** ([ADR-013](../adr/ADR-013-channels-push-delivery.md)):

* Claude Code **v2.1.80+** (permission relay **v2.1.81+**).
* Anthropic auth via **claude.ai or a Console API key** — **not** Bedrock / Vertex / Foundry.
* Team/Enterprise orgs must enable `channelsEnabled`; custom channels need the approved allowlist or
  `--dangerously-load-development-channels`.
* Where a harness does not support Channels, switchboard uses **pull only** — no configuration needed.

## Cross-references

- Decision & rationale: [ADR-013](../adr/ADR-013-channels-push-delivery.md).
- Durable primitive & the revised "MCP can't push" framing: [ADR-007](../adr/ADR-007-todos-as-core-primitive.md), [todos spec](todos.md).
- Transport (vended endpoint) & scope: [ADR-008](../adr/ADR-008-human-principal-vended-endpoints.md), [accounts-and-endpoints spec](accounts-and-endpoints.md).
- Human consent that permission-relay accelerates: [ADR-010](../adr/ADR-010-a2a-discovery-human-vended-friending.md), [friend-requests spec](friend-requests.md).
- Human-side SSE push analogue: [ADR-001](../adr/ADR-001-web-stack-starlette-htmx-pico.md).
- Claude Code Channels reference: <https://code.claude.com/docs/en/channels-reference>.
