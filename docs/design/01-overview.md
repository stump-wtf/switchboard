---
title: Overview
---

# Design · Overview

Switchboard's visual identity is the **"Operator" design language** — brass & bakelite,
switchboard-era telephony — adopted in
[ADR-0016](../adrs/ADR-0016-operator-design-language.md) and realized by the operator board
([SPEC-0013](../openspec/specs/operator-board/spec.md)). It originates from the high-fidelity
Claude Design *hero journey* package, which explored two directions and built the chosen one out
into a complete five-view application.

## Where the language came from

The design package (`Switchboard hero journey design.zip`) contains two canvases:

| Canvas | Contents |
|--------|----------|
| **Hero Board** (Turn 1) | Two competing directions of the live board: **1a "Operator"** — warm parchment, brass, oxblood, Zilla Slab — and **1b "Console"** — dark dev-tool, Space Grotesk, amber. See [Directions](./06-directions.md). |
| **Switchboard** (full build-out) | Direction 1a taken to a complete product: Board, Todos, Endpoints, Personas, and Friends views, the todo detail drawer, the vend/persona/friend modals, and toast notifications. See [Screens](./04-screens.md). |

Direction **1a "Operator"** won. It extends the identity the project has carried since
[ADR-0000](../adrs/ADR-0000-project-naming-and-scope.md): a manual telephone exchange took many
incoming lines, an operator verified each caller and patched the line through — hence brass jacks,
bakelite, operator-cream, oxblood, and patch-cable tones.

## What lives in this section

- **[Design language](./02-design-language.md)** — the canonical tokens: palette, typography,
  shape, elevation, and motion, for both the operator-cream (light) and bakelite (dark) themes.
- **[Components](./03-components.md)** — the `.sb-*` component inventory: badges, pills, cards,
  stat tiles, feed rows, chip pickers, drawers, modals, and toasts.
- **[Screens](./04-screens.md)** — the five operator-board views and their interaction patterns.
- **[Voice](./05-voice.md)** — microcopy rules: the operator metaphor, mono taglines, verbs.
- **[Directions](./06-directions.md)** — the 1a/1b exploration record, including the road not taken.

## How it's implemented

The language ships as two owned stylesheets plus vendored fonts, embedded in the binary and shared
with this docs site (no CDN, strict same-origin CSP):

| Artifact | Role |
|----------|------|
| `static/tokens.css` | Design tokens only — custom properties for both themes |
| `static/switchboard.css` | The `.sb-*` component classes consuming those tokens |
| `static/fonts/` | Zilla Slab, IBM Plex Sans, IBM Plex Mono (woff2, OFL) |

Governing artifacts: [ADR-0016](../adrs/ADR-0016-operator-design-language.md) (the decision),
[SPEC-0013](../openspec/specs/operator-board/spec.md) (the operator board that realizes it),
[ADR-0001](../adrs/ADR-0001-web-stack-go-htmx-pico.md) (the web stack it rides on), and
[ADR-0003](../adrs/ADR-0003-per-provider-ingestion-and-trust-model.md) (the trust vocabulary the
badge tokens encode).
