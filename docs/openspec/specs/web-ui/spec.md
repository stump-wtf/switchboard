---
status: approved
date: 2026-07-06
implements: [ADR-0001]
requires: [SPEC-0008, SPEC-0011]
---

# SPEC-0012: Web UI

## Overview

The web UI is switchboard's small, server-rendered, human-facing surface — the operator's board —
realizing [ADR-0001](../../../adrs/ADR-0001-web-stack-go-htmx-pico.md). It is served from the single
Go binary by `internal/web/web.go` with `internal/server/server.go` wiring the routes. A human logs
in (OIDC via Pocket ID, or a dev-only local login), registers agents, and vends/revokes scoped MCP
endpoints those agents plug into Claude Code Channels ([ADR-0013](../../../adrs/ADR-0013-channels-push-delivery.md)).

The stack is deliberately minimal per ADR-0001: `net/http` + chi routing, stdlib `html/template`
(contextual auto-escaping), HTMX core + `htmx-ext-sse` for interactivity, Pico.css (classless) plus a
`tokens.css` palette override, and inline SVG icons — all **vendored and embedded via `embed.FS`**, no
Node build, no CDN. Live updates use Server-Sent Events over a plain `net/http` handler using
`http.Flusher` (no SSE library). The screen set is small: **login** (public), **dashboard** (the
human's agents), **agent** (one agent + its endpoints + the vend form), and **vended** (the
one-time credential reveal + `.mcp.json` wiring). Templates compose a shared `layout` with a
per-page `content` block.

## Requirements

### Requirement: Server-Rendered Pages from Embedded Templates

The UI MUST be rendered on the server with stdlib `html/template` and MUST NOT require a client-side
SPA framework or a Node/bundler build step. All templates and static assets (vendored HTMX, Pico.css,
`tokens.css`, inline SVG icons) MUST be embedded in the binary via `embed.FS` and served without a
runtime CDN. Each page MUST compose the shared `layout` template with its own `content` block.
Template output MUST rely on `html/template`'s contextual auto-escaping; user-supplied values MUST NOT
be emitted via any mechanism that bypasses that escaping.

#### Scenario: Pages render from embedded templates

- **WHEN** the server starts
- **THEN** it MUST parse `layout.html` plus each page template (`login`, `dashboard`, `agent`,
  `vended`) from the embedded FS, and MUST fail startup if any template fails to parse

#### Scenario: Static assets served from embed, not a CDN

- **WHEN** a browser requests `/static/*`
- **THEN** the asset MUST be served from the embedded `static/` FS, so the UI works offline and under
  a strict CSP with no external origins

### Requirement: Screen Set and Routes

The UI MUST expose exactly these screens and routes: a public `GET /login`; and authenticated
`GET /` (dashboard), `POST /agents` (register agent), `GET /agents/{id}` (agent detail + vend form),
`POST /agents/{id}/vend` (mint a scoped credential), and `POST /endpoints/{id}/revoke`. The dashboard
MUST list only the authenticated human's agents. The agent page MUST show only agents owned by the
authenticated human and MUST return 404 for an agent the human does not own.

#### Scenario: Dashboard scopes to the authenticated human

- **WHEN** an authenticated human loads `GET /`
- **THEN** the page MUST list only that human's agents (via the human id from the request context)

#### Scenario: Accessing another human's agent is not found

- **WHEN** an authenticated human requests `GET /agents/{id}` for an agent they do not own
- **THEN** the server MUST respond `404 Not Found` and MUST NOT reveal the agent's existence or
  contents

### Requirement: Vend Flow and One-Time Credential Reveal

The vend form MUST require both `queues` and `verbs` (comma-separated) and MUST reject a submission
missing either with `400 Bad Request`. On success the server MUST mint a credential, persist only its
hash and a display prefix, and render the plaintext credential **exactly once** on the vended page
together with ready-to-paste `.mcp.json` wiring for Claude Code Channels. The plaintext credential
MUST NOT be persisted and MUST NOT be recoverable after the reveal. Endpoint scope MUST be immutable;
changing access MUST require revoke-and-re-vend.

#### Scenario: Missing scope is rejected

- **WHEN** the vend form is submitted with empty `queues` or empty `verbs`
- **THEN** the server MUST respond `400 Bad Request` and MUST NOT mint a credential

#### Scenario: Credential is shown once and stored only as a hash

- **WHEN** an endpoint is vended
- **THEN** the plaintext credential MUST be displayed once on the vended page with `.mcp.json` wiring,
  and only its hash and prefix MUST be persisted

### Requirement: Live Updates via SSE

Live-updating regions MUST use Server-Sent Events delivered by a plain `net/http` handler using
`http.Flusher`, with no third-party SSE dependency, and MUST be subscribed from the DOM via
`htmx-ext-sse`. SSE responses MUST set `Content-Type: text/event-stream`, `Cache-Control: no-cache`,
and advertise a client `retry:` interval, and MUST emit periodic keep-alive comments to hold the
connection open. SSE delivery MUST be treated as best-effort presentation: a missed event MUST NOT
corrupt state, since the authoritative data lives in PostgreSQL and a reload reflects current truth.

#### Scenario: SSE stream sets streaming headers and keep-alive

- **WHEN** a client subscribes to the events stream
- **THEN** the handler MUST set `Content-Type: text/event-stream` and `Cache-Control: no-cache`, send
  an initial `retry:` frame, and emit keep-alive comments on an interval to keep the connection alive

#### Scenario: Missed SSE event is recoverable by reload

- **WHEN** an SSE event is dropped (slow consumer or disconnect)
- **THEN** the UI state MUST NOT be corrupted, and reloading the page MUST show the current
  authoritative state from the database

### Requirement: Semantic HTML and Classless Styling

Templates MUST use semantic HTML (`<header>`, `<main>`, `<nav>`, `<table>`, `<form>`, `<article>`)
styled by classless Pico.css plus the `tokens.css` palette override, keeping markup free of a utility
class system. Icons MUST be inline SVG themed via `currentColor` (Lucide for UI chrome, Simple Icons
for provider/brand marks) rather than an icon webfont. The `<html>` element MUST declare a language
(`lang`) and the document MUST declare a responsive viewport.

#### Scenario: Semantic landmarks are present

- **WHEN** any page renders
- **THEN** the markup MUST wrap the page chrome in a `<header>` and the primary content in a `<main>`,
  styled by Pico.css defaults without utility classes

### Requirement: Error Handling and Server-Side Logging

Handler failures MUST be surfaced to the user as generic status responses (e.g. `500 internal error`,
`404 not found`, `400` for bad input) without leaking internal error detail, while the underlying
error MUST be logged server-side with structured context. A template render failure MUST respond
`500` and log the page and error; a store `ErrNotFound` MUST map to `404`; other store errors MUST map
to `500`. Errors MUST NOT be silently swallowed.

#### Scenario: Internal errors are logged, not leaked

- **WHEN** a store or render operation returns an error
- **THEN** the response body MUST be a generic message (no stack traces or internal details) AND the
  error MUST be logged server-side with structured context (page/handler + error)

## Security Requirements

### Authentication

All UI endpoints MUST require an authenticated human by default. The authenticated-human routes are
grouped behind the `RequireHuman` middleware, which resolves the session cookie to a human and injects
it into the request context. Only the login page and the OIDC/dev auth endpoints are public, each with
explicit justification.

| Endpoint | Auth | Justification |
|----------|------|---------------|
| `GET /` (dashboard) | Required | Human session (RequireHuman); scoped to the human's agents |
| `POST /agents` (register agent) | Required | Human session; state-changing |
| `GET /agents/{id}` (agent detail) | Required | Human session; ownership-checked (404 if not owned) |
| `POST /agents/{id}/vend` | Required | Human session; mints a credential; state-changing |
| `POST /endpoints/{id}/revoke` | Required | Human session; ownership-checked; state-changing |
| `GET /login` | Public | Unauthenticated humans must reach the login screen to begin auth |
| `GET /auth/login` | Public | Initiates the OIDC redirect to Pocket ID; pre-authentication by definition |
| `GET /auth/callback` | Public | OIDC redirect target; authenticated by the OIDC `state` + code exchange, not a session |
| `POST /auth/dev-login` | Public (dev only) | Local no-OIDC login; MUST be enabled only when `SWITCHBOARD_DEV_LOGIN` is set, never in production |
| `GET /logout` | Public | Clears the session cookie; safe for an unauthenticated caller |
| `GET /static/*` | Public | Vendored, non-sensitive CSS/JS/SVG assets embedded in the binary |
| `GET /healthz` | Public | Liveness/readiness probe (DB ping); returns no sensitive data |

The session cookie MUST be `HttpOnly`, `SameSite=Lax`, and `Secure` when served over TLS, and MUST be
cleared on logout. The OIDC `state` value MUST be carried in a short-lived cookie and MUST be verified
against the callback's `state` query parameter to prevent login CSRF; a mismatch MUST reject the
callback.

### Rate Limiting

Login/auth endpoints SHOULD be rate-limited to blunt credential-stuffing and OIDC-callback abuse; a
per-IP limit on `GET /auth/login`, `GET /auth/callback`, and `POST /auth/dev-login` is RECOMMENDED.
Application routes are gated by authentication and are single-operator in nature, so a dedicated
per-route limit is deferred for the MVP. When switchboard is exposed beyond localhost, a reverse-proxy
or middleware rate limit on the public auth endpoints MUST be enabled.

### Security Headers

All HTML responses MUST include:
- `Content-Security-Policy`: `default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline';
  img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action
  'self'` — everything is vendored/embedded, so no external origins are needed; `connect-src 'self'`
  permits the same-origin SSE stream, and inline `<style>`/inline SVG in templates require
  `style-src 'unsafe-inline'`.
- `X-Frame-Options`: DENY
- `X-Content-Type-Options`: nosniff
- `Referrer-Policy`: strict-origin-when-cross-origin

### Request Body Size Limits

All endpoints accepting request bodies (the `POST` form handlers) MUST bound the body with
`http.MaxBytesReader` before parsing. Default limit: 1 MiB — the forms carry only short names,
descriptions, and comma-separated queue/verb lists, so a small bound is safe.

### CSRF Protection

State-changing endpoints (`POST /agents`, `POST /agents/{id}/vend`, `POST /endpoints/{id}/revoke`,
`POST /auth/dev-login`) MUST implement CSRF protection. Strategy: the `SameSite=Lax` session cookie
provides a baseline against cross-site POSTs; in addition, state-changing form submissions MUST carry
a per-session CSRF token (synchronizer-token pattern) validated server-side, with the token embedded
as a hidden field in each rendered form. The OIDC login flow's CSRF is handled by the `state` cookie
↔ `state` parameter check described under Authentication.

### Redirect Validation

Redirects in this capability MUST target only fixed internal paths: after registering an agent the
server redirects to `/agents/{id}` (a server-minted id), and after vend/revoke to a fixed or
same-origin location. The revoke handler redirects to the `Referer`; it MUST validate that the target
is a same-origin, in-app path (or fall back to a safe default such as `/`) and MUST NOT honor an
off-site or attacker-supplied `Referer`. The OIDC callback MUST redirect only to a fixed post-login
path, never to a user-supplied `redirect`/`next` parameter. Open redirects MUST NOT be permitted.

## Accessibility Requirements

The UI MUST conform to WCAG 2.1 Level AA.

### Landmarks and Structure

Each page MUST expose ARIA landmark structure via semantic elements: the top chrome as `banner`
(`<header>`), primary navigation as `navigation` (`<nav>`), the main content as `main` (`<main>`), and
any footer as `contentinfo`. There MUST be exactly one `main` landmark per page. Headings MUST form a
logical, non-skipping hierarchy (a single `<h1>` per page, then `<h2>` sections). Tables (agents,
endpoints) MUST use `<th>` header cells with an appropriate scope so assistive technology can
associate data cells with headers.

### Labels and Icon-Only Controls

Every form control MUST have a programmatically associated `<label>`. Any icon-only or
symbol-only control (e.g. the brand mark, an SVG action icon) MUST provide an accessible name via
`aria-label` (or an SVG `<title>` with `role="img"`), so it is not announced as unlabeled. Color MUST
NOT be the sole means of conveying trust/state (the `signed`/`token`/`open`/`queue`/`active`/`revoked`
badges MUST also carry text).

### Dynamic Regions (HTMX / SSE)

Any region updated by HTMX swaps or SSE MUST be marked with an appropriate `aria-live` value
(`polite` for status/log updates, `assertive` only for urgent changes) so assistive technology
announces the update without a full-page reload. Live regions MUST exist in the DOM before updates
arrive so the first swap is announced.

### Keyboard Navigation and Focus

All interactive controls (links, buttons, form fields, the revoke action) MUST be reachable and
operable by keyboard in a logical tab order, activatable with Enter (and Space for buttons). A visible
focus indicator MUST be present and MUST NOT be suppressed. If any modal/dialog is introduced, it MUST
trap focus while open, set initial focus to the first meaningful control, close on Escape, and return
focus to the triggering control on close. Contrast for text and UI components MUST meet WCAG 2.1 AA
ratios under both the default bakelite-dark and operator-cream light themes.

#### Focus Management Note

The current screens are non-modal server-rendered pages; the modal focus-management requirements above
apply prospectively to any dialog/overlay added later (e.g. a confirm-revoke dialog) and MUST be
satisfied before such a control ships.
