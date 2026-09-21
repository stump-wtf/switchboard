# Charm-Web Screens

Governing: [SPEC-0015](../openspec/specs/operator-board-v2/spec.md) (view IA),
[SPEC-0016](../openspec/specs/mcp-oauth/spec.md) (consent surface).

Five views — **board · todos · endpoints · personas · friends** — under one shell
(top bar, horizontal nav, key-hint footer), plus login and the full-page wizards.

## board — the live patch panel

Three lanes as the live view of every inbound line: **received** (ephemeral in-flight cards,
SSE-only), **verified** (durable todos awaiting claim), **patched through** (claimed/completed
under an agent). Cards carry provider glyph, title, trust chip, state chip, age; lane headers
carry live counts; cards move between lanes over SSE without reload.

## todos — the durable queue

Filterable table (line, provider, trust, state, age) with live row updates and the detail drawer:
lifecycle timeline, idempotency key + dedup line, lease card with draining countdown, payload,
requeue/cancel actions.

## endpoints — vended capabilities

Cards per vended endpoint: principal, persona, queue/verb scope chips, MCP URL, credential tail
(hashed · last-used), expiry countdown, rotate/revoke. Vending is a full-page wizard
(persona → queues → verbs → credential lifetime → vend) ending in the one-time reveal with
copyable `.mcp.json` — and the URL-only variant for OAuth-capable clients.

## personas — least-privilege faces

Persona cards (initials block, name, prompt, verb subset, derived skills, vended-as usage). The
create/edit wizard's final step live-previews the A2A Agent Card from unsaved form state.

## friends — the A2A ledger

*pending · in your queue* separated from *established* edges (direction, peer, queues, last-seen,
revoke). The approval flow presents "approving **is** the vend" and surfaces the minted endpoint
explicitly.

## login

Same language, public: wordmark, tagline, a single glowing primary action (Pocket ID), the
dev-login fallback, and the theme control.

## Wizards (interaction pattern)

Create/vend flows are full pages, not overlays: server-side step state, back navigation
preserving entered values, a no-JS fallback completing the same flow, and explicit confirmation
before destructive steps (vend, revoke).
