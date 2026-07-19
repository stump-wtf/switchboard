---
status: draft
date: 2026-07-18
implements: [ADR-0019]
extends: [SPEC-0007, SPEC-0014]
requires: [SPEC-0008]
---

# SPEC-0016: MCP OAuth Authorization for Vended Endpoints

## Overview

Switchboard acts as an OAuth 2.1 authorization server for its own vended MCP endpoints (ADR-0019):
protected-resource discovery, dynamic client registration, authorization-code + PKCE with a human
consent screen, and opaque access/refresh tokens that are credentials **onto existing vended
endpoints** — preserving SPEC-0007's doctrine (human principal, immutable scope, revoke = instant
and total). The static `sbk_` bearer path (SPEC-0014) coexists. This spec also introduces
credential lifetime as an endpoint property, used by both credential shapes and by the vend wizard
(SPEC-0015).

## Requirements

### Requirement: Protected Resource Metadata

Each MCP mount (`/mcp/{slug}`) SHALL serve RFC 9728 protected-resource metadata identifying its
authorization server, and unauthenticated or invalid-token requests SHALL receive a 401 bearing a
`WWW-Authenticate: Bearer resource_metadata="…"` challenge instead of today's bare 401.

#### Scenario: Client discovers how to authorize

- **WHEN** an MCP client hits `/mcp/{slug}` without credentials
- **THEN** the 401 challenge points it to metadata from which it can discover the authorization
  server and begin the flow

### Requirement: Authorization Server Metadata

The server SHALL publish RFC 8414 authorization-server metadata (issuer, authorization endpoint,
token endpoint, registration endpoint, PKCE methods `S256`, grant types code + refresh) consistent
with the deployed base URL.

#### Scenario: Metadata round-trip

- **WHEN** a client fetches the advertised AS metadata
- **THEN** every endpoint URL in it resolves on this deployment and the flow can complete using
  only discovered URLs

### Requirement: Dynamic Client Registration

The server SHALL accept RFC 7591 dynamic client registration, persisting client_id, client name,
and redirect URIs. Redirect URIs SHALL be validated exactly (no wildcards, no open redirects) at
registration and again at authorization time.

#### Scenario: Claude Desktop self-registers

- **WHEN** a new MCP client POSTs a registration with its redirect URI
- **THEN** it receives a client_id usable in the authorization flow, and any authorize request with
  a non-matching redirect URI is rejected

### Requirement: Authorization Code Flow With Consent

`GET /oauth/authorize` SHALL require an authenticated human session (SPEC-0008 login), bind the
request to one vended endpoint, and render a consent screen in the charm-web language showing: the
client's name, the workspace it wants, the accountable principal, and scope bullets **derived from
the endpoint's `scope_queues` and `scope_verbs`** (e.g. "read todos on github · ci", "claim &
complete under a lease"). Approval SHALL issue a single-use, expiring, PKCE-bound (S256)
authorization code; denial SHALL return the standard error to the client. Codes SHALL be stored
hashed and marked used on redemption; replayed codes SHALL be rejected and SHALL revoke tokens
issued from that code.

#### Scenario: Consent reflects real enforcement

- **WHEN** the consent screen renders for an endpoint scoped to queues `github, ci` and verbs
  `read, claim, complete`
- **THEN** every bullet corresponds to that stored scope — nothing advertised that the scope guard
  would not actually allow

#### Scenario: Human absent

- **WHEN** an authorize request arrives with no valid human session
- **THEN** the operator is taken through login (SPEC-0008) first; the flow resumes only for an
  authenticated accountable principal

### Requirement: Token Issuance And Refresh

`POST /oauth/token` SHALL exchange a valid code + PKCE verifier for an opaque access token and
rotating refresh token, both high-entropy and stored **hashed**, each bound to exactly one endpoint
(`endpoint_id`). Access tokens SHALL carry an expiry no later than the endpoint's own expiry;
refresh SHALL rotate (old refresh invalidated) and SHALL fail once the endpoint is revoked or
expired. Raw token values SHALL never be logged or stored.

#### Scenario: Refresh after endpoint death

- **WHEN** a client presents a refresh token for a revoked endpoint
- **THEN** the grant fails and no new token is issued

### Requirement: Resource-Server Token Validation

The MCP auth path SHALL accept OAuth access tokens and `sbk_` static bearers interchangeably: both
resolve to an endpoint ID, after which scope enforcement, sessions, doorbells, and tooling are
identical and unchanged. Expired or revoked tokens SHALL yield the RFC-compliant challenge.

#### Scenario: Two credential shapes, one capability

- **WHEN** the same endpoint is accessed once via its static bearer and once via an OAuth token
- **THEN** both sessions see the identical tool surface and scope limits

### Requirement: Revocation Cascade

Revoking an endpoint SHALL atomically delete/invalidate its OAuth tokens and codes, close its live
MCP sessions, and (per SPEC-0007) permanently delete the endpoint — one act kills every way of
exercising the capability. Consent history SHALL not resurrect access.

#### Scenario: Revoke is total

- **WHEN** the operator revokes an endpoint with an active OAuth-connected client
- **THEN** the client's next request fails with the 401 challenge and refresh cannot recover access

### Requirement: Credential Lifetime

Endpoints SHALL gain an optional expiry (`expires_at`) chosen at vend time (SPEC-0015 wizard).
The reaper SHALL treat an expired endpoint as revoked (sessions closed, tokens dead, bearer
rejected), and the endpoints view SHALL show the countdown. Endpoints without expiry remain valid
until revoked.

#### Scenario: Expiry enforcement

- **WHEN** an endpoint's expiry passes while an agent holds a live session
- **THEN** the session is closed and subsequent bearer or OAuth access fails identically to
  revocation
