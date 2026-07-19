# Directions Explored

Governing: [ADR-0018](../adrs/ADR-0018-charm-web-design-language.md) (Considered Options).

## Chosen: charm-web wholesale (B)

The 2026-07 "Redesign with Bubbletea TUI" exploration produced screens for every view plus the
wizards and the MCP OAuth consent surface, in both themes — a complete product decision, adopted
wholesale (ADR-0018). Day mode is the default web posture; night is the terminal-dweller parity
theme.

## Explored and rejected

- **(A) Keep the Operator/brass language** — the ADR-0016 system (parchment, brass, oxblood;
  Zilla Slab display over IBM Plex). Rejected: the charm-web package is the product decision, and
  nobody uses switchboard yet — there is no migration constraint to honor. Its architecture
  (tokens, contrast gate, embed.FS, HTMX/SSE) survives; its skin does not. The published design
  record keeps a frozen copy of the brass tokens (`docs-site/static/design-tokens/`) so the docs
  site stays stable while the app moves.
- **(C) Hybrid: brass day + charm night** — rejected as two half-languages; trust/state colors
  and component shapes would fork per theme, doubling every design decision.
- **Terminal-window chrome around the app** — the TUI dialect of the design system frames
  everything in emulator chrome. Rejected for the app: on a web surface, a framed window means a
  literal embedded terminal. The browser dialect is borderless; content sits on the grid canvas.
- **Icon font / SVG icon set** — rejected; the language uses typed Unicode glyphs, which render
  in the same mono faces the text uses and need no extra assets.
- **Overlay modals (status quo interaction)** — replaced by full-page wizards with server-side
  step state (SPEC-0015 REQ "Wizard Interaction Pattern"); overlays remain only for the todo
  drawer and transient toasts.
