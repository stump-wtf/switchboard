# Charm-Web Component Vocabulary

Governing: [ADR-0018](../adrs/ADR-0018-charm-web-design-language.md),
[SPEC-0015](../openspec/specs/operator-board-v2/spec.md). Implemented as the `.sb-*` layer in
`static/switchboard.css`; behavior hooks are `data-sb-*` attributes only (never classes).

## Shell chrome

- **Top bar** — mark + lowercase `switchboard` wordmark (Space Mono) · `~/operator` breadcrumb ·
  MCP-connected indicator (`data-sb-mcp`, flat highlight bar) · LIVE throughput pill · `+ new`
  launcher (`data-sb-new`, purple, glowing) · theme control (`data-sb-theme-toggle`).
- **Nav** — the six views as a horizontal lowercase mono strip (`.sb-rail`), active view
  underlined in pink; `data-sb-nav` stamps feed the `g` go-to-view chord. Collapses to dots below
  820px. The postgres connectivity line lives at its right edge.
- **Key-hint footer** — every view ends in the terminal help line (`#sb-keys`): dim
  `key action • key action` pairs rendered by `sb-keys.js` from the one keymap registry.

## Chips and badges

Flat, square-cornered (3px), cell-honest — colored cells like Gum/Lip Gloss actually render:

- **Trust badges** — `sb-badge--signed|token|open|queue`: trust-token text on tinted background.
- **State chips** — `sb-status--pending|claimed|done|failed` with a leading `●` dot.
- **Scope chips** — `sb-chip--queue` (cyan family) and `sb-chip--verb` (mint family); toggle
  variants are real checkboxes styled as chips.
- **Provider tags** — two-letter mono tiles on feed rows and cards:

| Source | Tag |
|---|---|
| github | GH |
| stripe | ST |
| slack | SL |
| dockerhub | DH |
| healthchecks | HL |
| redis | RD |
| *(unknown)* | first two letters, upper-cased |

## Cards, tables, tiles

- **Cards** (`sb-card`, `sb-epcard`, `sb-fcard`, persona cards) — `--sb-panel` fill, 1px border,
  8px radius; revoked/blocked cards render dimmed.
- **Tables** (`sb-table`) — dense mono rows; the whole todo row opens the drawer
  (`data-sb-row-open` forwards to the accessible `data-sb-row-trigger`).
- **Stat tiles** (`sb-tiles`) — big mono numbers with microlabel sublabels; the throughput tile
  carries per-minute activity bars.

## Live regions

The SSE contract is unchanged (SPEC-0013): stable ids (`sb-feed`, `sb-tiles`, `sb-live`,
`sb-todo-count`, `sb-tc-*`, `sb-tr-*`, `sb-ep-seen-*`) are OOB swap targets, present in the DOM
before the first frame. Countdowns tick toward server-stamped `data-sb-deadline` values.

## Presentation JS

`sb.js` is split into feature modules under `static/js/` (all deferred IIFEs, CSP-clean):
`sb-live.js` (toasts, feed trim, countdowns, LIVE decay), `sb-overlay.js` (drawer/modal focus
machinery), `sb-vend.js` (vend scope gating), `sb-theme.js` (theme cycle + persistence),
`sb-keys.js` (keymap registry + footer hints), plus the synchronous pre-paint `theme-boot.js`.
