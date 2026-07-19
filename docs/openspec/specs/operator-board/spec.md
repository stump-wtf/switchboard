---
status: deprecated
date: 2026-07-10
implements: [ADR-0016, ADR-0001]
requires: [SPEC-0003, SPEC-0007, SPEC-0012]
extends: [SPEC-0012]
---

# SPEC-0013: Operator Board

## Overview

The operator board is switchboard's five-view human surface — **Board**, **Todos**, **Endpoints**,
**Personas**, and **Friends** — realizing the high-fidelity "Operator" designs adopted in
[ADR-0016](../../../adrs/ADR-0016-operator-design-language.md) on top of the web stack of
[ADR-0001](../../../adrs/ADR-0001-web-stack-go-htmx-pico.md). It extends the
[SPEC-0012](../web-ui/spec.md) baseline (embedded templates, OIDC sessions, CSRF, security headers,
SSE plumbing, error handling, WCAG 2.1 AA) and **supersedes SPEC-0012's "Screen Set and Routes" and
"Semantic HTML and Classless Styling" requirements**: the dashboard/agent/vended screens are replaced
by the Board/Todos/Endpoints views, and styling moves from classless Pico to the owned
`tokens.css` + `.sb-*` component layer. The Personas and Friends views ship with their backing
capabilities ([SPEC-0009](../personas/spec.md), [SPEC-0010](../friending/spec.md)) and MUST NOT block
the other three views.

## Requirements

### Requirement: Information Architecture and Navigation

The UI MUST present a persistent top bar (brand mark + wordmark, a live throughput pill, the
authenticated human's initials) and a left navigation rail with entries **Board**, **Todos** (with a
live todo count), **Endpoints**, **Personas**, and **Friends** (with a pending-incoming-request count
badge when nonzero), plus a footer connectivity indicator (`postgres · connected`) reflecting the
health of the database pool. `GET /` MUST render the Board view. Below 820px viewport width the rail
MUST collapse to icons-only. Views whose backing capability is not yet enabled (Personas, Friends)
MUST be hidden from the rail rather than rendered broken.

#### Scenario: Board is the landing view

- **WHEN** an authenticated human requests `GET /`
- **THEN** the Board view MUST render with the rail marking Board active and the top bar showing the
  live events-per-minute rate

#### Scenario: Database connectivity is reflected

- **WHEN** the database pool fails a health ping
- **THEN** the rail footer indicator MUST change from the connected state to a visually and textually
  distinct disconnected state

### Requirement: Board View — Live Incoming Lines

The Board view MUST show: a trust legend for the four trust modes (`signed`, `token`, `open`,
`queue` — [ADR-0003](../../../adrs/ADR-0003-per-provider-ingestion-and-trust-model.md)); summary
tiles for throughput (events/min with a recent-activity bar chart), todos in flight, todos awaiting
claim, and percentage of events from verified (signed) sources; and a newest-first live feed of
incoming events. Each feed row MUST show the provider tag and name, event type, a trust badge, and
its lifecycle stage (`verifying…` → `patched → todo` → `claimed · <agent>` → `done ✓`). Feed and
tiles MUST update live via the SSE stream without a page reload, and the awaiting-claim tile MUST be
visually emphasized when its count is nonzero.

#### Scenario: New event appears in the feed live

- **WHEN** a webhook is verified and enqueued as a todo while the Board is open
- **THEN** a new row MUST appear at the top of the feed via SSE with its trust badge and lifecycle
  stage, without a page reload

#### Scenario: Lifecycle stage advances

- **WHEN** an agent claims the todo behind a feed row
- **THEN** the row's stage MUST update to show `claimed · <agent name>` live

### Requirement: Todos View — Durable Queue

The Todos view MUST list todos in a table with filter pills (**All / Pending / Claimed / Done /
Failed**, each with a live count), a text search over id, source, and event type, and columns: id,
source · event (with a `dedup ×N` badge when the idempotency key collapsed N > 1 deliveries), trust,
status (with a sub-line showing remaining lease seconds for claimed todos or retry countdown for
failed todos), agent, age, and a contextual action (**Claim** for pending, **Complete** for claimed,
**Retry** for failed). Rows re-surfaced by the lease reaper MUST be visually highlighted briefly, and
the view MUST surface that the reaper is active. All status transitions MUST reflect the todo-queue
semantics of [SPEC-0003](../todo-queue/spec.md); the UI MUST NOT implement its own lifecycle rules.

#### Scenario: Filter pills scope the table

- **WHEN** the human activates the Failed filter
- **THEN** only todos in the failed state MUST be listed and the pill MUST show the failed count

#### Scenario: Reaper re-surface is visible

- **WHEN** a claimed todo's lease expires and the reaper returns it to pending
- **THEN** the row MUST update to pending via SSE, be briefly highlighted, and a toast MUST announce
  the re-surface

### Requirement: Todo Detail Drawer

Selecting a todo MUST open a right-hand drawer showing: id and status; for claimed todos a lease card
with a live countdown, progress bar, and **Extend lease** / **Release** actions; for failed todos the
attempt count and retry-backoff countdown; the trust mode, agent, attempts, and age; the idempotency
key with a dedup summary; the event payload rendered as formatted JSON (HTML-escaped); a lifecycle
timeline (received/verified → todo created → claimed under lease → completed/failed); and footer
actions appropriate to the state (**Claim under lease**, **Complete · ack** + **Fail**, **Retry
now**). Drawer actions MUST be state-changing POSTs protected per the SPEC-0012 baseline (session +
CSRF), and the drawer MUST close on Escape and on overlay click.

#### Scenario: Lease controls act on the store

- **WHEN** the human activates Extend lease on a claimed todo
- **THEN** the server MUST extend the todo's visibility lease (heartbeat semantics per SPEC-0003) and
  the countdown MUST reflect the new expiry

#### Scenario: Payload is rendered safely

- **WHEN** a todo's payload contains HTML-special characters or script content
- **THEN** the drawer MUST render it inert as escaped text within the formatted block

### Requirement: Endpoints View and Vend Modal

The Endpoints view MUST list vended MCP endpoints as cards showing agent name, bound persona (when
personas are enabled), status (`active`/`revoked`), scoped queues, allowed verbs, the credential
display prefix (never the full credential), and last-seen time, with **Edit** and **Revoke** actions
on active endpoints. A **Vend endpoint** action MUST open a modal that collects agent name, optional
persona, scoped queues, and allowed verbs via toggle chips, and MUST refuse submission until name,
at least one queue, and at least one verb are chosen. On successful vend the modal MUST show, exactly
once, the minted endpoint URL and plaintext credential together with ready-to-paste HTTP `.mcp.json`
wiring per [SPEC-0014](../mcp-transport/spec.md), with a warning that the credential is shown once.
Vend/revoke semantics (hashing at rest, immutability, revoke-kills-endpoint) MUST follow
[SPEC-0007](../vended-endpoints/spec.md); revoked cards MUST be visually dimmed and state when the
endpoint was killed.

#### Scenario: Vend modal validates scope

- **WHEN** the vend form is submitted with no queues or no verbs selected
- **THEN** the submission MUST be rejected and no credential MUST be minted

#### Scenario: Credential reveal is one-time

- **WHEN** the human closes the minted-credential modal
- **THEN** the plaintext credential MUST NOT be retrievable from any subsequent page, card, or
  endpoint of the UI

### Requirement: Personas View

When the personas capability ([SPEC-0009](../personas/spec.md)) is enabled, the Personas view MUST
list personas as cards showing name, backing agent, published/draft state, the human-authored system
prompt, the advertised verb subset, and the agent-card URL
(`/a/{persona_id}/.well-known/agent-card.json`, the per-persona well-known path of
[SPEC-0009](../personas/spec.md)), with
**Edit** and **Publish/Unpublish** actions. A create/edit modal MUST collect name (with a live
agent-card URL preview), backing agent, system prompt, verb subset (constrained to the backing
endpoint's vended verbs), and a published toggle; deleting MUST be available only from the edit
modal. Publish state changes MUST take effect on the well-known endpoint immediately.

#### Scenario: Verb subset is constrained

- **WHEN** the human edits a persona whose backing agent's endpoint lacks a verb
- **THEN** that verb MUST NOT be selectable for the persona

### Requirement: Friends View

When the friending capability ([SPEC-0010](../friending/spec.md)) is enabled, the Friends view MUST
present friendships in two switchable layouts — cards and a grouped ledger (Incoming / Outgoing /
Active / Blocked) — with filter pills and counts. Each friendship MUST show the local agent, the
remote agent (handle and board host), status, negotiated or requested intents, and status-appropriate
actions: **Approve**/**Decline** for incoming, **Withdraw** for outgoing, **Revoke** for active,
**Unblock** for blocked. An **Add friend** modal MUST collect the local agent, remote handle (with
live resolution preview), requested intents, and a message to the remote operator, and MUST state
that links are mutual and non-transitive and remain pending until the remote operator approves.

#### Scenario: Incoming request is approved

- **WHEN** the human approves an incoming friendship request
- **THEN** the friendship MUST become active via the SPEC-0010 approval flow and move to the Active
  group live

### Requirement: Design Language Conformance

All views MUST be styled exclusively by the design-token layer (`static/tokens.css`) and the `.sb-*`
component layer (`static/switchboard.css`) defined by [ADR-0016](../../../adrs/ADR-0016-operator-design-language.md),
using the vendored Zilla Slab / IBM Plex Sans / IBM Plex Mono woff2 faces served from
`static/fonts/`. Templates MUST NOT reference external stylesheets, font CDNs, or utility-class
frameworks. Trust and status colors MUST come from the named tokens (signed/token/open/queue;
pending/claimed/done/failed) and MUST NOT be hard-coded per-template. Pico.css MUST NOT be served.

#### Scenario: No external origins

- **WHEN** any view renders under the baseline CSP (`default-src 'self'`)
- **THEN** all styles, fonts, scripts, and icons MUST load from same-origin embedded assets with no
  CSP violations

### Requirement: Live Updates and Toasts

Live regions across all views MUST subscribe to the authenticated UI SSE stream (SPEC-0012 baseline)
carrying typed events for at minimum: event received, todo created, todo claimed, todo completed,
todo failed, todo re-surfaced (reaper), and endpoint last-seen updates. Queue lifecycle transitions
initiated by others (agents, the reaper) MUST surface as transient toast notifications. SSE remains
best-effort presentation: a dropped event MUST NOT corrupt view state and a reload MUST render
authoritative database state.

#### Scenario: Toast on background transition

- **WHEN** an agent completes a todo while the human has the Todos view open
- **THEN** the row MUST update to done and a toast MUST announce the completion with the todo id

### Requirement: Error Handling Standards

All error-producing operations MUST follow structured error handling:

- Errors MUST be wrapped with contextual information at each layer boundary
- Sentinel errors MUST be defined for domain-specific failure modes callers distinguish
  programmatically (e.g. not-found vs conflict on claim)
- Silent error swallowing MUST NOT occur — every error MUST be returned, logged with context, or
  explicitly suppressed with a documented reason
- Structured logging MUST be used for error reporting (key-value pairs, not string interpolation)
- User-facing failures MUST follow the SPEC-0012 baseline: generic status responses, no internal
  detail leakage

#### Scenario: Action failure is generic to the user, specific in the log

- **WHEN** a drawer action fails against the store
- **THEN** the user MUST receive a generic error response while the server log records the handler,
  todo id, and wrapped error

## Endpoints

| Method | Path | Auth | Description |
|--------|------|------|-------------|
| GET | `/` | Required | Board view |
| GET | `/todos` | Required | Todos view (query params: filter, q) |
| GET | `/todos/{id}` | Required | Todo detail drawer fragment |
| POST | `/todos/{id}/claim` | Required | Claim under lease (as the operator) |
| POST | `/todos/{id}/complete` | Required | Complete · ack |
| POST | `/todos/{id}/fail` | Required | Mark failed (schedules retry) |
| POST | `/todos/{id}/retry` | Required | Re-queue a failed todo now |
| POST | `/todos/{id}/extend` | Required | Extend lease (heartbeat) |
| POST | `/todos/{id}/release` | Required | Release lease back to pending |
| GET | `/endpoints` | Required | Endpoints view |
| GET | `/endpoints/vend` | Required | Vend-endpoint modal fragment |
| POST | `/endpoints/vend` | Required | Vend a scoped endpoint (modal submit) |
| POST | `/endpoints/{id}/revoke` | Required | Revoke (kill) an endpoint |
| GET | `/agents`, `/agents/{id}` | Required | Legacy SPEC-0012 paths; redirect to `/endpoints` |
| GET | `/personas` | Required | Personas view (capability-gated) |
| POST | `/personas` | Required | Create persona |
| POST | `/personas/{id}` | Required | Update persona (incl. publish toggle) |
| POST | `/personas/{id}/delete` | Required | Delete persona |
| GET | `/friends` | Required | Friends view (capability-gated) |
| GET | `/friends/new` | Required | Add-friend modal fragment |
| GET | `/friends/resolve` | Required | Resolve a friend handle (live card preview fragment) |
| POST | `/friends` | Required | Send friendship request |
| POST | `/friends/{id}/approve` | Required | Approve incoming request |
| POST | `/friends/{id}/decline` | Required | Decline incoming request |
| POST | `/friends/{id}/withdraw` | Required | Withdraw outgoing request |
| POST | `/friends/{id}/revoke` | Required | Revoke (block) an active link |
| POST | `/friends/{id}/unblock` | Required | Remove a blocked entry |
| GET | `/events` | Required | UI SSE stream (session-authenticated) |

All previously specified public routes (login, auth, static, healthz) retain their SPEC-0012
designations; this spec introduces no new public endpoints.

## Security Requirements

This capability inherits the full SPEC-0012 security baseline and re-affirms it for the new surface:

### Authentication

Every route in the table above MUST require an authenticated human session (`RequireHuman`); the SSE
stream (`GET /events`) MUST be session-authenticated and scoped to the human's own data. No new
public endpoints are introduced.

### Rate Limiting

State-changing POST routes SHOULD share the human-surface rate limiter; the SSE endpoint MUST cap
concurrent streams per session to a small fixed number to bound resource use.

### Security Headers

All HTML and SSE responses MUST carry the SPEC-0012 header set (CSP `default-src 'self'` with
same-origin `connect-src` for SSE, `X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff`,
`Referrer-Policy: strict-origin-when-cross-origin`). The design-language work MUST NOT introduce any
external origin.

### Request Body Size Limits

All POST handlers MUST bound bodies with a 1 MiB limit before parsing (forms carry only names,
prompts, and chip selections).

### CSRF Protection

All state-changing POSTs (todo actions, vend, revoke, persona and friend mutations) MUST carry the
per-session synchronizer token per the SPEC-0012 baseline, including requests issued by HTMX; the
CSRF token MUST be injected into HTMX request headers or hidden fields uniformly.

### Redirect Validation

Post-action redirects MUST target fixed same-origin paths (the originating view); no user-supplied
redirect targets are accepted.

## Accessibility Requirements

This spec involves user-facing UI. The following accessibility requirements are MANDATORY per WCAG
2.1 AA and extend the SPEC-0012 baseline to the new components.

### WCAG 2.1 AA Compliance

All five views, the drawer, modals, and toasts MUST meet WCAG 2.1 Level AA, including contrast under
both the operator-cream (light) and bakelite (dark) themes.

### ARIA Landmarks

The top bar MUST be `banner`, the rail `navigation`, each view's content `main` (exactly one per
page), and any footer `contentinfo`. The rail MUST expose the active view via `aria-current="page"`.

### Icon-Only Controls

Icon-only controls (the brand mark, drawer close, rail icons when collapsed) MUST have accessible
names via `aria-label` or SVG `<title>` + `role="img"`. Trust and status MUST NOT be conveyed by
color alone — badges always carry text.

### Dynamic Content Regions

The Board feed, Todos table, count pills, and toast container MUST be `aria-live="polite"` regions
(toast container MAY be `assertive` for failure toasts) existing in the DOM before first update.

### Keyboard Navigation

All interactive elements — filter pills, chip toggles, table rows' actions, drawer and modal controls
— MUST be reachable in logical tab order and operable with Enter/Space; Escape MUST dismiss the
drawer and any modal; the lease countdown MUST NOT trap focus or steal it on tick.

### Focus Management

The drawer and every modal MUST trap focus while open, set initial focus to the first meaningful
control, close on Escape, and return focus to the triggering control on close.
