---
status: accepted
date: 2026-07-18
decision-makers: Joe Stump
supersedes: [ADR-0016]
extends: [ADR-0001]
governs: [SPEC-0015]
---

# ADR-0018: Adopt the Charm-Web Design Language (Day/Night, Monospace-First, Keyboard-First); Retire the Operator/Brass System

## Context and Problem Statement

The 2026-07 Claude Design redesign package ("Redesign with Bubbletea TUI") rebuilt the operator
board in a Charm/Bubbletea-inspired **charm-web** dialect: monospace-first typography (JetBrains
Mono workhorse, Space Mono display), a **day theme** (lavender-paper, deepened neon accents) and a
**night theme** (blue-black void, ANSI neon, Tron grid), terminal-style key-hint footers, and
keyboard-first navigation. It also upgraded the interaction language: single-form modals become
**full-page multi-step wizards**, and the board becomes a three-lane live patch panel. Nobody uses
switchboard yet; there is no migration constraint. Which design language is canonical, and what
carries over from ADR-0016's implementation?

## Decision Drivers

* The redesign package is the product decision — screens exist for every view plus the wizards and
  the MCP OAuth consent surface, in both themes.
* ADR-0016's *architecture* reasoning still holds and was validated in practice: hand-rolled token
  CSS (no framework), vendored OFL fonts as woff2 (no CDN; same-origin CSP), inline SVG, HTMX + SSE,
  `embed.FS`. Only the *language on top* changes.
* Trust modes (`signed`/`token`/`open`/`queue`) and todo states remain first-class UI vocabulary and
  need stable tokens in **both** themes, contrast-gated.
* The audience is operators of AI agents; the surface should read as a calm, technical instrument —
  the charm-web dialect (dense mono, chips, key hints) matches how the product is actually driven
  (keyboard, terminals, agents).

## Considered Options

* **(A) Keep the Operator/brass language** (ADR-0016 status quo).
* **(B) Adopt the charm-web language wholesale** — new token set (day+night), new fonts, new
  component vocabulary, keyboard-first interaction, wizard flow pattern; keep the CSS/token
  *architecture* and template/HTMX/SSE stack unchanged.
* **(C) Hybrid** — keep brass day mode, add charm night mode.

## Decision Outcome

Chosen option: **"(B) charm-web wholesale."** ADR-0016 is superseded as the design language; its
architectural spine is retained:

* **Tokens** — `static/tokens.css` is replaced with the charm-web token set: night (`:root` default
  or day default — SPEC-0015 fixes the choice), the other theme under `[data-theme]`, honoring
  `prefers-color-scheme`, with the existing `<html data-theme>` override contract preserved. The
  WCAG-AA contrast test (`tokens_contrast_test.go`) is **ported to the new palette pairs, not
  deleted** — it is the only automated gate on both themes.
* **Typography** — **JetBrains Mono** (body/UI/code) and **Space Mono** (display/wordmark), both
  OFL, vendored as woff2 under `static/fonts/` and embedded; the Zilla Slab / IBM Plex families are
  removed. `fonts_test.go` retargets.
* **Theme toggle** — a visible control (persisted to `localStorage`, keyboard `t`) with a no-flash
  boot: the pre-paint theme set must satisfy the same-origin CSP (`script-src 'self'`) via a tiny
  external script loaded before first paint — no inline scripts, no CSP loosening beyond a hash if
  unavoidable.
* **Interaction language** — keyboard-first: a global keymap (`g` go-to-view, `/` filter, `enter`
  open, `t` theme) rendered as the key-hint footer on every view; **full-page wizards** with
  server-side step state replace overlay modals for create/vend/connect flows (no-JS fallback
  discipline is retained from SPEC-0012).
* **Component vocabulary** — flat chips for trust/state (square, cell-honest), box-drawing-adjacent
  borders, glow reserved for surface chrome, stat tiles, lane cards. The `sb-*` class namespace and
  `data-sb-*` behavior-attribute convention survive; the component CSS is rewritten.
* **Docs site** — `docs-site/` stops sharing the app's `tokens.css`; the published design record
  gets a frozen copy so app-token churn cannot silently restyle the docs (decoupling over
  co-evolution).
* The Operator "hero journey" package under `docs/design/` is replaced by the charm-web design
  exploration as the canonical reference.

### Consequences

* Good: one coherent language across app, consent screens, and wizards; night parity for terminal
  dwellers; the token/test architecture carries over so the swap is mechanical where it matters.
* Bad: every template, both stylesheets, the icon set, and ~4.3k LOC of DOM-coupled tests rewrite;
  `// Governing: SPEC-0013` comments across `internal/web` go stale until SPEC-0015 lands with the
  code.
* Neutral: HTMX, SSE, `embed.FS`, CSRF, error-handling standards are untouched (ADR-0001 spine).
