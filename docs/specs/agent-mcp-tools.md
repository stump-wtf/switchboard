# switchboard — Agent MCP Tool Surface (vended endpoint)

Authoritative contract for the MCP tools exposed on a **vended agent endpoint**
([ADR-008](../adr/ADR-008-human-principal-vended-endpoints.md)). This is the *work* surface — todos,
webhook self-management, and friending — distinct from the read-only event-history surface of
[`mcp-tools.md`](mcp-tools.md) ([ADR-005](../adr/ADR-005-mcp-tool-and-resource-contract.md)). A given
persona ([ADR-009](../adr/ADR-009-personas-as-scoped-agent-cards.md)) sees only the subset of these
verbs in its **verb allowlist**, acting only on its **granted queues**.

## Conventions

- **Every call is scoped.** A verb not in the endpoint's allowlist is not exposed; a queue outside the
  grant is denied with `forbidden`. Scope is enforced at the boundary
  ([ADR-008](../adr/ADR-008-human-principal-vended-endpoints.md)).
- **Structured output.** Every tool declares an input JSON Schema and returns SDK structured output.
- **Idempotent producers.** `create_for`/`create_webhook`-produced todos dedup on idempotency key
  ([todos spec](todos.md)).
- **Time:** ISO-8601 UTC. **Errors:** stable `code` + human `message`, never secret-bearing.
- **Push is a doorbell, not a verb.** The same vended endpoint may also carry the Claude Code Channels
  `claude/channel` capability and *push* a `notifications/claude/channel` wake when a todo is
  created/assigned — but the agent still **claims/completes via the verbs below**. Push is best-effort
  and never gates correctness ([ADR-013](../adr/ADR-013-channels-push-delivery.md), [channel-delivery spec](channel-delivery.md)).

## Verb groups

| Group | Verbs | Gated by |
|-------|-------|----------|
| **Todos** | [`list_todos`](#list_todos), [`claim`](#claim), [`complete`](#complete), [`fail`](#fail), [`create_for`](#create_for) | queue grant + allowlist |
| **Webhooks** | [`create_webhook`](#create_webhook), [`list_webhooks`](#list_webhooks), [`rotate_webhook`](#rotate_webhook), [`delete_webhook`](#delete_webhook) | vended webhook **ceiling** ([ADR-012](../adr/ADR-012-agents-self-manage-webhooks.md)) |
| **Friending** | [`send_friend_request`](#send_friend_request), [`list_pending_approvals`](#list_pending_approvals), [`approve`](#approve) / [`deny`](#deny), [`revoke`](#revoke) | human-consent verbs ([ADR-010](../adr/ADR-010-a2a-discovery-human-vended-friending.md)) |

---

## Todos

### `list_todos`
Drain view: todos visible to this endpoint (its granted queues; directly-assigned todos to this
persona; pending pool todos). Compact rows; cursor-paginated.

```
list_todos(queue?: string, state?: TodoState, kind?: string,
           mine_only?: bool = false, limit?: int = 50, cursor?: string)
  → { todos: TodoSummary[], next_cursor?: string }
```
`TodoSummary`: `id`, `queue`, `source`, `kind`, `title`, `state`, `assignee`, `attempt`, `created_at`.

### `claim`
Atomically claim a `pending` todo, acquiring a lease. Fails `conflict` if already claimed,
`forbidden` if outside scope or not the assignee.
```
claim(id: string, lease_ttl_seconds?: int)
  → { id, state: "claimed", owner, lease_expires_at, attempt }
```

### `complete`
Ack success on a todo this endpoint holds the lease for. Terminal → `done`.
```
complete(id: string, result?: object)
  → { id, state: "done", completed_at }
```

### `fail`
Report failure on a leased todo. Retries (→ `pending`) if attempts remain, else dead-letters
(→ `failed`).
```
fail(id: string, reason: string, result?: object)
  → { id, state: "pending" | "failed", attempt }
```
Helpers (optional, code-session): `heartbeat(id, lease_ttl_seconds?)` to extend a lease and
`release(id)` to voluntarily requeue ([todos spec](todos.md)).

### `create_for`
Create a todo **for another agent/queue** — the durable transport for cross-agent work
([ADR-010](../adr/ADR-010-a2a-discovery-human-vended-friending.md)). Allowed only for a `(target,
queue)` this endpoint has been **vended access to** (via a friend grant or its own queues). Dedups on
`idempotency_key`.
```
create_for(target_queue: string, title: string, kind?: string,
           payload?: object, idempotency_key?: string, assignee?: string)
  → Todo   // the created (or pre-existing, if key matched) todo
```
> Cross-agent work lands here as a **todo**, never as an A2A direct peer task
> ([ADR-010](../adr/ADR-010-a2a-discovery-human-vended-friending.md)).

---

## Webhooks (self-management within a vended ceiling)

Bounded by the endpoint's **ceiling** — max count, allowed source types, allowed target queues
([ADR-012](../adr/ADR-012-agents-self-manage-webhooks.md)). Switchboard mints the signing secret,
stores it in OpenBao ([ADR-004](../adr/ADR-004-secrets-management-openbao-approle.md)), and owns
verification + idempotency ([ADR-003](../adr/ADR-003-per-provider-ingestion-and-trust-model.md)). The
agent receives **only the URL**, never the secret.

### `create_webhook`
```
create_webhook(source_type: string, target_queue: string, name?: string)
  → { webhook_id, source_type, target_queue, ingest_url, trust_mode }
```
Refused with `ceiling_exceeded` (count), `forbidden_source_type`, or `forbidden` (queue) when outside
the ceiling. `ingest_url` is what the agent hands to the producer. **No secret is returned.**

### `list_webhooks`
```
list_webhooks() → { webhooks: WebhookInfo[], ceiling: { max, allowed_source_types, allowed_queues, used } }
```
`WebhookInfo`: `webhook_id`, `source_type`, `target_queue`, `ingest_url`, `trust_mode`,
`secret_status` (`configured`/`none-by-design`), `created_at`. Never the secret value.

### `rotate_webhook`
Mint a new signing secret (and URL if URL encodes a token); retire the old.
```
rotate_webhook(webhook_id: string) → { webhook_id, ingest_url, trust_mode }
```

### `delete_webhook`
```
delete_webhook(webhook_id: string) → { webhook_id, deleted: true }
```

---

## Friending (human-consent verbs)

Discovery is A2A ([personas-and-agent-cards spec](personas-and-agent-cards.md)); these verbs drive the
**request → human-approval-todo → approve(=vend) → revoke** flow
([ADR-010](../adr/ADR-010-a2a-discovery-human-vended-friending.md), [friend-requests spec](friend-requests.md)).

### `send_friend_request`
Ask a target persona for a **requested scope** (queues + verbs). Creates a **pending edge** that grants
nothing. Must carry verifiable **OIDC-signed provenance** of the requesting human
([ADR-011](../adr/ADR-011-identity-assurance-oidc-passkey-deferred.md)); a request without it is
rejected `unauthenticated`.
```
send_friend_request(target: string, requested_scope: { queues: string[], verbs: string[] }, reason: string)
  → { request_id, state: "pending" }
```

### `list_pending_approvals`
Approvals awaiting **this human's** decision (surfaced both here and as todos in the human's queue —
switchboard dogfooding, [ADR-007](../adr/ADR-007-todos-as-core-primitive.md)).
```
list_pending_approvals() → { approvals: ApprovalRequest[] }
```
`ApprovalRequest`: `request_id`, `from_human`, `from_persona`, `requested_scope`, `reason`,
`provenance_verified` (bool), `created_at`.

### `approve` / `deny`
The human decides. **Approve is the vend** — it may **narrow** the requested scope, and minting the
scoped endpoint happens on approve ([ADR-008](../adr/ADR-008-human-principal-vended-endpoints.md)).
Granted access is **per-direction** and revocable.
```
approve(request_id: string, granted_scope?: { queues: string[], verbs: string[] })  // omit ⇒ grant as requested; narrower ⇒ narrow
  → { request_id, state: "approved", endpoint_id, granted_scope }
deny(request_id: string, reason?: string)
  → { request_id, state: "denied" }
```
`granted_scope` MUST be ⊆ `requested_scope`; a superset is rejected `invalid_argument`.

### `revoke`
Kill a previously-granted edge (= kill the vended endpoint). Instant, one-directional.
```
revoke(edge_id: string) → { edge_id, revoked: true }
```

---

## Errors

| `code` | When |
|--------|------|
| `forbidden` | Verb not in allowlist, or queue/target outside grant. |
| `conflict` | `claim` on an already-claimed todo (lost race). |
| `not_found` | Unknown todo/webhook/request id. |
| `invalid_argument` | Bad input — e.g. `granted_scope ⊄ requested_scope`, malformed cursor. |
| `ceiling_exceeded` / `forbidden_source_type` | Webhook create beyond count/type ceiling ([ADR-012](../adr/ADR-012-agents-self-manage-webhooks.md)). |
| `unauthenticated` | Friend request missing valid OIDC-signed human provenance ([ADR-011](../adr/ADR-011-identity-assurance-oidc-passkey-deferred.md)). |
| `internal` | Unexpected server-side failure. |

## Cross-references

- Todo object & state machine: [todos spec](todos.md), [ADR-007](../adr/ADR-007-todos-as-core-primitive.md).
- Endpoint scope (queues + verb allowlist), vend/revoke: [accounts-and-endpoints spec](accounts-and-endpoints.md), [ADR-008](../adr/ADR-008-human-principal-vended-endpoints.md).
- Webhook ceiling & verification: [ADR-012](../adr/ADR-012-agents-self-manage-webhooks.md), [webhook-ingestion spec](webhook-ingestion.md), [ADR-003](../adr/ADR-003-per-provider-ingestion-and-trust-model.md).
- Friending flow & provenance: [friend-requests spec](friend-requests.md), [ADR-010](../adr/ADR-010-a2a-discovery-human-vended-friending.md), [ADR-011](../adr/ADR-011-identity-assurance-oidc-passkey-deferred.md).
- Read-only event-history surface (separate): [`mcp-tools.md`](mcp-tools.md), [ADR-005](../adr/ADR-005-mcp-tool-and-resource-contract.md).
