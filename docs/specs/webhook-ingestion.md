# switchboard — Webhook Ingestion & Routing to Todos

How inbound events become todos. This spec pins the supported **sources**, the **verification** applied
per source, and the **routing rules** that map a verified event to a target queue/assignee and produce
a todo ([ADR-007](../adr/ADR-007-todos-as-core-primitive.md)). Trust semantics are decided in
[ADR-003](../adr/ADR-003-per-provider-ingestion-and-trust-model.md); this spec adds the
producer→todo step on top of it. Secrets come from OpenBao
([ADR-004](../adr/ADR-004-secrets-management-openbao-approle.md)); agent-created webhooks are bounded by
a ceiling ([ADR-012](../adr/ADR-012-agents-self-manage-webhooks.md)).

## Pipeline

```
receive → verify (per source) → persist event (ADR-002) → route → create todo(s) (dedup) → drain by agents
```

Verification and persistence are unchanged from the event-store MVP
([ADR-003](../adr/ADR-003-per-provider-ingestion-and-trust-model.md)/[ADR-002](../adr/ADR-002-sqlite-persistence-and-retention.md)).
The new steps are **route** and **create todo(s)**.

## Supported sources & verification

| Source | Trust mode | Verification (switchboard-owned, mandatory where applicable) |
|--------|-----------|--------------------------------------------------------------|
| **GitHub** | `signed` | `X-Hub-Signature-256`, HMAC-SHA256 over raw body, `sha256=` prefix, constant-time compare. Fail ⇒ 401, not persisted, no todo. |
| **Stripe** | `signed` | `Stripe-Signature` (`t=` + `v1=`) HMAC-SHA256 over `"{t}.{body}"`; reject if `|now − t| > 300s`. |
| **Slack** | `signed` | `X-Slack-Signature` + `X-Slack-Request-Timestamp`, `v0=` HMAC-SHA256 over `"v0:{ts}:{body}"`; reject stale. |
| **Docker Hub** | `unverified` | No native signing scheme ⇒ routed through the **generic** endpoint. Labeled unverified everywhere. |
| **generic** (`/webhooks/generic/{name}`) | `unverified` | No signature; a weak per-instance path/query **token** (bozo filter, constant-time compare). Opt-in, disabled by default. |
| **Redis** (queue consumer) | `redis` | No HTTP/signature. Trust = the Redis connection ACL/TLS; `verify_detail` names the ACL user. |

All per-source verification detail is authoritative in
[ADR-003](../adr/ADR-003-per-provider-ingestion-and-trust-model.md); switchboard owns it regardless of
whether the webhook was configured by an operator or self-created by an agent
([ADR-012](../adr/ADR-012-agents-self-manage-webhooks.md)) — an agent cannot weaken it.

## Idempotency-key derivation

The todo's `idempotency_key` ([todos spec](todos.md)) is derived per source so at-least-once redelivery
collapses into one todo:

| Source | Idempotency key |
|--------|-----------------|
| GitHub | `gh:` + `X-GitHub-Delivery` |
| Stripe | `stripe:` + event `id` |
| Slack | `slack:` + `X-Slack-Request-Timestamp` + body hash (Slack has no delivery id) |
| Docker Hub / generic | `generic:{name}:` + `sha256(body)` (best-effort; no provider id) |
| Redis | `redis:{channel}:` + message id (`XADD` id for streams) or `sha256(body)` for pub/sub |

If a non-terminal todo with the derived key already exists in the target queue, ingestion **returns the
existing todo** and creates nothing new ([ADR-007](../adr/ADR-007-todos-as-core-primitive.md)).

## Routing rules

A **routing rule** maps a received event to zero-or-more todos. Rules are evaluated in order; each rule
that matches produces one todo (fan-out to multiple queues is allowed).

```json
{
  "type": "object",
  "required": ["match", "target"],
  "properties": {
    "match": {
      "type": "object",
      "description": "All present criteria must match (AND).",
      "properties": {
        "source":     { "type": "string", "description": "e.g. 'github', 'generic:dockerhub', 'redis:deploys'." },
        "event_type": { "type": "string", "description": "Provider-declared type, e.g. 'pull_request'." },
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

- **No matching rule ⇒ no todo** (the event is still stored for history/audit,
  [ADR-005](../adr/ADR-005-mcp-tool-and-resource-contract.md)). A configurable **default rule** may route
  unmatched events to a catch-all queue.
- **Directly-assigned** todos (rule sets `assignee`) can be claimed only by that persona; unassigned
  todos land in a **pool queue** drained competitively ([todos spec](todos.md)).
- The todo's `payload_ref` points at the stored (sanitized) event; the payload is not copied inline.

### Example

```json
// GitHub pull_request opened → reviews queue, titled, kind=pull_request
{ "match": { "source": "github", "event_type": "pull_request", "filter": "action=='opened'" },
  "target": { "queue": "reviews", "kind": "pull_request", "title": "PR #{number} opened in {repo}" } }

// Docker Hub push (unverified) → deploys pool, no direct assignee
{ "match": { "source": "generic:dockerhub" },
  "target": { "queue": "deploys", "kind": "image_push", "title": "New image tag {push_data.tag}" } }
```

## Redis path

The Redis consumer feeds the *same* route → create-todo step with no HTTP endpoint
([ADR-003](../adr/ADR-003-per-provider-ingestion-and-trust-model.md)), proving the producer abstraction
generalizes beyond HTTP. Consumer-group streams are preferred so messages survive an app restart
(ADR-003 deferred sub-decision).

## Cross-references

- Trust modes & per-source verification (authoritative): [ADR-003](../adr/ADR-003-per-provider-ingestion-and-trust-model.md).
- Todo object, dedup, lifecycle: [todos spec](todos.md), [ADR-007](../adr/ADR-007-todos-as-core-primitive.md).
- Agent-created webhooks & the ceiling: [ADR-012](../adr/ADR-012-agents-self-manage-webhooks.md), [agent-mcp-tools spec](agent-mcp-tools.md).
- Secrets (signing secrets, Redis URL): [ADR-004](../adr/ADR-004-secrets-management-openbao-approle.md).
- Event persistence & history surface: [ADR-002](../adr/ADR-002-sqlite-persistence-and-retention.md), [ADR-005](../adr/ADR-005-mcp-tool-and-resource-contract.md), [`openapi.yaml`](openapi.yaml).
