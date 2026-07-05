# switchboard — Todo Object & State Machine

Authoritative schema and lifecycle for the **todo**, switchboard's core agent-facing primitive
([ADR-007](../adr/ADR-007-todos-as-core-primitive.md)). A todo is a durable work-item — created by a
producer (a webhook, the Redis consumer, another agent, or switchboard itself), claimed and completed
by a consumer (an agent persona draining its queue).

The verbs that drive this state machine are in the [agent-mcp-tools spec](agent-mcp-tools.md). How
push (webhook) and pull (queue) adapters normalize deliveries into todos is in the
[ingestion-adapters spec](ingestion-adapters.md) ([ADR-014](../adr/ADR-014-ingestion-adapters-push-pull.md)).
Todos are persisted in
the same PostgreSQL layer as events ([ADR-002](../adr/ADR-002-postgres-persistence-and-retention.md)) and
subject to the same retention posture.

## Conventions

- **Time:** all timestamps are ISO-8601 UTC strings.
- **At-least-once:** a todo may be delivered to a consumer more than once (crash/lease-expiry).
  Consumers **MUST** be idempotent. Exactly-once is not offered.
- **Single-ownership while claimed:** at most one consumer holds a valid lease on a todo at any time.
- **No secrets in a todo.** The `payload_ref` points at stored, sanitized event data
  ([ADR-003](../adr/ADR-003-per-provider-ingestion-and-trust-model.md)); signing secrets never appear.
- **The todo is the delivery of record.** A todo may *also* be announced to an attached session via a
  best-effort Channels push, but that push never replaces claiming from the queue — an offline session
  loses nothing ([ADR-013](../adr/ADR-013-channels-push-delivery.md), [channel-delivery spec](channel-delivery.md)).

## Todo object

```json
{
  "type": "object",
  "required": ["id", "queue", "source", "state", "idempotency_key", "attempt", "max_attempts", "created_at", "updated_at"],
  "properties": {
    "id":              { "type": "string", "description": "Stable todo id (ULID/uuid)." },
    "queue":           { "type": "string", "description": "Target queue/topic this todo lives in (e.g. 'reviews', 'deploys')." },
    "source":          { "type": "string", "description": "Producer origin, e.g. 'github', 'generic:dockerhub', 'redis:deploys', 'agent:<id>', 'switchboard:friend-approval'." },
    "kind":            { "type": ["string", "null"], "description": "Optional producer-declared work type, e.g. 'pull_request', 'deploy', 'friend_approval'." },
    "title":           { "type": ["string", "null"], "description": "Short human/agent-legible summary for list views." },
    "payload_ref":     { "type": ["string", "null"], "description": "Reference to the stored payload — an event id (ADR-002/005) or a blob key. NOT the payload inline." },
    "payload":         { "type": ["object", "null"], "description": "Optional small inline structured payload for producers with no stored event (e.g. friend-approval details)." },
    "idempotency_key": { "type": "string", "description": "Dedup key. For webhooks: the provider delivery id (X-GitHub-Delivery, Stripe event id, …). Unique among non-terminal todos in a queue." },
    "assignee":        { "type": ["string", "null"], "description": "Persona/endpoint id this todo is directly assigned to. Null ⇒ pool/topic queue drained competitively." },
    "state":           { "$ref": "#/$defs/TodoState" },
    "owner":           { "type": ["string", "null"], "description": "Endpoint/persona id currently holding the lease. Null unless state=claimed." },
    "lease_expires_at":{ "type": ["string", "null"], "format": "date-time", "description": "When the current lease lapses; null unless claimed." },
    "attempt":         { "type": "integer", "minimum": 0, "description": "Times this todo has entered 'claimed'." },
    "max_attempts":    { "type": "integer", "minimum": 1, "description": "Attempt cap; beyond it a failed todo dead-letters." },
    "result":          { "type": ["object", "null"], "description": "Consumer-supplied completion/failure detail (set on done/failed)." },
    "created_at":      { "type": "string", "format": "date-time" },
    "updated_at":      { "type": "string", "format": "date-time" },
    "claimed_at":      { "type": ["string", "null"], "format": "date-time" },
    "completed_at":    { "type": ["string", "null"], "format": "date-time" }
  },
  "additionalProperties": false
}
```

### `TodoState`
```json
{ "type": "string", "enum": ["pending", "claimed", "done", "failed"] }
```

`done` and `failed` are terminal. `retry` is a **transition**, not a state — a failed/lease-expired
todo transitions back to `pending` with `attempt` incremented (until `max_attempts`).

## State machine

```
   create ─────────────▶ pending
                            │
             claim (atomic) │  sets owner + lease_expires_at, attempt++
                            ▼
                         claimed ──── complete ───▶ done      (terminal)
                            │
             ┌──────────────┼───────────────┐
             │ fail          │ lease expiry  │ release
             ▼               ▼               ▼
     attempt<max?      attempt<max?      pending
     ├ yes → pending   ├ yes → pending   (voluntarily give up lease)
     └ no  → failed    └ no  → failed
                       (dead-letter)     (dead-letter)
```

### Transitions

| From | Event | To | Effects / guards |
|------|-------|----|------------------|
| — | `create` | `pending` | Dedup on `idempotency_key`: if a non-terminal todo with the same key+queue exists, **no new todo** — the existing one is returned. |
| `pending` | `claim` | `claimed` | Atomic. Sets `owner`, `lease_expires_at = now + lease_ttl`, `claimed_at`, `attempt++`. Fails if already claimed (lost race). Respects `assignee` (only the assignee may claim a directly-assigned todo). |
| `claimed` | `heartbeat` | `claimed` | Owner extends `lease_expires_at`. Only the lease owner may heartbeat. |
| `claimed` | `complete` | `done` | Owner acks success. Sets `result`, `completed_at`. Only the lease owner. |
| `claimed` | `fail` | `pending` or `failed` | Owner reports failure. If `attempt < max_attempts` → `pending` (retry); else → `failed` (dead-letter). Sets `result`. |
| `claimed` | `release` | `pending` | Owner voluntarily gives up the lease (does not consume an attempt beyond the one already counted). |
| `claimed` | `lease expiry` | `pending` or `failed` | Detected lazily (on next claim scan) or by a sweeper. If `attempt < max_attempts` → `pending` (re-claimable — **crash safety**); else → `failed`. |
| `failed` | `retry` (operator/agent) | `pending` | Manual re-queue of a dead-lettered todo; resets/raises the attempt budget per policy. |

### Guarantees

- **Crash safety.** A consumer that claims then dies never strands the todo: the lease lapses and it
  returns to `pending`, re-claimable by anyone whose scope covers the queue
  ([ADR-007](../adr/ADR-007-todos-as-core-primitive.md)).
- **No double-processing.** While `claimed` with a live lease, no other consumer can claim it.
- **Dedup.** `create` is idempotent on `idempotency_key` within a queue — at-least-once webhook
  deliveries collapse into one todo.
- **Bounded retries.** `attempt` is capped by `max_attempts`; exhaustion dead-letters to `failed`
  rather than looping forever.

## Example lifecycle

```json
// 1. Webhook produces a todo (dedup key = GitHub delivery id)
{ "id": "01J…A", "queue": "reviews", "source": "github", "kind": "pull_request",
  "title": "PR #482 opened in joestump/switchboard", "payload_ref": "event:4207",
  "idempotency_key": "gh:8f2c…", "assignee": null, "state": "pending",
  "attempt": 0, "max_attempts": 5, "created_at": "2026-07-05T17:41:09Z", "updated_at": "…" }

// 2. Reviewer persona claims (lease 120s)
{ "…": "…", "state": "claimed", "owner": "persona:reviewer@agent-7",
  "lease_expires_at": "2026-07-05T17:43:09Z", "attempt": 1, "claimed_at": "2026-07-05T17:41:12Z" }

// 3. Completes
{ "…": "…", "state": "done", "result": { "review": "approved", "comment_id": 991 },
  "completed_at": "2026-07-05T17:42:01Z" }
```

## Cross-references

- Primitive rationale & lifecycle decision: [ADR-007](../adr/ADR-007-todos-as-core-primitive.md).
- Verbs driving the machine (`list_todos`/`claim`/`complete`/`fail`/`create_for`): [agent-mcp-tools spec](agent-mcp-tools.md).
- Producers → todos (push/pull adapters, routing, idempotency-key derivation, pull ack-coupling): [ingestion-adapters spec](ingestion-adapters.md), [ADR-014](../adr/ADR-014-ingestion-adapters-push-pull.md), [ADR-003](../adr/ADR-003-per-provider-ingestion-and-trust-model.md).
- Who may drain which queue (scope), and how ownership traces to a human: [ADR-008](../adr/ADR-008-human-principal-vended-endpoints.md), [accounts-and-endpoints spec](accounts-and-endpoints.md).
- Persistence & retention of terminal todos: [ADR-002](../adr/ADR-002-postgres-persistence-and-retention.md).
