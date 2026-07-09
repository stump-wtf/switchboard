---
status: approved
date: 2026-07-06
implements: [ADR-0011]
---

# SPEC-0008: Human Identity & Assurance

## Overview

Switchboard has exactly one class of principal: the **human**. Humans authenticate via OIDC against
**Pocket ID**, a passkey-only identity provider that holds humans only — no agents or bots are ever
principals in the IdP. A successful login upserts the human (keyed on the OIDC subject) and mints a
server-side, revocable session carried by an opaque, hashed cookie token. This human identity is what
makes every agent action attributable: an agent's vended endpoint ([SPEC-0007](../vended-endpoints/spec.md))
is owned by an agent, which is owned by a human established through this capability.

The vended-endpoint credential auth in [SPEC-0007](../vended-endpoints/spec.md) is a **separate
machine-auth path** — a bearer credential resolved to a scoped endpoint — and is deliberately not the
same as human OIDC login. This capability governs only human login and session establishment.

For the MVP, switchboard **trusts the issuer** and does **not** enforce an `amr`/`acr` assurance claim
(no passkey step-up), because Pocket ID is passkey-only and therefore every human login is
passkey-backed by construction. Enforcing step-up before federating to any non-passkey IdP is a
recorded, triggered deferred-hardening requirement, not a silent gap.

This capability realizes [ADR-0011](../../../adrs/ADR-0011-identity-assurance-oidc-passkey-deferred.md)
(simple OIDC now, passkey step-up deferred) and supports the human-principal model of
[ADR-0008](../../../adrs/ADR-0008-human-principal-vended-endpoints.md).

## Requirements

### Requirement: OIDC Relying-Party Login

Human login MUST use the OIDC authorization-code flow against Pocket ID, with PKCE and a nonce. The
login initiation MUST generate per-login state, nonce, and PKCE verifier and stash them across the
redirect in a short-lived cookie. Switchboard MUST act as a relying party only; it MUST NOT hold or
verify passwords.

#### Scenario: Login redirects to the IdP

- **WHEN** a human visits the login route and OIDC is configured
- **THEN** switchboard sets a short-lived state cookie (state + nonce + PKCE verifier) and redirects
  the browser to Pocket ID's authorization endpoint with a PKCE S256 challenge

#### Scenario: OIDC not configured

- **WHEN** a human attempts to log in but no OIDC issuer is configured
- **THEN** switchboard MUST return a clear "OIDC not configured" error and MUST NOT establish a session

### Requirement: Callback Verification

The OIDC callback MUST verify the returned `state` against the stashed value, exchange the code using
the PKCE verifier, verify the ID token signature against the issuer, and confirm the ID token `nonce`
matches the stashed nonce. Any mismatch or verification failure MUST abort login and MUST NOT
establish a session. On success, switchboard MUST clear the state cookie.

#### Scenario: State or nonce mismatch aborts login

- **WHEN** the callback's `state` does not match the stashed state, or the verified ID token's `nonce`
  does not match the stashed nonce
- **THEN** switchboard MUST reject the callback with a client error and MUST NOT mint a session

#### Scenario: Valid callback establishes a session

- **WHEN** the callback presents a valid code, the exchanged ID token verifies against the issuer, and
  state + nonce match
- **THEN** switchboard upserts the human from the token's `sub`/`name`/`email` and establishes a
  session

### Requirement: Human Upsert Keyed on OIDC Subject

The human record MUST be keyed on the OIDC subject (`sub`). Login MUST upsert: insert on first login,
and on return update mutable profile fields (display name, email) without changing identity. The IdP
MUST hold humans only; no agent identity is ever created in the IdP.

#### Scenario: Returning human is not duplicated

- **WHEN** a previously seen human logs in again with the same OIDC subject
- **THEN** switchboard updates the existing human's profile fields in place and does not create a
  second human record

### Requirement: Server-Side Session Establishment

A session MUST be server-side and revocable: switchboard mints an opaque high-entropy token, stores
only its SHA-256 hash with an expiry, and sets the plaintext token in an `HttpOnly` cookie. The cookie
MUST be `HttpOnly`, `SameSite=Lax`, and MUST be `Secure` whenever the base URL is HTTPS. Sessions MUST
expire after a bounded lifetime. Session lookup MUST admit only live (unexpired) sessions.

#### Scenario: Session cookie carries only an opaque token

- **WHEN** a session is established
- **THEN** the cookie holds an opaque token, the database stores only its SHA-256 hash and an expiry,
  and the cookie is `HttpOnly`, `SameSite=Lax`, and `Secure` under HTTPS

#### Scenario: Expired session is not honored

- **WHEN** a request presents a session cookie whose stored session has passed its expiry
- **THEN** switchboard MUST treat the request as unauthenticated

### Requirement: Session-Gated Human Surface

Human-only routes MUST admit only authenticated humans; unauthenticated requests to those routes MUST
be redirected to login. The authenticated human MUST be resolved from the session and carried in
request context for downstream authorization (including ownership checks in
[SPEC-0007](../vended-endpoints/spec.md)). Logout MUST revoke the server-side session and clear the
cookie.

#### Scenario: Unauthenticated access is redirected

- **WHEN** an unauthenticated request hits a human-only route (e.g. the dashboard or agent management)
- **THEN** switchboard redirects to the login page and does not serve the protected content

#### Scenario: Logout revokes the session

- **WHEN** a human logs out
- **THEN** switchboard deletes the server-side session record and clears the session cookie, so the
  prior cookie no longer authenticates

### Requirement: Assurance Posture — Trust the Issuer, Step-Up Deferred

Switchboard MUST trust the Pocket ID issuer and MUST NOT enforce an `amr`/`acr` assurance claim on
login or consent actions in the MVP, because Pocket ID is passkey-only. Before switchboard federates
to, or accepts provenance from, **any IdP that is not passkey-only**, it MUST require and verify a
phishing-resistant `amr`/`acr` claim on high-consequence consent actions. The IdP-trust-set
configuration point MUST carry a guard/comment referencing
[ADR-0011](../../../adrs/ADR-0011-identity-assurance-oidc-passkey-deferred.md) so this requirement is
confronted when a non-passkey issuer is added.

#### Scenario: No step-up enforced today

- **WHEN** a human logs in with a valid Pocket ID token
- **THEN** login succeeds without any `amr`/`acr` step-up check, and this absence is intentional per
  ADR-0011

#### Scenario: Non-passkey issuer triggers the deferred requirement

- **WHEN** a maintainer adds a non-passkey IdP to the trust set
- **THEN** the deferred-hardening requirement applies: a phishing-resistant `amr`/`acr` claim MUST be
  required and verified on consent actions before that issuer's logins or provenance are trusted

### Requirement: Development Login Guard

A development login that bypasses OIDC MAY exist for local use, but it MUST be gated by explicit
configuration (`SWITCHBOARD_DEV_LOGIN`) and MUST NOT be enabled in production. When used, it MUST log
a prominent warning that OIDC was bypassed.

#### Scenario: Dev login is off by default

- **WHEN** the dev-login flag is not set and a request hits the dev-login route
- **THEN** switchboard MUST respond as not found and MUST NOT establish a session

## Security Requirements

### Authentication

All endpoints MUST require authentication by default. Login and callback endpoints are necessarily
public entry points to the OIDC flow; the protected human surface requires a session.

| Endpoint | Auth | Justification |
|----------|------|---------------|
| `GET /login` (login page) | Public | Entry point to authenticate; renders login options only, no protected data |
| `GET /auth/login` (start OIDC) | Public | Initiates the OIDC flow before a session exists |
| `GET /auth/callback` | Public | IdP redirect target; self-verifies via state + nonce + ID-token signature |
| `POST /auth/dev-login` | Public but config-gated | Local dev only; returns 404 unless `SWITCHBOARD_DEV_LOGIN` is set — never enabled in production |
| `GET /logout` | Required (session) | — |
| `GET /` (dashboard) | Required (session) | — |
| `POST /agents`, `GET /agents/{id}`, `POST /agents/{id}/vend`, `POST /endpoints/{id}/revoke` | Required (session + ownership) | Managed under [SPEC-0007](../vended-endpoints/spec.md) |

The callback is public only because it is the IdP's redirect target; it is not unauthenticated in
effect — it MUST reject any request whose `state`/`nonce`/ID-token signature does not verify.

### Rate Limiting

Login and callback endpoints SHOULD be rate-limited to blunt credential-stuffing and callback-abuse
attempts; the MVP relies on a reverse-proxy limit in front of `/auth/*`. A native limiter on the auth
routes is RECOMMENDED future hardening and is deferred. The OIDC state cookie's short lifetime bounds
replay of an in-flight login.

### Security Headers

All HTTP responses (especially the server-rendered login and dashboard pages) MUST include:
- `Content-Security-Policy`: `default-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self' <oidc-issuer>`
- `X-Frame-Options`: DENY
- `X-Content-Type-Options`: nosniff
- `Referrer-Policy`: strict-origin-when-cross-origin

### Request Body Size Limits

Auth POST endpoints (`/auth/dev-login`, and any form POST) MUST bound their bodies with
`http.MaxBytesReader`. Default limit: 64 KiB (auth forms carry no large payloads).

### CSRF Protection

State-changing session-authenticated endpoints (`logout` and the human management POSTs) MUST
implement CSRF protection. Strategy: the session cookie is `SameSite=Lax` (blocking cross-site form
POSTs), and state-changing forms MUST additionally carry a per-session CSRF token verified server-side.
The OIDC `state` parameter provides CSRF protection for the login/callback exchange itself.

### Redirect Validation

The only redirects are switchboard-internal: login redirects to Pocket ID's authorization endpoint
(the configured issuer, not a user-supplied target), and post-login/logout redirect to the fixed local
path `/`. No redirect in this capability takes a user-supplied target, so open redirects MUST NOT be
possible; if a post-login "return to" target is ever added, it MUST be validated against an allowlist
of local paths.

## Accessibility Requirements

The login page and any auth-related server-rendered UI MUST conform to WCAG 2.1 AA:

- **ARIA landmarks**: the page MUST expose `banner`, `main`, and `contentinfo` landmarks; the login
  form lives within `main`.
- **Icon-only controls**: any icon-only control (e.g. a provider logo button) MUST carry an
  `aria-label` describing its action (e.g. "Log in with Pocket ID").
- **Dynamic regions**: any auth error or status message MUST be announced via `aria-live` (polite for
  status, assertive for errors).
- **Keyboard navigation**: the login controls MUST be reachable and operable by keyboard in a logical
  tab order; the primary "Log in" control MUST activate on Enter and Space.
- **Focus management**: on load, focus SHOULD land on the primary login control; if an error is shown,
  focus SHOULD move to the error region so it is announced.
