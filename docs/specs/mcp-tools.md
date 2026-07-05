# switchboard — MCP Tool & Resource Contract

Authoritative contract for the MCP surface of **switchboard**, served by the official `mcp` Python
SDK from the same Starlette process as the web UI ([ADR-001](../adr/ADR-001-web-stack-go-htmx-pico.md))
and reading the same PostgreSQL layer ([ADR-002](../adr/ADR-002-postgres-persistence-and-retention.md)).

This document is the source of truth for tool/resource **schemas**. The *shape* decisions behind it
are in [ADR-005](../adr/ADR-005-mcp-tool-and-resource-contract.md); the trust semantics are in
[ADR-003](../adr/ADR-003-per-provider-ingestion-and-trust-model.md). The `EventSummary` /
`EventDetail` / `ProviderStatus` shapes here are identical to their counterparts in
[`openapi.yaml`](openapi.yaml) and [`asyncapi.yaml`](asyncapi.yaml) — the HTTP, SSE, and MCP surfaces
must not drift.

## Conventions

- **Server name:** `switchboard`.
- **Transport:** whatever the code session wires (stdio and/or the SDK's Streamable HTTP mount inside
  the Starlette app). Out of scope for this contract.
- **Structured output:** every tool declares an input JSON Schema and returns SDK **structured
  output** validated against the output schemas below — not free-form text.
- **Trust is always present.** Every event object carries `trust_mode`, `verified`, and (in detail)
  `verify_detail`, so an agent can always tell a signed, verified event from an unverified/Redis one.
- **No secrets cross the boundary.** Responses contain **sanitized** headers only; signing secrets and
  full signature header values are never returned ([ADR-002](../adr/ADR-002-postgres-persistence-and-retention.md)/[ADR-004](../adr/ADR-004-secrets-management-openbao-approle.md)).
- **Time:** all timestamps are ISO-8601 UTC strings.
- **Errors:** tools raise MCP tool errors with a stable `code` and a human `message`. `message` never
  contains secret material. See [Errors](#errors).

## Shared schemas

### `TrustMode`
```json
{ "type": "string", "enum": ["signed", "unverified", "redis"] }
```

### `EventSummary`
Compact row for `list_webhook_events` and the `events/recent` resource. No payload/headers.
```json
{
  "type": "object",
  "required": ["id", "provider", "trust_mode", "verified", "payload_size", "received_at"],
  "properties": {
    "id":           { "type": "integer", "description": "Event primary key." },
    "provider":     { "type": "string", "description": "e.g. 'github', 'stripe', 'generic:dockerhub', 'redis:deploys'." },
    "event_type":   { "type": ["string", "null"], "description": "Provider-declared type; null if unknown." },
    "trust_mode":   { "$ref": "#/$defs/TrustMode" },
    "verified":     { "type": "boolean", "description": "true ONLY for a signed provider whose signature passed." },
    "payload_size": { "type": "integer", "description": "Raw body size in bytes." },
    "received_at":  { "type": "string", "format": "date-time" }
  },
  "additionalProperties": false
}
```

### `EventDetail`
Full record for `get_webhook_event` — `EventSummary` plus:
```json
{
  "allOf": [{ "$ref": "#/$defs/EventSummary" }],
  "properties": {
    "verify_detail": { "type": ["string", "null"], "description": "e.g. 'hmac-sha256 ok', 'unverified by design', 'redis acl: deploy-bot'." },
    "external_id":   { "type": ["string", "null"], "description": "Provider delivery id (idempotency key)." },
    "content_type":  { "type": ["string", "null"] },
    "source_ip":     { "type": ["string", "null"], "description": "null for Redis-ingested events." },
    "headers":       { "type": "object", "additionalProperties": { "type": "string" },
                       "description": "SANITIZED headers; signature/secret headers redacted to «redacted»." },
    "payload":       { "type": "string", "description": "Raw body exactly as received." }
  }
}
```

### `ProviderStatus`
```json
{
  "type": "object",
  "required": ["name", "kind", "trust_mode", "enabled", "secret_status"],
  "properties": {
    "name":          { "type": "string" },
    "kind":          { "type": "string", "enum": ["signed", "generic", "redis"] },
    "trust_mode":    { "$ref": "#/$defs/TrustMode" },
    "enabled":       { "type": "boolean" },
    "secret_status": { "type": "string", "enum": ["configured", "missing", "none-by-design"],
                       "description": "Runtime status from OpenBao (ADR-004). Never the secret itself." },
    "path":          { "type": ["string", "null"], "description": "Webhook route path (HTTP providers)." },
    "channel":       { "type": ["string", "null"], "description": "Redis channel/stream (redis providers)." }
  },
  "additionalProperties": false
}
```

## Tools

| Tool | Side effects | Summary |
|------|--------------|---------|
| [`list_webhook_events`](#list_webhook_events) | none (read) | Paginated event summaries with filters. |
| [`get_webhook_event`](#get_webhook_event) | none (read) | Full detail for one event. |
| [`replay_webhook_event`](#replay_webhook_event) | **outbound HTTP POST** | Re-emit a stored payload to a target. |
| [`list_providers`](#list_providers) | none (read) | Configured providers + status. |

---

### `list_webhook_events`

Paginated, filtered event summaries. Order is `received_at DESC, id DESC` (stable). Pagination is
cursor-based ([ADR-005](../adr/ADR-005-mcp-tool-and-resource-contract.md)) so concurrent inserts on an
always-on receiver never cause duplicate/gap pages.

**Input schema**
```json
{
  "type": "object",
  "properties": {
    "provider":   { "type": "string", "description": "Filter by provider slug." },
    "event_type": { "type": "string", "description": "Filter by provider-declared event type." },
    "since":      { "description": "Lower bound: ISO-8601 timestamp OR an event id.",
                    "oneOf": [{ "type": "string", "format": "date-time" }, { "type": "integer" }] },
    "limit":      { "type": "integer", "minimum": 1, "maximum": 200, "default": 50 },
    "cursor":     { "type": "string", "description": "Opaque cursor from a prior response's next_cursor." }
  },
  "additionalProperties": false
}
```

**Output schema**
```json
{
  "type": "object",
  "required": ["events"],
  "properties": {
    "events":      { "type": "array", "items": { "$ref": "#/$defs/EventSummary" } },
    "next_cursor": { "type": ["string", "null"], "description": "Present when the result set was truncated at limit." }
  }
}
```

**Example**
```json
// → list_webhook_events({ "provider": "github", "limit": 2 })
{
  "events": [
    { "id": 4213, "provider": "github", "event_type": "push", "trust_mode": "signed",
      "verified": true, "payload_size": 8421, "received_at": "2026-07-05T18:03:21.442Z" },
    { "id": 4207, "provider": "github", "event_type": "pull_request", "trust_mode": "signed",
      "verified": true, "payload_size": 15903, "received_at": "2026-07-05T17:41:09.010Z" }
  ],
  "next_cursor": "eyJ0IjoiMjAyNi0wNy0wNVQxNzo0MTowOS4wMTBaIiwiaWQiOjQyMDd9"
}
```

---

### `get_webhook_event`

Full detail for a single event, including sanitized headers, the raw payload, and the verification
result.

**Input schema**
```json
{
  "type": "object",
  "required": ["id"],
  "properties": { "id": { "type": "integer" } },
  "additionalProperties": false
}
```

**Output schema:** `EventDetail`. Raises `not_found` if no event has that id.

**Example**
```json
// → get_webhook_event({ "id": 4214 })
{
  "id": 4214, "provider": "generic:dockerhub", "event_type": null,
  "trust_mode": "unverified", "verified": false, "verify_detail": "unverified by design",
  "external_id": null, "content_type": "application/json", "source_ip": "10.0.4.12",
  "payload_size": 512, "received_at": "2026-07-05T18:04:02.101Z",
  "headers": { "content-type": "application/json", "x-hub-signature-256": "«redacted»" },
  "payload": "{\"push_data\":{\"tag\":\"latest\"},\"repository\":{\"repo_name\":\"joestump/switchboard\"}}"
}
```

---

### `replay_webhook_event`

**The only tool with side effects.** Re-POSTs the stored raw payload (and a replay-safe subset of
headers) to a target URL — for exercising a downstream consumer under local development.

- `target_url` is optional; if omitted it falls back to the configured `replay_default_target`
  (Settings). If **neither** is set, the tool returns an `invalid_argument` error rather than guessing.
- It does **not** replay back to the originating provider (providers are senders, not receivers).
- The target URL scheme MUST be `http`/`https`. The implementation SHOULD constrain targets
  (localhost/trusted default or an allowlist) to avoid becoming an SSRF primitive
  ([ADR-005](../adr/ADR-005-mcp-tool-and-resource-contract.md)). Every replay is logged.

**Input schema**
```json
{
  "type": "object",
  "required": ["id"],
  "properties": {
    "id":         { "type": "integer" },
    "target_url": { "type": "string", "format": "uri",
                    "description": "Optional. Defaults to settings.replay_default_target. http/https only." }
  },
  "additionalProperties": false
}
```

**Output schema**
```json
{
  "type": "object",
  "required": ["id", "target_url", "delivered", "response_status", "response_ms"],
  "properties": {
    "id":              { "type": "integer" },
    "target_url":      { "type": "string", "format": "uri" },
    "delivered":       { "type": "boolean", "description": "true if the POST completed (any HTTP status)." },
    "response_status": { "type": ["integer", "null"], "description": "Downstream HTTP status; null on connection failure." },
    "response_ms":     { "type": "integer", "description": "Round-trip time in milliseconds." }
  }
}
```

**Example**
```json
// → replay_webhook_event({ "id": 4213, "target_url": "http://127.0.0.1:9000/hook" })
{ "id": 4213, "target_url": "http://127.0.0.1:9000/hook", "delivered": true,
  "response_status": 200, "response_ms": 14 }
```

---

### `list_providers`

Configured providers with type, enable state, and secret status. The secret value is never returned.

**Input schema**
```json
{ "type": "object", "properties": {}, "additionalProperties": false }
```

**Output schema**
```json
{
  "type": "object",
  "required": ["providers"],
  "properties": { "providers": { "type": "array", "items": { "$ref": "#/$defs/ProviderStatus" } } }
}
```

**Example**
```json
// → list_providers({})
{
  "providers": [
    { "name": "github", "kind": "signed", "trust_mode": "signed", "enabled": true,
      "secret_status": "configured", "path": "/webhooks/github", "channel": null },
    { "name": "generic:dockerhub", "kind": "generic", "trust_mode": "unverified", "enabled": true,
      "secret_status": "none-by-design", "path": "/webhooks/generic/dockerhub", "channel": null },
    { "name": "redis:deploys", "kind": "redis", "trust_mode": "redis", "enabled": true,
      "secret_status": "none-by-design", "path": null, "channel": "deploys" }
  ]
}
```

## Resources

Per the brief (§7) and [ADR-005](../adr/ADR-005-mcp-tool-and-resource-contract.md), recent events are
also exposed as a **read-only resource** for clients that model context as resources rather than tool
calls. All mutation/replay stays in tools.

| URI | Returns | Notes |
|-----|---------|-------|
| `switchboard://events/recent` | `{ "events": EventSummary[] }` | Newest-first, capped (e.g. 50). Same shape as `list_webhook_events` without filters. |

- **MIME type:** `application/json`.
- The MVP guarantees at least a **pull-able** recent-events resource. Whether the SDK's
  resource-subscription/update mechanism is wired for push (so subscribed clients are notified on new
  events, mirroring the UI's SSE) is left to the code session.

## Errors

Tools raise MCP tool errors with a stable machine `code` and a human `message` (never containing
secrets):

| `code` | When |
|--------|------|
| `not_found` | `get_webhook_event` / `replay_webhook_event` given an `id` that does not exist. |
| `invalid_argument` | Bad input — e.g. `replay` with no `target_url` and no configured default, or a non-`http(s)` target, or a malformed cursor. |
| `replay_failed` | Replay could not connect to / complete against the target (distinct from a downstream non-2xx, which is reported via `response_status`). |
| `internal` | Unexpected server-side failure (DB unavailable, etc.). |

## Cross-references

- Shape decisions & rationale: [ADR-005](../adr/ADR-005-mcp-tool-and-resource-contract.md).
- Trust semantics (`trust_mode`/`verified`/`verify_detail`): [ADR-003](../adr/ADR-003-per-provider-ingestion-and-trust-model.md).
- Storage schema behind these shapes: [ADR-002](../adr/ADR-002-postgres-persistence-and-retention.md).
- HTTP + SSE surfaces sharing these shapes: [`openapi.yaml`](openapi.yaml), [`asyncapi.yaml`](asyncapi.yaml).
- MCP Python SDK: <https://github.com/modelcontextprotocol/python-sdk> · MCP spec: <https://modelcontextprotocol.io/>.
```
