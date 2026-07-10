---
status: draft
date: 2026-07-10
implements: [ADR-0017]
requires: [SPEC-0006, SPEC-0007]
extends: [SPEC-0006, SPEC-0007]
---

# SPEC-0014: MCP Streamable HTTP Transport

## Overview

This capability serves every vended MCP endpoint **exclusively over Streamable HTTP/S** from the
central switchboard service, realizing [ADR-0017](../../../adrs/ADR-0017-mcp-streamable-http-only.md).
An agent's entire client footprint is the vended URL plus bearer credential
([SPEC-0007](../vended-endpoints/spec.md)); no local binary, subprocess, or adapter is required or
supported. It refines SPEC-0007's `.mcp.json` wiring output and carries the agent tool surface of
[SPEC-0006](../agent-tools/spec.md) over MCP, and it retires the local stdio adapter
(`switchboard channel`) and the bespoke `/agent/*` REST proxy surface.

## Requirements

### Requirement: Streamable HTTP MCP Endpoint

The service MUST mount an MCP server implementing the **Streamable HTTP** transport at
`/mcp/{endpoint}`, where `{endpoint}` is the unique slug minted at vend time. The implementation
MUST use the official Go MCP SDK (`github.com/modelcontextprotocol/go-sdk`), vendored per the
hermetic build. The endpoint MUST support `initialize`, `tools/list`, `tools/call`, and `ping`, MUST
negotiate protocol versions via the SDK (not a hard-coded version string), and MUST support the
transport's server-to-client notification stream. Stdio MUST NOT be offered as an agent transport:
the `switchboard channel` subcommand and `internal/channel` stdio adapter MUST be removed.

#### Scenario: Standard HTTP MCP client connects

- **WHEN** an MCP client sends `initialize` to `https://<host>/mcp/{endpoint}` with a valid bearer
  credential
- **THEN** the server MUST complete the handshake via the SDK, advertise its tool capability, and
  serve subsequent `tools/list` and `tools/call` requests on the same session

#### Scenario: stdio path is gone

- **WHEN** `switchboard channel` is invoked
- **THEN** the CLI MUST fail with an unknown-command error (the subcommand no longer exists)

### Requirement: Bearer Authentication Bound to the Vended Endpoint

Every request under `/mcp/{endpoint}` MUST present `Authorization: Bearer <credential>`. The server
MUST resolve the credential by hash to an active vended endpoint and MUST verify it matches the
`{endpoint}` slug in the path; a missing or unknown credential MUST yield `401`, and a valid
credential presented against a different endpoint's path MUST yield `403`. Requests to a revoked
endpoint MUST yield `401` and MUST NOT reveal whether the slug ever existed. The URL alone MUST NOT
grant access. Successful authentication MUST update the endpoint's last-seen timestamp.

#### Scenario: Revocation kills the endpoint

- **WHEN** the owning human revokes an endpoint and its agent then calls `tools/list`
- **THEN** the server MUST respond `401` and MUST NOT process the call

#### Scenario: Credential/path mismatch is rejected

- **WHEN** agent A's valid credential is presented at agent B's `/mcp/{endpoint}` path
- **THEN** the server MUST respond `403` without executing any tool

### Requirement: Agent Tool Surface over MCP

The tools served MUST be the SPEC-0006 agent verbs — `list_todos`, `claim`, `complete`, `fail`, and
`heartbeat` — with the schemas, structured outputs, and error mapping SPEC-0006 defines. `tools/list`
MUST advertise only the verbs in the endpoint's allowlist, and `tools/call` MUST enforce both the
verb allowlist and the queue scope at the boundary before touching the store; out-of-scope calls
MUST return a scope error without side effects. Todo lifecycle semantics MUST defer to
[SPEC-0003](../todo-queue/spec.md) (the transport adds no lifecycle behavior).

#### Scenario: Scope filters the advertised tools

- **WHEN** an endpoint vended with verbs `list_todos, claim` requests `tools/list`
- **THEN** exactly those two tools MUST be advertised, and a `tools/call` of `complete` MUST return a
  scope error without mutating any todo

### Requirement: Channels Push over the HTTP Stream

Sessions MUST advertise `capabilities.experimental["claude/channel"]` and MUST deliver todo-ready
doorbells as `notifications/claude/channel` on the Streamable HTTP notification stream, preserving
the [SPEC-0011](../channels/spec.md) push shape (one-line summary content; snake_case `meta`
identifiers) and its lossy, best-effort semantics: a notification MUST only ever be a hint, the
durable queue remains the ledger, and an agent with no open stream MUST lose nothing except latency.
Only verified, human-attributed todos MUST be pushed, and payload content MUST be guarded against
channel-tag injection per SPEC-0011.

#### Scenario: Doorbell arrives over HTTP

- **WHEN** a todo becomes ready in a queue within the endpoint's scope while the agent holds an open
  stream
- **THEN** the server MUST emit `notifications/claude/channel` with the summary and routing `meta` on
  that stream

#### Scenario: No stream, no loss

- **WHEN** a todo becomes ready while the agent has no open notification stream
- **THEN** no notification is delivered, and a later `list_todos` MUST return the todo unchanged

### Requirement: HTTP Wiring Is the Only Wiring

The vended-credential reveal (SPEC-0007 / SPEC-0013) and all documentation MUST emit only HTTP
wiring: `{"mcpServers":{"switchboard":{"type":"http","url":"https://<host>/mcp/<slug>","headers":{"Authorization":"Bearer <credential>"}}}}`.
Instructions to install the binary, add it to PATH, or configure a stdio command MUST be removed
from the vended screen, README, and reference docs. The `/agent/*` REST routes and `/agent/stream`
SSE MUST be removed in the same release that ships this transport.

#### Scenario: Vend reveal shows HTTP wiring

- **WHEN** a human vends an endpoint
- **THEN** the one-time reveal MUST show the `type: "http"` `.mcp.json` block with the minted URL and
  credential, and MUST NOT reference a local command

### Requirement: Error Handling Standards

All error-producing operations MUST follow structured error handling:

- Errors MUST be wrapped with contextual information at each layer boundary
- Sentinel errors MUST be defined for failure modes callers distinguish programmatically
  (unauthorized, scope violation, not-found, lease conflict)
- Silent error swallowing MUST NOT occur — every error MUST be returned to the caller as a proper
  JSON-RPC/tool error, logged with context, or explicitly suppressed with a documented reason
- Structured logging MUST be used (key-value pairs) and MUST include the endpoint slug and tool name
  without ever logging the bearer credential

#### Scenario: Store failure surfaces as a tool error

- **WHEN** a `tools/call` hits a database failure
- **THEN** the client MUST receive a generic tool error while the server log records endpoint, tool,
  and the wrapped error chain

### Requirement: Concurrency Safety

All concurrent operations MUST follow safe concurrency patterns:

- Context propagation MUST carry cancellation from the HTTP request/stream into every store call
- Session and stream lifecycle MUST be explicitly managed — streams MUST shut down cleanly on client
  disconnect, server shutdown, and revocation, without leaking goroutines
- Shared state (session registry, notification fan-out) MUST be race-safe
- Concurrent tests MUST run with race detection enabled in CI

#### Scenario: Revocation closes live streams

- **WHEN** an endpoint is revoked while its agent holds an open notification stream
- **THEN** the server MUST close that stream promptly and subsequent requests MUST receive `401`

## Endpoints

| Method | Path | Auth | Description |
|--------|------|------|-------------|
| POST | `/mcp/{endpoint}` | Required (Bearer) | Streamable HTTP MCP requests (initialize, tools/*) |
| GET | `/mcp/{endpoint}` | Required (Bearer) | Server notification stream (Streamable HTTP) |
| DELETE | `/mcp/{endpoint}` | Required (Bearer) | Session teardown per Streamable HTTP |

No public endpoints are introduced. TLS terminates at the deployment reverse proxy (plain Caddy
`reverse_proxy`); the service MUST assume credentials appear only on loopback/proxied traffic and
MUST never emit the credential in logs, errors, or notifications.

## Security Requirements

### Authentication

All `/mcp/*` routes MUST require bearer authentication resolving to an active vended endpoint as
specified above; there are no anonymous MCP operations. Web-UI session cookies MUST carry no
authority on `/mcp/*`.

### Rate Limiting

`/mcp/*` MUST be rate-limited per endpoint (token bucket comparable to the current agent-surface
limit) covering both request POSTs and stream (re)establishment, so a leaked credential cannot be
used for volumetric abuse before revocation.

### Security Headers

MCP responses are JSON/SSE, not HTML: responses MUST set `X-Content-Type-Options: nosniff` and
`Cache-Control: no-store`; HTML-oriented headers (CSP, X-Frame-Options) are inherited from the
global middleware and MUST NOT be weakened for this surface.

### Request Body Size Limits

MCP request bodies MUST be bounded with a 1 MiB limit before JSON-RPC parsing; oversized requests
MUST receive `413`.

### CSRF Protection

Not applicable by construction: `/mcp/*` accepts only bearer-authenticated requests and ignores
cookies, so cross-site request forgery has no ambient credential to ride. This exemption MUST be
revisited if any cookie-based auth is ever accepted on this surface.

### Redirect Validation

The MCP surface MUST NOT issue redirects.
