# switchboard — Ingestion Adapters (push & pull) → Todos

How external deliveries become todos. Ingestion is modelled as **adapters** in two families — **push**
(webhooks, inbound HTTP) and **pull** (queue adapters the app consumes) — that share one normalization
contract into the durable todo queue ([ADR-007](../adr/ADR-007-todos-as-core-primitive.md)). The adapter
abstraction and the pull-side ack coupling are decided in
[ADR-014](../adr/ADR-014-ingestion-adapters-push-pull.md); trust semantics are in
[ADR-003](../adr/ADR-003-per-provider-ingestion-and-trust-model.md); secrets/connection strings come
from OpenBao ([ADR-004](../adr/ADR-004-secrets-management-openbao-approle.md)); agent-managed webhooks
(the push family) are bounded by a ceiling ([ADR-012](../adr/ADR-012-agents-self-manage-webhooks.md)).

## The shared contract (both families)

Every adapter — push or pull — runs the **same back half**:

```
                          ┌─────────────── shared back half ───────────────┐
push:  inbound HTTP  ──▶  verify → derive idempotency key → normalize → create todo (dedup) → persist event
pull:  consume queue ──▶  verify → derive idempotency key → normalize → create todo (dedup) → persist event → ACK source
```

The **todo contract is identical regardless of transport**: at-least-once delivery + idempotent dedup
([ADR-007](../adr/ADR-007-todos-as-core-primitive.md)). Only the **front half** (the transport) and the
**pull-only trailing ack** differ.

- **Idempotency key** dedups redeliveries into one todo ([todos spec](todos.md)); if a non-terminal todo
  with the derived key already exists in the target queue, ingestion **returns the existing todo** and
  creates nothing new.
- **Normalize** produces the same todo shape from any adapter — a push and a pull delivery of the *same
  logical event* yield the identical todo.
- **Persist event** keeps the history/audit surface ([ADR-005](../adr/ADR-005-mcp-tool-and-resource-contract.md))
  regardless of family.

## Push family — webhooks (inbound HTTP)

Verified by signature/token at receive; switchboard owns verification and cannot be made to weaken it,
including for agent-created webhooks ([ADR-012](../adr/ADR-012-agents-self-manage-webhooks.md)).

| Adapter | Trust mode | Verification (authoritative in [ADR-003](../adr/ADR-003-per-provider-ingestion-and-trust-model.md)) |
|---------|-----------|------------------------------------------------------------------------------------------------------|
| **GitHub** | `signed` | `X-Hub-Signature-256`, HMAC-SHA256 over raw body, `sha256=` prefix, constant-time compare. Fail ⇒ 401, not persisted, no todo. |
| **Stripe** | `signed` | `Stripe-Signature` (`t=` + `v1=`) HMAC-SHA256 over `"{t}.{body}"`; reject if `|now − t| > 300s`. |
| **Slack** | `signed` | `X-Slack-Signature` + `X-Slack-Request-Timestamp`, `v0=` HMAC-SHA256 over `"v0:{ts}:{body}"`; reject stale. |
| **Docker Hub** | `unverified` | No native signing scheme ⇒ routed through the **generic** endpoint. Labeled unverified everywhere. |
| **generic** (`/webhooks/generic/{name}`) | `unverified` | No signature; a weak per-instance path/query **token** (bozo filter, constant-time compare). Opt-in, disabled by default. |

**Push "ack" is the HTTP response.** A non-2xx makes the *sender* retry — that is the source of
at-least-once — and idempotency dedup collapses the retries. There is no source-side message to remove.

### Idempotency-key derivation (push)

| Adapter | Idempotency key |
|---------|-----------------|
| GitHub | `gh:` + `X-GitHub-Delivery` |
| Stripe | `stripe:` + event `id` |
| Slack | `slack:` + `X-Slack-Request-Timestamp` + body hash (Slack has no delivery id) |
| Docker Hub / generic | `generic:{name}:` + `sha256(body)` (best-effort; no provider id) |

## Pull family — queue adapters (the app consumes)

No HTTP request and no signature — **trust is the connection itself** (ACL / TLS), per
[ADR-003](../adr/ADR-003-per-provider-ingestion-and-trust-model.md)'s `redis` trust mode. **Redis is the
reference implementation**; the same shape extends to **SQS / NATS / AMQP** later
([ADR-014](../adr/ADR-014-ingestion-adapters-push-pull.md)).

### The ack-coupling rule (store-then-ack)

A pull adapter **must not ack/remove the source message until the resulting todo is durably stored.**

```
consume message
   → derive idempotency key FROM the source message id
   → create todo (dedup)
   → todo durably in SQLite (ADR-002)        ← the durability boundary
   → THEN ack/remove the source message
```

A crash **between** consume and todo-store leaves the source message **un-acked** ⇒ it is
**redelivered** ⇒ dedup (idempotency key = source message id) collapses it ⇒ **no duplicate todo, no
lost message.** Thus **the todo's durability — and thereafter its lease/ack
([ADR-007](../adr/ADR-007-todos-as-core-primitive.md)) — *is* the queue's ack.** Same at-least-once +
idempotent contract as the push family, different transport.

- **Default: ack-on-store** (todo durability is the boundary). A pull adapter *may* instead defer the
  source ack until the todo is **completed** for stronger end-to-end coupling, at the cost of holding
  redelivery state longer — an **[open question](../README.md)** (default: ack-on-store).

### Reference adapter — Redis

| Mode | Consume / ack primitive | Durability |
|------|-------------------------|------------|
| **Streams + consumer group** (preferred) | `XREADGROUP` … then `XACK` after todo stored | per-message ack + redelivery on restart |
| **Reliable list** | `BRPOPLPUSH` onto a processing list, then `LREM` after todo stored | reliable-queue pattern, same store-then-ack |
| **Pub/sub** | fire-and-forget, no ack | **no redelivery** — only where loss is acceptable; not recommended for durable work |

This decides [ADR-003](../adr/ADR-003-per-provider-ingestion-and-trust-model.md)'s deferred
"pub/sub vs. consumer-group stream" sub-decision in favor of an **ack-capable** mode for durable work.

### Idempotency-key derivation (pull)

| Adapter | Idempotency key |
|---------|-----------------|
| Redis stream | `redis:{stream}:` + entry id (`XADD` id) |
| Redis list | `redis:{list}:` + `sha256(body)` (lists carry no per-message id) |
| Redis pub/sub | `redis:{channel}:` + `sha256(body)` (best-effort; delivery not guaranteed) |
| SQS / NATS / AMQP (later) | `<adapter>:{queue}:` + the transport's native message id |

### Future pull adapters

The same family absorbs other queues by mapping their native ack primitive to store-then-ack:
SQS (visibility timeout → `DeleteMessage`), NATS JetStream (`ack`), AMQP (`basic.ack`). No model change
— add an adapter ([ADR-014](../adr/ADR-014-ingestion-adapters-push-pull.md)).

## Routing rules (shared)

A **routing rule** maps a normalized delivery to zero-or-more todos, regardless of family. `source`
names the **adapter instance** (e.g. `github`, `generic:dockerhub`, `redis:deploys`). Rules evaluate in
order; each match produces one todo (fan-out allowed).

```json
{
  "type": "object",
  "required": ["match", "target"],
  "properties": {
    "match": {
      "type": "object",
      "description": "All present criteria must match (AND).",
      "properties": {
        "source":     { "type": "string", "description": "Adapter instance, e.g. 'github', 'generic:dockerhub', 'redis:deploys'." },
        "event_type": { "type": "string", "description": "Declared type, e.g. 'pull_request'." },
        "filter":     { "type": "string", "description": "Optional expression over payload fields, e.g. action=='opened'." }
      }
    },
    "target": {
      "type": "object",
      "required": ["queue"],
      "properties": {
        "queue":    { "type": "string", "description": "Destination queue." },
        "assignee": { "type": ["string", "null"], "description": "Persona/endpoint id for direct assignment; null ⇒ pool queue." },
        "kind":     { "type": ["string", "null"], "description": "Todo 'kind' stamp." },
        "title":    { "type": ["string", "null"], "description": "Template for the todo title, e.g. 'PR #{number} {action}'." }
      }
    },
    "stop": { "type": "boolean", "default": false, "description": "If true, stop evaluating further rules on a match." }
  }
}
```

- **No matching rule ⇒ no todo** (the delivery is still stored for history/audit,
  [ADR-005](../adr/ADR-005-mcp-tool-and-resource-contract.md)); a configurable **default rule** may route
  unmatched deliveries to a catch-all queue.
- **Directly-assigned** todos (rule sets `assignee`) can be claimed only by that persona; unassigned
  todos land in a **pool queue** drained competitively ([todos spec](todos.md)).
- The todo's `payload_ref` points at the stored (sanitized) delivery; the payload is not copied inline.

### Example

```json
// PUSH: GitHub pull_request opened → reviews queue
{ "match": { "source": "github", "event_type": "pull_request", "filter": "action=='opened'" },
  "target": { "queue": "reviews", "kind": "pull_request", "title": "PR #{number} opened in {repo}" } }

// PULL: a message on the Redis 'deploys' stream → deploys pool
{ "match": { "source": "redis:deploys" },
  "target": { "queue": "deploys", "kind": "deploy", "title": "Deploy request {service}@{ref}" } }
```

## Cross-references

- Adapter abstraction + ack coupling (decision): [ADR-014](../adr/ADR-014-ingestion-adapters-push-pull.md).
- Trust modes & per-source verification (authoritative): [ADR-003](../adr/ADR-003-per-provider-ingestion-and-trust-model.md).
- Todo object, dedup, lease/ack lifecycle: [todos spec](todos.md), [ADR-007](../adr/ADR-007-todos-as-core-primitive.md).
- Agent-created webhooks (push family) & the ceiling: [ADR-012](../adr/ADR-012-agents-self-manage-webhooks.md), [agent-mcp-tools spec](agent-mcp-tools.md).
- Secrets (webhook signing secrets, queue connection URLs): [ADR-004](../adr/ADR-004-secrets-management-openbao-approle.md).
- Delivery/history surface: [ADR-002](../adr/ADR-002-sqlite-persistence-and-retention.md), [ADR-005](../adr/ADR-005-mcp-tool-and-resource-contract.md), [`openapi.yaml`](openapi.yaml).
