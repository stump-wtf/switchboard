# Charm-Web Design Language

Governing: [ADR-0018](../adrs/ADR-0018-charm-web-design-language.md),
[SPEC-0015](../openspec/specs/operator-board-v2/spec.md) REQ "Design Token System",
REQ "Typography And Vendored Fonts".

## Themes

Two complete themes over one semantic token vocabulary (`--sb-*`, in `static/tokens.css`):

- **day (default)** — lavender-paper surfaces (`#EEEDFA` canvas → `#FDFDFF` raised), dark-indigo
  ink, the brand hues **deepened** for AA contrast: purple `#6C46EA`, pink `#E43A8C`, cyan
  `#0793B4`, mint `#059669`. Neon glows soften into colored shadows.
- **night** — blue-black void (`#08080F` canvas → `#1E1E38` raised), phosphor text (`#F4F4FF` →
  dim indigo-grey), ANSI neon: charm purple `#7D56F4`, hot pink `#FF5FA2`, cyan `#4EE6FF`, mint
  `#00F0A8`. Elevation is expressed with **glow**, not blur-shadow.

Theme selection: day at `:root`; night via `@media (prefers-color-scheme: dark)` on
`:root:not([data-theme="day"])` and always via `[data-theme="night"]`. The pre-paint boot script
(`static/js/theme-boot.js`, external — CSP `script-src 'self'` holds) stamps the stored choice
(`localStorage["sb-theme"]`) before first paint; the visible control and the `t` key cycle it.

Both canvases carry the signature **grid floor**: a faint 28px cell grid (`--sb-grid`) under
everything — Tron at night, pencil-on-paper by day.

## Trust and state colors

Trust modes (`signed · token · open · queue`, ADR-0003) and todo states
(`pending · claimed · done · failed`) are first-class named tokens in **both** themes
(`--sb-trust-*`, `--sb-status-*`), each a text/tinted-background pair held to **WCAG 2.1 AA
(≥ 4.5:1)** by `tokens_contrast_test.go` — the automated gate on every token edit.

| Role | Day | Night |
|---|---|---|
| signed / done | deep mint on mint tint | neon mint on dark mint |
| token / claimed | deep gold on gold tint | neon gold on dark gold |
| open / failed | deep coral on coral tint | neon coral on dark coral |
| queue / pending | deep cyan on cyan tint | neon cyan on dark cyan |

## Typography

Monospace-first — hierarchy comes from weight, size, and color, never from a proportional face:

- **JetBrains Mono** — body, UI, code. Weights 400/500/700.
- **Space Mono** — display: the wordmark and view titles (tight tracking, bold).

Both are SIL OFL, vendored as woff2 under `static/fonts/` and embedded — no font CDN
(`static/fonts/README.md` lists the exact files). The retired Zilla Slab / IBM Plex families are
gone. View titles render lowercase (`endpoints`, not `Endpoints`).

## Iconography

Terminal UIs use glyphs, not icon sets: status `✓ ✗ ● ○ ◆ •`, arrows `↑ ↓ ← →`, prompts `❯ $`,
progress blocks `█ ▓ ▒ ░`, box drawing `─ │ ╭ ╮ ╰ ╯`. Type the glyph in the mono face; do not
substitute a web icon font. The one inline-SVG exception is the switchboard mark (pink jacks,
cyan patch cord) in the top bar.

## Shape, elevation, motion

- **Radii** — soft-square: chips 3px (flat highlight bars, no pills for state), buttons 5px,
  panels 8px, modals 12px. `--sb-radius-pill` survives only for the LIVE throughput pill.
- **Elevation** — `--sb-glow-*` neon bloom on chrome and primary actions; real drop shadows only
  on overlays (modal/drawer/toast).
- **Motion** — quick and springy: `--sb-ease` (ease-out), `--sb-ease-spring` for button lifts,
  120–340ms. Never `transition: all`. Everything respects `prefers-reduced-motion`.
