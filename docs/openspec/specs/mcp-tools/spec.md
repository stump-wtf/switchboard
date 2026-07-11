---
status: approved
date: 2026-07-06
implements: [ADR-0005]
---

# SPEC-0005: MCP Tool and Resource Contract

## Overview

Switchboard exposes its stored webhook/queue event log to MCP clients (Claude Code and other
agents) as a set of tools plus a read-only resource, served by the official Go MCP SDK over
streamable HTTP from the same process that serves the human web UI and the vended agent API
([ADR-0005](../../../adrs/ADR-0005-mcp-tool-and-resource-contract.md)). This capability pins the
*shape* of that shared contract: tool naming, input/output JSON schemas, pagination and filtering
semantics, the one side-effecting verb (`replay_webhook_event`), the tools-vs-resource split, and a
stable machine-readable error shape.

This is the **shared, generic** event-history contract. It is deliberately read-oriented: an agent
scans the event log, drills into individual events, replays a stored payload to a local consumer,
and enumerates configured providers. The agent-facing *work* surface — claiming/completing todos and
self-managing ingestion sources — is a separate capability
([SPEC-0006](../agent-tools/spec.md)). Every event object returned here carries its trust metadata
(`trust_mode`, `verified`, `verify_detail`) so an agent can never mistake an unverified event for a
signed, verified one.

## Requirements

### Requirement: Tool Surface and Naming

The server MUST expose exactly four tools under the server name `switchboard`:
`list_webhook_events`, `get_webhook_event`, `replay_webhook_event`, and `list_providers`. Tool names
MUST be stable snake_case identifiers. Each tool MUST declare an input JSON Schema and MUST return
SDK **structured output** validated against a declared output schema; tools MUST NOT return free-form
text blobs in place of structured output. Of the four tools, only `replay_webhook_event` MAY produce
a side effect; the other three MUST be read-only.

#### Scenario: Tool discovery lists the contract set

- **WHEN** an MCP client lists the tools offered by the `switchboard` server
- **THEN** it receives `list_webhook_events`, `get_webhook_event`, `replay_webhook_event`, and
  `list_providers`, each with a declared input schema and structured output schema

#### Scenario: A read tool never mutates state

- **WHEN** a client calls `list_webhook_events`, `get_webhook_event`, or `list_providers`
- **THEN** no outbound request is made and no stored record is modified

### Requirement: Event Shape Parity and Trust Disclosure

The `EventSummary` shape returned by `list_webhook_events` and the recent-events resource MUST carry
`id`, `provider`, `event_type`, `trust_mode`, `verified`, `payload_size`, and `received_at`. The
`EventDetail` shape returned by `get_webhook_event` MUST additionally carry `verify_detail`,
`external_id`, `content_type`, `source_ip`, sanitized `headers`, and the raw `payload`. Every event
object over MCP MUST include `trust_mode` and `verified` (and, in detail, `verify_detail`) so that an
agent can distinguish a signed, verified event from a token/open/queue-trust event. These shapes MUST
match the normalized shape stored in the `events` table so the MCP, HTTP, and UI surfaces never
drift.

#### Scenario: Summary omits heavy fields

- **WHEN** a client calls `list_webhook_events`
- **THEN** each returned event carries the summary fields only and MUST NOT include the raw
  `payload` or `headers`

#### Scenario: Trust metadata always present

- **WHEN** any event object is returned over MCP by any tool or resource
- **THEN** it MUST include `trust_mode` and `verified`, and a token/open/queue-trust event MUST
  report `verified: false`

### Requirement: Deterministic Pagination and Filtering

`list_webhook_events` MUST order results by `received_at DESC, id DESC` (stable). It MUST accept the
optional filters `provider`, `event_type`, and `since` (an ISO-8601 timestamp OR an event id, bounding
the lower edge), and the pagination controls `limit` (default 50, minimum 1, maximum 200) and an
opaque `cursor`. When a result set is truncated at `limit`, the tool MUST return a `next_cursor`
encoding the last `(received_at, id)` seen. Cursor-based pagination MUST be used rather than numeric
offset so that concurrent inserts on an always-on receiver never produce duplicate or skipped rows
across pages. A malformed cursor MUST raise `invalid_argument`.

#### Scenario: Cursor round-trip has no duplicates or gaps

- **WHEN** a client pages through events using each response's `next_cursor` as the next request's
  `cursor`, while new events are being ingested concurrently
- **THEN** no event appears twice and no event within the requested bounds is skipped

#### Scenario: Limit is clamped

- **WHEN** a client requests `limit` greater than 200
- **THEN** the tool MUST reject it via schema validation (`invalid_argument`) rather than returning
  an unbounded response

### Requirement: Replay Safety

`replay_webhook_event` is the only side-effecting tool. It MUST re-POST the stored raw payload and a
replay-safe subset of headers to a target URL. `target_url` is OPTIONAL; when omitted the tool MUST
fall back to the configured `replay_default_target` setting, and when neither is present it MUST
return `invalid_argument` rather than guessing a target. The tool MUST NOT replay back to the
originating provider. The target URL scheme MUST be `http` or `https`; any other scheme MUST be
rejected with `invalid_argument`. The implementation SHOULD constrain targets to a
localhost/trusted-network default or an allowlist to avoid becoming an SSRF primitive. Every replay
attempt MUST be logged. The response MUST report `id`, resolved `target_url`, `delivered` (whether
the POST completed at all), `response_status` (downstream HTTP status, or null on connection
failure), and `response_ms`.

#### Scenario: No target and no default is an error, not a guess

- **WHEN** `replay_webhook_event` is called with neither `target_url` nor a configured
  `replay_default_target`
- **THEN** the tool MUST return `invalid_argument` and MUST NOT perform any outbound request

#### Scenario: Non-http scheme is rejected before any request

- **WHEN** `replay_webhook_event` is called with a `target_url` whose scheme is not `http`/`https`
  (e.g. `file://` or `gopher://`)
- **THEN** the tool MUST reject it with `invalid_argument` and MUST NOT perform any outbound request

#### Scenario: Downstream non-2xx is reported, not raised

- **WHEN** the target accepts the connection and responds with a non-2xx status
- **THEN** the tool MUST return `delivered: true` with the downstream `response_status`, rather than
  raising `replay_failed` (which is reserved for connection/transport failures)

### Requirement: Provider Enumeration Without Secrets

`list_providers` MUST return, for each configured provider, a `ProviderStatus` with `name`, `family`
(`webhook` or `queue`), `trust_mode`, `enabled`, `secret_status` (`configured`, `missing`, or
`none-by-design`), and the route `path` (HTTP providers) or `channel` (queue providers). It MUST NOT
return any signing secret value. `secret_status` MUST report only the presence/absence classification,
never the secret material itself.

#### Scenario: Secret status without the secret

- **WHEN** a client calls `list_providers` and a provider is a signed webhook with a configured secret
- **THEN** the provider reports `secret_status: "configured"` and the response contains no secret value

### Requirement: Read-Only Recent-Events Resource

The server MUST expose a read-only MCP resource at URI `switchboard://events/recent` with MIME type
`application/json`, returning `{ "events": EventSummary[] }` newest-first and capped (e.g. 50), using
the same `EventSummary` shape as `list_webhook_events` with no filters. The resource MUST be strictly
read-only; all mutation and replay MUST remain in tools. Push/subscription of the resource MAY be
wired by the implementation, but a pull-able recent-events resource is REQUIRED.

#### Scenario: Resource returns summaries, never mutates

- **WHEN** a client reads `switchboard://events/recent`
- **THEN** it receives newest-first `EventSummary` objects and no state is changed

### Requirement: Stable Error Shape

Tools MUST raise MCP tool errors with a stable machine `code` and a human `message`. The `message`
MUST NOT contain any secret material. The defined codes are: `not_found` (unknown event id),
`invalid_argument` (bad input — missing replay target with no default, non-http(s) target, malformed
cursor, out-of-range limit), `replay_failed` (replay could not connect to or complete against the
target), `rate_limited` (the caller's per-endpoint replay budget is exhausted — see Rate Limiting;
distinct from `invalid_argument` and `replay_failed` so a caller can back off deterministically), and
`internal` (unexpected server-side failure).

#### Scenario: Unknown id raises not_found

- **WHEN** `get_webhook_event` or `replay_webhook_event` is called with an `id` that does not exist
- **THEN** the tool MUST raise the `not_found` error code

## Security Requirements

### Authentication

The MCP surface is served over HTTP by the SDK's streamable-HTTP transport mounted in the same Go
HTTP server. All MCP tool and resource access MUST require authentication by default; unauthenticated
access MUST be rejected. The event log includes potentially sensitive payloads and provider metadata
and MUST NOT be world-readable.

| Endpoint | Auth | Justification |
|----------|------|---------------|
| MCP streamable-HTTP mount (tool calls, resource reads) | Required | Event log + provider metadata are sensitive; agents authenticate before any read. |
| `list_webhook_events` / `get_webhook_event` / `list_providers` / resource read | Required | Read tools still expose stored payloads and headers. |
| `replay_webhook_event` | Required | Side-effecting outbound POST; must be attributable. |
| `/healthz` (server process liveness) | Public | Liveness probe only; returns `ok`/`db down`, no event data. |

### Rate Limiting

`replay_webhook_event` MUST be rate-limited per authenticated caller because it performs outbound
requests and could be abused for amplification/SSRF probing. Read tools SHOULD be rate-limited to
bound database load from broad `list` scans. A token-bucket per caller identity is RECOMMENDED; exact
limits are a code-session concern but replay MUST be bounded more tightly than reads.

### Security Headers

All HTTP responses from the MCP mount MUST include:
- `Content-Security-Policy`: `default-src 'none'` (the MCP transport serves JSON, not browser assets)
- `X-Frame-Options`: DENY
- `X-Content-Type-Options`: nosniff
- `Referrer-Policy`: strict-origin-when-cross-origin

### Request Body Size Limits

All MCP HTTP requests accepting bodies MUST bound them with `http.MaxBytesReader`. Default limit:
**1 MiB** for tool-call requests. Requests exceeding the limit MUST be rejected before processing.

### CSRF Protection

The MCP transport is a machine-to-machine JSON API authenticated by a bearer credential, not a
cookie-authenticated browser session, so classic form CSRF does not apply. State-changing calls
(`replay_webhook_event`) MUST rely on bearer-credential authentication and MUST NOT be reachable via
ambient cookie authentication. No HTML forms are served on this surface.

### Redirect Validation

`replay_webhook_event` accepts a user-supplied outbound target. That target MUST be validated:
scheme restricted to `http`/`https`, and the implementation SHOULD enforce a localhost/trusted-network
default or an allowlist to prevent SSRF. The MCP surface itself issues no HTTP redirects to clients;
no open-redirect vector exists on the response side.

## Cross-References

- Shape decisions & rationale: [ADR-0005](../../../adrs/ADR-0005-mcp-tool-and-resource-contract.md).
- Agent-facing work + self-management surface: [SPEC-0006](../agent-tools/spec.md).
- Design & architecture for this capability: [design.md](design.md).
