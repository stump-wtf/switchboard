# Charm-Web Design Exploration — Overview

**Canonical design reference for the switchboard UI.**
Governing: [ADR-0018](../adrs/ADR-0018-charm-web-design-language.md) (charm-web design language),
[SPEC-0015](../openspec/specs/operator-board-v2/spec.md) (Operator Board v2).
Supersedes the Operator/brass "hero journey" package that previously lived in this directory
(ADR-0016, retired).

## What this is

The 2026-07 redesign rebuilt the operator board in a **charm-web** dialect — a browser-native
re-expression of the Charm/Bubbletea TUI visual language:

- **Monospace-first typography** — JetBrains Mono is the workhorse (body/UI/code), Space Mono the
  chunky display voice (wordmark, view titles). No non-mono families anywhere.
- **Two complete themes** — **day** (lavender paper, the same brand hues deepened for contrast) and
  **night** (blue-black void, ANSI neon, Tron grid). Day is the default; night follows
  `prefers-color-scheme` and the `<html data-theme>` override.
- **Keyboard-first interaction** — a global keymap (`g` go-to-view, `/` filter, `t` theme) rendered
  as a terminal-style key-hint footer on every view.
- **Full-page wizards** — create/vend/connect flows become multi-step pages with server-side step
  state, replacing overlay modals.
- **Cell-honest components** — flat square chips for trust/state, box-drawing-adjacent borders,
  glyph icons over icon fonts, glow reserved for surface chrome.

## What carried over from the Operator system (ADR-0016)

The *architecture* was validated and survives wholesale: hand-rolled token CSS (no framework),
vendored OFL fonts as woff2 (no CDN, same-origin CSP), inline SVG, HTMX + SSE, `embed.FS`, the
`sb-*` class namespace and `data-sb-*` behavior-attribute convention, and the WCAG-AA contrast
test gating both themes. Only the language on top changed.

## Files in this package

| File | Contents |
|---|---|
| `01-overview.md` | This overview |
| `02-design-language.md` | Tokens, themes, typography, iconography |
| `03-components.md` | Component vocabulary + provider tag table |
| `04-screens.md` | The six views, wizards, consent surface |
| `05-voice.md` | Copy voice and microcopy rules |
| `06-directions.md` | Directions explored and rejected |

## Implementation anchors

| Artifact | Role |
|---|---|
| `static/tokens.css` | Design tokens only — custom properties for both themes |
| `static/switchboard.css` | The `.sb-*` component layer |
| `static/js/` | Split presentation modules + the pre-paint theme boot |
| `static/fonts/` | Vendored woff2 (see its README for the required files) |
| `tokens_contrast_test.go` | The AA contrast gate over both themes |
