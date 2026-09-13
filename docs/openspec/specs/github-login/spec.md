---
status: draft
date: 2026-09-13
implements: [ADR-0026]
extends: [SPEC-0008, SPEC-0010]
---

# SPEC-0021: GitHub Login for the Operator Web UI

## Graph Edges

- **Implements:** [ADR-0026](../../adrs/ADR-0026-github-second-human-login-provider.md) — GitHub as a second human login provider with issuer-gated consent actions.
- **Extends:** [SPEC-0008](../identity/spec.md) — the human identity & assurance model this capability extends with a non-passkey provider.
- **Extends:** [SPEC-0010](../friending/spec.md) — the friending flow whose consent endpoints gain the passkey-session gate.

## Overview

The operator web UI (SPEC-0012, SPEC-0015) authenticates humans against
Pocket ID via OIDC (SPEC-0008, ADR-0011). This capability adds **"Log in with
GitHub"** as a second production provider, per ADR-0026. A GitHub login
establishes the same session type (same cookie machinery, same `Human`
principal) with recorded provider provenance — and, critically, with a
different *assurance class*: consent actions (friend approvals and any
capability-minting action, SPEC-0010) are gated to passkey-only-issuer
sessions, closing ADR-0011's deferred-hardening requirement by construction.

GitHub issues no OIDC ID token for user login; identity verification is OAuth
2.0 code exchange plus `GET /user` and `GET /user/emails`, anchored on the
primary verified email.

## Requirements

### Requirement: Provider Selection on the Login Page

The login page MUST render a "Log in with GitHub" button when the GitHub
provider is configured, and MUST NOT render it otherwise.

#### Scenario: GitHub provider configured

- **WHEN** a visitor loads the login page with GitHub client credentials configured
- **THEN** both the Pocket ID login control and a "Log in with GitHub" button are shown, and the button starts `/auth/login?provider=github`

#### Scenario: GitHub provider not configured

- **WHEN** a visitor loads the login page on a deployment without GitHub credentials
- **THEN** no GitHub control is rendered and `/auth/login?provider=github` returns 404

### Requirement: GitHub OAuth Callback Exchange

Switchboard MUST complete the GitHub authorization-code flow at
`GET /auth/callback?provider=github&code=...&state=...`: validate `state`
against the login cookie, exchange the code at
`https://github.com/login/oauth/access_token`, fetch `GET /user` and
`GET /user/emails`, and require a primary email with `verified == true`.

#### Scenario: Successful GitHub login

- **WHEN** a user completes GitHub consent with a verified primary email
- **THEN** a session is established for that human principal with `iss=github.com` and the provider subject recorded, and the user lands on the operator board

#### Scenario: Unverified or missing primary email

- **WHEN** GitHub returns no primary email with `verified == true`
- **THEN** login is rejected with a user-visible error, no session is established, and the reason is logged without the token

#### Scenario: Invalid or replayed state

- **WHEN** the callback `state` does not match the state cookie, or the cookie is expired or absent
- **THEN** the callback is rejected before any token exchange

### Requirement: Consent Actions Require a Passkey-Issuer Session

Every consent action — friend request approval, and any other endpoint that
mints a capability or vends access — MUST require that the acting session's
issuer is the passkey-only IdP (Pocket ID). Sessions established by GitHub
MUST receive a distinguishable rejection (HTTP 403 with a user-visible
explanation and a link to re-authenticate via Pocket ID) on those endpoints,
and MUST be permitted on all other operator-board routes.

#### Scenario: GitHub session attempts a friend approval

- **WHEN** a session with `iss=github.com` posts an approval on a friending consent endpoint
- **THEN** the action is rejected with HTTP 403, nothing is minted, and the response explains that approval requires Pocket ID authentication

#### Scenario: Pocket ID session performs a friend approval

- **WHEN** a session with the Pocket ID issuer posts the same approval
- **THEN** the action succeeds exactly as it does today (SPEC-0010 unchanged)

#### Scenario: GitHub session uses ordinary board features

- **WHEN** a session with `iss=github.com` lists todos, views providers, or inspects webhook events
- **THEN** every non-consent route behaves exactly as it does for a Pocket ID session

### Requirement: Session Parity and Provenance

A GitHub session MUST be indistinguishable from a Pocket ID session to all
non-consent consumers, except that the session MUST record `iss` and the
provider's `sub` so provenance and the issuer gate are auditable.

#### Scenario: Session provenance is recorded

- **WHEN** a session is established by either provider
- **THEN** the session record carries `iss` and `sub` identifying the provider and provider-side subject

### Requirement: Token Containment

GitHub access tokens MUST be used only inside the callback to fetch the
profile and MUST NOT be persisted in sessions, cookies, logs, or the
database. GitHub API calls in steady state MUST be zero.

#### Scenario: Token lifetime

- **WHEN** a GitHub callback completes and the session is established
- **THEN** the access token is dropped and never appears in any persisted store or log line

## Endpoint Inventory

| Method | Path | Auth | Notes |
|---|---|---|---|
| GET | /auth/login | Public | Login page; renders provider controls |
| GET | /auth/callback | Public | OAuth callback for either provider; state-validated |
| POST | /auth/logout | Required | Unchanged |
| GET | / (operator board) | Required | GitHub sessions allowed |
| GET/POST | todo queue, providers view, webhook inspection | Required | GitHub sessions allowed |
| POST | friending approve/reject (consent) | Required + **passkey issuer** | GitHub sessions rejected 403 per ADR-0026 |
| POST | any future capability-minting action | Required + **passkey issuer** | Same gate by policy |

## Security Requirements

This is a web-facing spec. Topics not restated here follow the existing
identity baseline (SPEC-0008, ADR-0011):

- **Authentication**: Per SPEC-0008 — HttpOnly, SameSite=Lax signed session
  cookies with server-side revocation; GitHub login reuses that machinery
  verbatim and additionally enforces the issuer gate above.
- **Rate limiting**: The state cookie's single-use, short-TTL property is the
  primary anti-replay control on `/auth/callback`; per-IP throttling SHOULD be
  added if abuse is observed.
- **Security headers**: Per the existing shell middleware (CSP and friends) —
  unchanged; the login button adds no inline script.
- **Request body size limits**: Per existing middleware; no new
  body-consuming endpoints.
- **CSRF protection**: The OIDC state cookie (random, HMAC-bound, single-use,
  short TTL) is reused as the GitHub flow's `state` parameter, giving login
  CSRF protection identical to the existing flow; all POST routes keep the
  existing CSRF middleware.
- **Redirect validation**: The post-login redirect target MUST come only from
  the state cookie's allow-listed destination; arbitrary `next` parameters on
  the GitHub callback MUST NOT be honored.

## Accessibility Requirements

This spec touches the login page UI. The following are MANDATORY for the
GitHub login button and its error surfaces, per WCAG 2.1 AA:

- **WCAG 2.1 AA compliance** — the minimum conformance target
- **ARIA landmarks** — the login page's existing landmarks are preserved
- **`aria-label` on icon-only controls** — the GitHub button MUST carry an accessible name ("Log in with GitHub") even when rendered with only the GitHub mark
- **`aria-live` regions for dynamic content** — login errors (rejected email, state mismatch) and the 403 consent-gate explanation MUST be announced via a polite live region
- **Keyboard navigation** — the button is reachable in tab order and activates with Enter/Space like the existing login controls
- **Focus management in modals and dialogs** — not applicable; this capability introduces no modal
