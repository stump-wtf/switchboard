---
status: draft
date: 2026-07-18
implements: [ADR-0018]
supersedes: [SPEC-0013]
requires: [SPEC-0003, SPEC-0007, SPEC-0012]
---

# SPEC-0015: Operator Board v2 (Charm-Web)

## Overview

The redesigned human surface: the charm-web design language (ADR-0018) applied across a five-view
information architecture — **board · todos · endpoints · personas · friends** — with a
three-lane live patch-panel board, full-page wizards, a global keyboard map, and day/night themes.
Supersedes SPEC-0013 (Operator Board). The durable architecture of SPEC-0012 (embedded templates,
HTMX + SSE, CSRF, error standards, no-JS fallbacks) is inherited, not restated.

> **Amended 2026-09-21, with the shared-receiver removal.** The sixth view, Providers, was removed with the provider registry
> ([SPEC-0017](../providers-view/spec.md), retired).

## Requirements

### Requirement: Design Token System

The UI SHALL be styled exclusively through a charm-web token layer replacing the Operator/brass
tokens: a day theme (lavender-paper, deepened accents) and a night theme (blue-black, neon), both
complete, selected via `prefers-color-scheme` with an `<html data-theme>` override. Trust-mode and
todo-state colors SHALL exist as named tokens in both themes, and token text/background pairs SHALL
pass WCAG AA, enforced by porting the existing contrast test to the new palette.

#### Scenario: Contrast gate

- **WHEN** the token stylesheet changes
- **THEN** the automated contrast test fails the build if any declared token pair drops below AA in
  either theme

### Requirement: Typography And Vendored Fonts

The UI SHALL use JetBrains Mono (body/UI/code) and Space Mono (display), vendored as woff2 under
`static/fonts/` and embedded — no font CDN. The Zilla Slab and IBM Plex families SHALL be removed.
The font-coverage test SHALL retarget to the new files.

#### Scenario: Offline serve

- **WHEN** the binary runs with no outbound network
- **THEN** all UI text renders in the vendored faces with zero external requests

### Requirement: Application Shell And Navigation

The shell SHALL present the top bar (wordmark, `~/operator` breadcrumb, MCP-connected indicator,
"+ new" wizard launcher, theme control) and a five-view navigation: board, todos, endpoints,
personas, friends. Every view SHALL end in a key-hint footer rendering the active
keymap. The login page SHALL be restyled in the same language.

#### Scenario: Navigation offers every view

- **WHEN** an operator opens any view
- **THEN** navigation offers all five views, with the active view indicated

### Requirement: Theme Toggle

The shell SHALL provide a visible theme control (and the `t` key) cycling day/night, persisted in
`localStorage`, applied before first paint without flashing the wrong theme, and without violating
the same-origin CSP (no inline scripts; a tiny external boot script or CSP hash).

#### Scenario: Returning operator

- **WHEN** an operator who chose night reloads on a day-preferring OS
- **THEN** the page paints night from the first frame

### Requirement: Patch Panel Board

The board SHALL render three lanes — **received**, **verified**, **patched through** — as the live
view of every inbound line. Lane semantics: *received* holds ephemeral in-flight cards (an event
arriving/being verified, SSE-only, never persisted as a distinct state — rejected events leave the
lane with a rejection surface, accepted ones advance); *verified* holds durable todos not yet
claimed (trust check passed, normalized, deduped); *patched through* holds todos claimed by or
completed under an agent. Cards SHALL carry provider glyph, title, trust chip, state chip, and age;
lane headers SHALL carry live counts. Cards SHALL move between lanes over SSE without reload.

#### Scenario: A signed webhook crosses the board

- **WHEN** a signed GitHub webhook is received, verified, stored as a todo, and later claimed
- **THEN** its card appears in *received*, advances to *verified* on todo creation, and moves to
  *patched through* on claim — each transition pushed live

#### Scenario: Rejected caller

- **WHEN** a webhook fails signature verification
- **THEN** the payload is not persisted (SPEC-0001 unchanged) and the received lane surfaces the
  redacted rejection transiently

### Requirement: Todos View And Drawer

The todos view SHALL present the durable queue as a filterable table (line, provider, trust, state,
age) with the detail drawer (lifecycle timeline, idempotency key, lease, claimed-by, payload,
requeue/cancel actions) restyled in the new language, preserving all current actions and SSE row
updates.

#### Scenario: Live claim

- **WHEN** an agent claims a todo while the view is open
- **THEN** the row's state chip updates in place without reload

### Requirement: Endpoints View And Vend Wizard

The endpoints view SHALL render vended endpoints as cards (principal, persona, queues, verbs, MCP
URL, credential tail with hashed/last-used, expiry countdown, rotate/revoke). Vending SHALL become
a full-page wizard: persona → queues → verbs → **credential lifetime** → vend, ending in the
one-time credential reveal with copyable `.mcp.json`. For OAuth-capable clients the reveal SHALL
offer the URL-only variant (SPEC-0016). Scope stays immutable; the wizard SHALL make re-vend the
path for scope changes (SPEC-0007 doctrine).

#### Scenario: Lifetime chosen at vend

- **WHEN** the operator selects a 7-day lifetime and completes the wizard
- **THEN** the endpoint records its expiry, the card shows the countdown, and the reaper enforces
  it (SPEC-0016 REQ "Credential Lifetime")

### Requirement: Personas View And Wizard

The personas view SHALL render persona cards (initials block, name, human-authored prompt, verb
subset, derived skills, vended-as usage). Creating/editing SHALL be a wizard whose final step shows
a **live preview of the A2A Agent Card** rendered from unsaved form state, so the operator sees
exactly what peers will discover before saving.

#### Scenario: Preview before publish

- **WHEN** the operator edits the verb subset in the wizard
- **THEN** the previewed card's advertised skills update to the derived set before anything is
  persisted

### Requirement: Friends View And Approval Flow

The friends view SHALL separate *pending · in your queue* from *established* edges (direction,
peer, queues, last-seen, revoke). The approval flow SHALL present "approving **is** the vend" —
showing the scoped endpoint that approval mints — and on approval SHALL surface the vended result
explicitly rather than only a toast. Per-direction, revocable, non-transitive semantics (SPEC-0010)
are unchanged.

#### Scenario: Approve mints and shows the grant

- **WHEN** the operator approves a pending friend request
- **THEN** the flow completes with the vended endpoint's identity and scope visible, and the edge
  moves to established

### Requirement: Global Keyboard Map

The UI SHALL implement a global keymap — `g` then a view key (go to view), `/` (focus filter),
`enter` (open selection), `t` (theme) — declared in one registry that both drives behavior and
renders the key-hint footer, so hints never drift from bindings. Bindings SHALL never shadow text
inputs.

#### Scenario: Hints match behavior

- **WHEN** a key binding changes in the registry
- **THEN** the footer hint changes with it, with no second source of truth

### Requirement: Wizard Interaction Pattern

Create/vend/connect flows SHALL be full pages (not overlay modals) with server-side step state,
back navigation that preserves entered values, and a no-JS fallback that completes the same flow.
Destructive and irreversible steps (vend, revoke) SHALL confirm before executing.

#### Scenario: JavaScript disabled

- **WHEN** an operator completes the vend wizard with JS disabled
- **THEN** every step renders server-side and the flow completes identically

### Requirement: Live Fragment Architecture

SSE live updates SHALL continue through the existing hub with per-view fragment template files
(splitting the shared fragments file), and lane movement SHALL be expressed as typed events with
out-of-band removal + insertion. The SSE hub and auth (SPEC-0012) are unchanged.

#### Scenario: Fragment isolation

- **WHEN** a todos fragment changes
- **THEN** no board or friends template file is touched
