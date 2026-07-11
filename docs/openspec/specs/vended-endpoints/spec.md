---
status: approved
date: 2026-07-06
implements: [ADR-0008]
requires: [SPEC-0008]
---

# SPEC-0007: Vended MCP Endpoints

## Overview

Switchboard's access model has exactly one accountable principal — a human — and treats every AI
agent as a disposable, untrusted subject that gets access only through a **vended endpoint**. A
vended endpoint is the pair **(endpoint URL + credential)**; possessing that pair *is* the capability
grant. Each endpoint is scoped to a specific set of queues and a specific verb allowlist, so an agent
can do exactly what its owning human granted and nothing more. Credentials are switchboard-minted,
shown to the human exactly once, and stored only as a SHA-256 hash in PostgreSQL — never in plaintext
at rest. Revoking an endpoint invalidates its credential and unroutes its URL instantly and totally,
with no residue in any identity provider because agents are never principals in the IdP.

This capability realizes [ADR-0008](../../../adrs/ADR-0008-human-principal-vended-endpoints.md) (human
principal + per-agent vended MCP endpoints). The human OIDC login that establishes the accountable
principal is a separate capability, [SPEC-0008](../identity/spec.md); credential storage-at-rest
posture follows [ADR-0002](../../../adrs/ADR-0002-postgres-persistence-and-retention.md).

## Requirements

### Requirement: Human as Accountable Principal

Every agent and every vended endpoint MUST be owned by exactly one human principal, established via
OIDC login ([SPEC-0008](../identity/spec.md)). Agents MUST NOT be principals in the identity provider.
Registering an agent MUST NOT, by itself, grant any access; access MUST come only from a vended
endpoint. All human-facing management operations (register agent, vend, revoke) MUST require an
authenticated human session and MUST be authorized against ownership — a human MUST NOT be able to
manage another human's agents or endpoints.

#### Scenario: Registration grants nothing

- **WHEN** a human registers an agent but has not yet vended an endpoint for it
- **THEN** the agent exists as an owned record with no credential, and no MCP call can be authenticated
  on its behalf

#### Scenario: Ownership guards management

- **WHEN** a human requests an agent or attempts to vend/revoke on an agent they do not own
- **THEN** switchboard MUST treat it as not found (no cross-tenant access) and MUST NOT perform the
  operation

### Requirement: Endpoint Vending

Vending an endpoint MUST mint a high-entropy credential, MUST persist only its SHA-256 hash plus a
non-secret display prefix, and MUST return the plaintext credential to the human exactly once. The
plaintext credential MUST NOT be retrievable after that single display. A vended endpoint MUST record
its owning agent, its scope (queues + verbs), its mutability, and a state of `active`.

#### Scenario: Vend returns the credential once

- **WHEN** a human vends an endpoint for one of their agents with a chosen queue and verb scope
- **THEN** switchboard mints a `sbk_`-prefixed token, stores only its SHA-256 hash and display prefix,
  marks the endpoint `active`, and shows the plaintext token to the human exactly once

#### Scenario: Plaintext is never recoverable

- **WHEN** the human reloads the endpoint view or lists endpoints after vending
- **THEN** only the non-secret credential prefix is shown; the full plaintext token is never returned
  again

### Requirement: Scoped Capability Enforcement

A vended endpoint's scope MUST consist of an allowed set of queues and a verb allowlist. Every
credential-authenticated call MUST be authorized at the boundary: a verb outside `scope.verbs` MUST be
rejected as `forbidden`, and an operation targeting a queue outside `scope.queues` MUST be rejected as
`forbidden`. An agent MUST NOT be able to widen its own scope.

#### Scenario: Verb outside allowlist is denied

- **WHEN** a credential is presented for a verb (e.g. `complete`) that is not in the endpoint's
  `scope.verbs`
- **THEN** switchboard MUST reject the call with a `forbidden` error and MUST NOT perform the action

#### Scenario: Queue outside grant is denied

- **WHEN** a call targets a todo whose queue is not in the endpoint's `scope.queues`
- **THEN** switchboard MUST reject the call with a `forbidden` error

#### Scenario: Scope is enforced from the presented credential

- **WHEN** any agent call arrives with a valid credential
- **THEN** the effective scope MUST be resolved from the stored endpoint record, not from any value
  the agent supplies in the request

### Requirement: Credential Hashing at Rest

Credentials MUST be stored as a SHA-256 hash and MUST NOT be stored in plaintext anywhere. Credential
resolution at the boundary MUST hash the presented bearer token and look it up by hash. Because the
tokens are high-entropy (32 random bytes), SHA-256 with constant-time lookup is REQUIRED and a slow
password hash (bcrypt/argon2) is NOT REQUIRED.

#### Scenario: Presented token resolves by hash

- **WHEN** an agent presents its bearer credential
- **THEN** switchboard hashes the presented token with SHA-256 and resolves it against
  `endpoints.credential_hash`, admitting the call only if an `active` endpoint matches

#### Scenario: No plaintext at rest

- **WHEN** the endpoints table is inspected
- **THEN** it MUST contain only the credential hash and a non-secret display prefix, never the
  plaintext token

### Requirement: Immutable Scope, Revoke-and-Re-vend

A vended endpoint's scope SHOULD be immutable by default (`mutability = immutable`): to change what an
agent may do, the human SHOULD revoke the endpoint and vend a new one with the new scope, rather than
editing a live grant in place. This keeps a given URL+credential pair denoting one fixed power set for
its lifetime. (Whether to also support mutable-in-place scope is an open question recorded in
design.md.)

#### Scenario: Scope change is a re-vend

- **WHEN** a human needs an agent to act on a different queue than an existing endpoint permits
- **THEN** the human revokes the existing endpoint and vends a new one with the new scope, receiving a
  new URL+credential pair

### Requirement: Instant, Total Revocation

Revoking an endpoint MUST invalidate its stored credential and unroute its URL immediately, and MUST
be authorized against ownership (only via the endpoint's owning human). After revocation, every call
presenting the revoked credential MUST fail authentication. Revocation MUST leave no residue: no
lingering identity in the IdP (agents were never there) and no path that still accepts the credential.

#### Scenario: Revoked credential stops working

- **WHEN** a human revokes an active endpoint and the agent later presents the same credential
- **THEN** the credential MUST NOT resolve to any `active` endpoint and the call MUST fail with
  `unauthenticated`

#### Scenario: Revoke is idempotent and ownership-guarded

- **WHEN** a human attempts to revoke an endpoint that is already revoked, or one they do not own
- **THEN** switchboard MUST treat it as not found and MUST NOT change state

### Requirement: Database Operation Standards

All endpoint and agent persistence MUST use parameterized queries only (no string interpolation into
SQL). Ownership-scoped mutations (vend, revoke) MUST enforce the ownership predicate in the same
statement that performs the write, so a mutation cannot succeed against a row the human does not own.
Connection use MUST propagate the request context for cancellation and timeout.

#### Scenario: Revocation binds ownership in the write

- **WHEN** a revoke executes
- **THEN** the UPDATE MUST constrain to endpoints whose agent's `owner_human_id` matches the caller,
  and MUST report not-found when zero rows are affected

## Security Requirements

### Authentication

All endpoints MUST require authentication by default. The vended agent surface authenticates via a
bearer credential resolved to an `active` endpoint and its immutable scope; the human management routes
authenticate via an OIDC-established session ([SPEC-0008](../identity/spec.md)) and are additionally
authorized against ownership.

> **Transport superseded (ADR-0017; [SPEC-0014](../mcp-transport/spec.md)).** The bearer-authenticated
> agent surface is now served as MCP over Streamable HTTP at `/mcp/{endpoint}`; the `/agent/*` REST
> routes and `/agent/stream` SSE listed below are **retired** and no longer mounted. The bearer-auth
> model (credential → active endpoint → immutable scope) is unchanged and enforced on `/mcp/{endpoint}`.

| Endpoint (retired REST shape) | Auth | Justification |
|----------|------|---------------|
| `GET /agent/whoami` | Required (bearer credential) | — |
| `GET /agent/todos` | Required (bearer credential; verb `list_todos`) | — |
| `POST /agent/todos/{id}/claim` | Required (bearer credential; verb `claim`) | — |
| `POST /agent/todos/{id}/complete` | Required (bearer credential; verb `complete`) | — |
| `POST /agent/todos/{id}/fail` | Required (bearer credential; verb `fail`) | — |
| `GET /agent/stream` | Required (bearer credential; SSE scoped to endpoint queues) | — |
| `POST /agents/{id}/vend` (human UI) | Required (session + ownership) | — |
| `POST /endpoints/{id}/revoke` (human UI) | Required (session + ownership) | — |

There are no public endpoints in this capability. A missing or malformed `Authorization: Bearer`
header MUST yield `401 unauthenticated`; a credential that does not resolve to an `active` endpoint
MUST yield `401 unauthenticated` (invalid or revoked). Scope failures after successful authentication
MUST yield `403 forbidden`.

### Rate Limiting

No per-endpoint rate limiting is enforced in the MVP; agent credentials are per-agent and
individually revocable, so an abusive endpoint is contained by revocation. Deployments SHOULD place a
reverse-proxy rate limit in front of `/agent`. A native per-credential limiter is a RECOMMENDED future
hardening and is deferred.

### Security Headers

The `/agent` surface returns JSON and SSE to non-browser clients, but all HTTP responses served by
switchboard MUST still include:
- `Content-Security-Policy`: `default-src 'none'; frame-ancestors 'none'` (the agent API serves no
  browser-executable content)
- `X-Frame-Options`: DENY
- `X-Content-Type-Options`: nosniff
- `Referrer-Policy`: strict-origin-when-cross-origin

### Request Body Size Limits

All endpoints accepting request bodies (`claim`, `complete`, `fail`) MUST bound the body with
`http.MaxBytesReader`. Default limit: 1 MiB. Oversized bodies MUST be rejected rather than buffered.

### CSRF Protection

The credential-authenticated `/agent` endpoints are not cookie-authenticated, so they are not subject
to browser CSRF; the bearer credential is the sole authenticator. The human-facing state-changing
routes (`vend`, `revoke`) ARE cookie-session authenticated and MUST implement CSRF protection
(SameSite=Lax on the session cookie plus a per-session CSRF token on the POST forms). CSRF specifics
for the human UI are owned by [SPEC-0008](../identity/spec.md).

### Redirect Validation

No user-supplied redirects exist in this capability. The vended endpoint URL is switchboard-generated,
not client-supplied, so open-redirect risk does not apply.
