---
status: accepted
date: 2026-07-18
decision-makers: Joe Stump
extends: [ADR-0008, ADR-0017]
related: [ADR-0011]
governs: [SPEC-0016]
---

# ADR-0019: Switchboard Becomes an OAuth Authorization Server for Vended MCP Endpoints

## Context and Problem Statement

Vended MCP endpoints (ADR-0008, ADR-0017) authenticate with a static bearer credential minted at
vend time and shown once. That works for headless agents an operator configures by hand, but MCP
clients (Claude Desktop, Claude Code, other hosts) speak the **MCP authorization spec**: they
discover a protected resource's authorization server, register dynamically, and run an OAuth 2.1
authorization-code + PKCE flow ending in a human consent screen. The redesign makes that consent
screen ("Claude Desktop wants to connect to your switchboard workspace over MCP") a first-class
surface. Today switchboard has **zero** authorization-server code — its only OAuth is as an OIDC
*relying party* to Pocket ID for human login (ADR-0011). How do MCP clients get authorized without
breaking the vended-endpoint doctrine (human accountable principal, immutable scope, revoke =
instant and total)?

## Decision Drivers

* **MCP-spec compliance**: Streamable-HTTP MCP servers advertise OAuth via RFC 9728
  protected-resource metadata + `WWW-Authenticate`; clients expect RFC 8414 AS metadata, RFC 7591
  dynamic client registration, and auth-code + PKCE. Today `internal/mcp` returns a bare 401.
* **The vended endpoint IS the grant**: ADR-0008's model — a human vends a scoped capability and
  can kill it — must not be diluted by a parallel OAuth grant universe.
* **Resource server = authorization server**: one Go binary serves both; opaque tokens looked up in
  Postgres suffice — no JWT/JWKS machinery, no cross-service validation.
* **Existing primitives**: `internal/cred` already mints/hashes high-entropy opaque secrets;
  `internal/auth` already guards human sessions (the consent screen sits behind it);
  `scope_queues`/`scope_verbs` on `endpoints` already express exactly what consent must display.
* Nobody uses the current bearer path in production; coexistence is cheap and re-vending is free.

## Considered Options

* **(A) Bearer-only status quo** — MCP clients configure a static header by hand.
* **(B) Full standalone OAuth AS** — its own grant/consent model, tokens independent of endpoints.
* **(C) OAuth AS layered on vended endpoints** — switchboard implements discovery, DCR, auth-code +
  PKCE, and token issuance, but every access token is a **credential onto an existing vended
  endpoint** (`oauth_tokens.endpoint_id → endpoints.id`); consent scope bullets derive from the
  endpoint's `scope_queues`/`scope_verbs`; endpoint revocation cascades to all its tokens.

## Decision Outcome

Chosen option: **"(C) OAuth layered on vended endpoints."** The OAuth flow is a second way to hold a
credential to the same scoped capability, not a second capability model:

* **New sibling package** (`internal/oauthsrv`): RFC 9728 protected-resource metadata per MCP mount,
  RFC 8414 AS metadata, RFC 7591 dynamic client registration, `GET /oauth/authorize` (behind the
  existing human session; renders the consent screen in the charm-web language),
  `POST /oauth/token` (auth-code + PKCE S256, refresh grant). `internal/auth` (human OIDC RP) is
  not overloaded.
* **Storage**: new tables `oauth_clients`, `oauth_codes`, `oauth_tokens` (hashes only, via
  `internal/cred` primitives), plus `endpoints.expires_at` — credential lifetime becomes a
  first-class endpoint property (the vend wizard's lifetime step) enforced by the reaper for both
  bearer and OAuth credentials.
* **Resource server**: `internal/mcp` auth accepts OAuth access tokens alongside the `sbk_` bearer
  (both resolve to an endpoint ID; scope guard, sessions, doorbells unchanged) and emits
  `WWW-Authenticate: Bearer resource_metadata="…"` on 401.
* **Doctrine preserved**: scope stays immutable (re-vend to change); revoking the endpoint deletes
  its tokens and closes its sessions in the same stroke (`CloseEndpointSessions`); refresh tokens
  rotate but never outlive the endpoint; the accountable principal is the human who vended and who
  consents.

### Consequences

* Good: `.mcp.json` for OAuth-capable clients needs only the URL — no pasted secret; consent shows
  humans exactly what the agent may do, derived from real enforcement data; ADR-0008 semantics
  survive intact.
* Bad: a genuinely new security-critical subsystem (~1k LOC + tests) with conformance surface
  (PKCE failure modes, redirect-URI validation, code replay); more tables; the vend flow gains a
  bearer-vs-OAuth choice to explain.
* Neutral: ADR-0011 stays humans-only OIDC RP; this ADR covers agents-inbound only. ADR-0017's
  transport is untouched.
