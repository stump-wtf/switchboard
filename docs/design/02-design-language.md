---
title: Design language
---

# Design language

The canonical token set for the "Operator" identity
([ADR-0016](../adrs/ADR-0016-operator-design-language.md)). The light **operator-cream** theme is
the default and matches the approved designs; the dark **bakelite** theme derives from the same
material palette. All values live in `static/tokens.css`.

## The mark

Three brass-outlined jack rings joined by a copper patch-cord arc — a switchboard patch in progress:

<svg width="56" height="56" viewBox="0 0 28 28" fill="none" role="img" aria-label="Switchboard mark">
  <path d="M8 11 C 5.3 13.2, 6.1 16.4, 9.6 17.4" stroke="#C2603A" stroke-width="2.3" stroke-linecap="round"/>
  <circle cx="8" cy="8.4" r="2.7" stroke="#C79A45" stroke-width="1.8"/>
  <circle cx="9.8" cy="19.7" r="2.7" stroke="#C79A45" stroke-width="1.8"/>
  <circle cx="20.2" cy="9" r="2.7" stroke="#C79A45" stroke-width="1.8"/>
</svg>

Rings in brass `#C79A45` (1.8 stroke), cord in copper `#C2603A` (2.3 stroke, round caps). Renders at
27–28px in chrome; always inline SVG via `currentColor`-compatible markup, never a raster.

## Typography

| Role | Face | Weights | Usage |
|------|------|---------|-------|
| Display / headings | **Zilla Slab** (serif) | 500 · 600 · 700 | Page titles (26px), card titles (17px), modal titles (18px), stat numerals (24–27px) |
| Body / UI | **IBM Plex Sans** | 400 · 500 · 600 | Body copy and controls, 13–14.5px |
| Technical / meta | **IBM Plex Mono** | 400 · 500 · 600 | ids, credentials, taglines, table meta; micro-labels 9.5–10.5px UPPERCASE with 0.08–0.16em tracking |

All faces are vendored woff2 under `static/fonts/` (OFL) — never loaded from a CDN.

## Palette — operator-cream (light, default)

### Surfaces & ink

| Token | Value | Role |
|-------|-------|------|
| canvas | `#EFE6D2` | App background (parchment) |
| panel | `#FAF4E6` | Cards, tables, feed rows |
| raised | `#F6EFDE → #F0E7D3` | Top bar gradient, drawer/modal surface |
| header strip | `#F0E4CC` | Table header rows |
| input | `#FBF6EA` | Form fields |
| code | `#EFE1C2` | Payload/key blocks, code chips |
| border | `#E7DAC0` / `#E1D3B6` | Card and chrome borders |
| divider | `#EDE1C6` | Row dividers |
| input border | `#D9C48F` / `#E0CFA9` | Field and pill outlines |
| ink | `#241C15` | Near-black chrome text |
| text | `#2B2118` | Primary text |
| body | `#4A4237` | Long-form body |
| secondary | `#6B6152` | Secondary text |
| muted | `#8A7C64` | Metadata |
| faint | `#A2937A` / `#A89A80` / `#B3A488` | Micro-labels, timestamps, axis text |

### Brand accents

| Token | Value | Role |
|-------|-------|------|
| oxblood | `#7A2B24` | Primary actions, links, ids, active filter pills |
| oxblood-hover | `#9A3B2E` | Link/button hover |
| oxblood-disabled | `#B9998F` | Disabled primary |
| on-oxblood | `#F3E7CF` | Text on oxblood |
| brass | `#B0863B` | Active accents, selected chip borders, lease bars |
| brass-bright | `#C9A24B` / `#C79A45` | Logo, emphasis |
| brass-chart | `#CBAE68` | Chart bars (current bar: oxblood) |
| ring | `#C9902A` | Ringing/verifying pulse dots |
| copper | `#C2603A` | Logo cord |

### Trust modes ([ADR-0003](../adrs/ADR-0003-per-provider-ingestion-and-trust-model.md))

| Trust | Text | Background | Dot |
|-------|------|------------|-----|
| `signed` | `#235C33` | `#DCE9D9` | `#2F9E44` |
| `token` | `#7A5310` | `#F0E4C6` | `#C9902A` |
| `open` | `#7A2B24` | `#EEDAD3` | `#B0554B` |
| `queue` | `#1F5570` | `#D7E5EC` | `#3A7CA5` |

### Todo status

| Status | Color | Badge bg |
|--------|-------|----------|
| pending | `#1F5570` | `#D7E5EC` |
| claimed | `#7A5310` | `#F0E4C6` |
| done | `#235C33` | `#DCE9D9` |
| failed | `#7A2B24` | `#EEDAD3` |
| neutral/done-stage | `#6B6455` | — |

### Chips & special

| Token | Value | Role |
|-------|-------|------|
| verb chip | `#3B5A3E` on `#E4EADD` (selected border `#9BBF9E`) | Allowed-verb / skill / intent chips |
| queue chip | `#7A2B24` on `#EFE1C2` · `#6B6152` on `#EDE1C6` (selected border `#B0863B`) | Scoped-queue chips, agent badges |
| dedup badge | `#5A4A8A` on `#E7DFF0` | `dedup ×N` idempotency badge |
| live | `#2F9E44` on `#DCE9D9` | LIVE pill, connectivity dot |
| toast | `#F3E7CF` on `#241C15` | Toast notifications |
| scrim | `rgba(36,28,21,.32–.4)` | Drawer/modal overlay |

## Palette — bakelite (dark)

The dark theme keeps the raw-material tokens already established: bakelite `#1a1411`, walnut
`#241c17` (raised `#2f251d`), brass `#c99b4d` (bright `#e3ba6a`, dim `#9a7734`), cream text
`#ece0c6` (muted `#a99a7c`), parchment `#d8c8a1`, oxblood `#b1533f` (deep `#8f3a2c`), amber
`#d99a3c`, patch-green `#6f9a5f`, patch-blue `#6d8a99`, copper `#a86b3c`. Trust/status roles map
onto the same names as light; exact dark values are pinned in `tokens.css` and must hold WCAG 2.1
AA contrast.

## Shape

| Element | Radius |
|---------|--------|
| Cards, tables, feed rows | 10–12px |
| Modals | 14px |
| Buttons | 7–9px |
| Chips | 5–8px |
| Pills, badges | 999px |

## Elevation

Soft, long-throw, warm-tinted shadows (`rgba(20,15,8,…)`):

| Surface | Shadow |
|---------|--------|
| Modal | `0 30px 70px -20px rgba(20,15,8,.6)` |
| Drawer | `-16px 0 40px -18px rgba(20,15,8,.5)` |
| Toast | `0 12px 30px -10px rgba(20,15,8,.6)` |

## Motion

| Animation | Timing | Use |
|-----------|--------|-----|
| `ringPulse` | 1–1.4s ease-in-out ∞ (opacity 1→.3, scale 1→.75) | Verifying dots, lease-reaper indicator |
| `livePulse` | 1.4s ease-in-out ∞ (opacity 1→.4, scale 1→.7) | LIVE dot |
| `fadeUp` | .25s ease (8px rise + fade) | Modal, drawer, toast entrance |
| row intro | .5s ease (fade + 10px slide) | New feed/table rows |
| progress | .5s ease width transition | Lease countdown bars |

Motion is informational, never decorative: pulses mean "in flight", the fade-up means "transient
surface". Respect `prefers-reduced-motion` by disabling the infinite pulses.
