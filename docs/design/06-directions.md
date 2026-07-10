---
title: Directions explored
---

# Directions explored

Turn 1 of the design package rendered the same live Board twice — identical data, two identities —
and asked for a pick. This page records both so the choice stays legible
([ADR-0016](../adrs/ADR-0016-operator-design-language.md) § Considered Options).

## 1a · Operator — brass & bakelite *(chosen)*

Warm, tactile, switchboard-era. Parchment canvas `#EFE6D2`, cream panels, brass accents, oxblood
primaries, Zilla Slab + IBM Plex. Trust reads as deep naturalistic tints (green/amber/red/blue on
cream). The full five-view build-out, the [design language](./02-design-language.md), and the
[component inventory](./03-components.md) all descend from this direction.

Why it won:

- It **is** the product story — the operator metaphor ([ADR-0000](../adrs/ADR-0000-project-naming-and-scope.md))
  made visible; the palette had already seeded `static/tokens.css` and the docs site.
- Warmth differentiates: infrastructure dashboards default to dark-console; a verified, human-owned
  queue reads better as a calm, lit room.
- The light surface holds AA contrast comfortably for the dense mono metadata the UI leans on.

## 1b · Console — dark dev-tool *(not chosen)*

Dense, technical, monospace-forward. Near-black surfaces (`#0E1116`, `#12161C`, `#161A21`), borders
`#222833`/`#262C36`, **Space Grotesk** headings, amber accent `#E7A94A`, glassy translucent badge
fills, 5–8px radii, left-border active nav.

| Role | Value |
|------|-------|
| Canvas / bar / panel | `#0E1116` / `#12161C` / `#161A21` |
| Text / muted | `#EEF1F6` · `#DDE2EA` / `#7C8494` · `#5C6470` |
| Accent (brand) | `#E7A94A` (amber) |
| signed | `#5CD79B` on `rgba(67,192,138,.14)` |
| token | `#ECB45A` on `rgba(224,167,59,.14)` |
| open | `#F0836B` on `rgba(229,103,78,.15)` |
| queue | `#6BB0E8` on `rgba(76,155,224,.15)` |
| live | `#43C08A` |

Why it lost:

- Generic at a glance — reads as "another dark ops console", surrendering the switchboard identity.
- The translucent badge fills sit close to the surface value; AA contrast needs constant tending.
- Choosing it would have discarded the committed bakelite/brass token layer rather than extending it.

What it contributed anyway: the **bakelite dark theme** keeps 1b's density lessons (tighter radii,
left-border active states are available to `.sb-*` dark variants), and 1b's trust hues informed the
dark-theme trust mapping. The direction remains available in the design archive if a
console-density mode is ever wanted.
