---
title: Components
---

# Components

The `.sb-*` component inventory implemented in `static/switchboard.css`, consuming the
[design-language tokens](./02-design-language.md). Every component exists in the full build-out
canvas; nothing here is speculative.

## Chrome

- **Top bar** — 54–58px, raised gradient surface, bottom border. Left: mark + Zilla Slab wordmark +
  mono context label (`operator board`). Right: **LIVE pill** (green pill, pulsing dot,
  `LIVE · {rate}/min` in mono) and the **avatar circle** (oxblood, initials in mono).
- **Nav rail** — 196px (collapses to 60px icons-only below 820px). Items: square color dot + label;
  active item gets tinted background `#E9DABA`, ink text, brass dot; inactive items muted with sand
  dots. Right-aligned mono counts (todo count) and a count badge pill for pending friend requests.
  Footer: connectivity dot + `postgres · connected` in mono.

## Badges & pills

- **Trust badge** — mono 9.5px UPPERCASE, 999px pill, trust-mode bg/text pair (`signed` / `token` /
  `open` / `queue`). Always carries text; color is never the only signal.
- **Provider tag** — mono 11px two-letter tag (GH/ST/SL/DH/HL/RD) in a 6px-radius chip using the
  row's trust colors.
- **Status dot + label** — 7px dot in status color beside mono label (pending/claimed/done/failed);
  optional second line: `⧗ 23s lease` or `↻ retry 18s`.
- **Filter pill** — mono 11.5px `Label · count` pill; active = oxblood bg, cream text; inactive =
  panel bg, secondary text, sand border.
- **Dedup badge** — tiny violet pill `dedup ×N`.
- **Published/draft pill, endpoint status pill, friendship status pill** — same badge grammar with
  their own token pairs.

## Cards & tiles

- **Stat tile** — panel card, mono UPPERCASE micro-label, Zilla Slab numeral (24–27px), mono unit
  caption. The awaiting-claim tile tints red (`#F3E2DB`) when count > 0.
- **Throughput tile** — stat tile plus a 24-bucket bar chart (3px gap, 2px top radii, brass bars,
  current bar oxblood) with mono axis captions.
- **Trust-distribution tile** — stacked 10px segment bar (2px gaps) + mono legend with square
  swatches.
- **Endpoint card** — initials avatar (10px-radius square), name + `persona · <name>`, status pill,
  two chip groups under mono micro-labels (Scoped queues, Allowed verbs), bordered footer with
  masked cred + last-seen, action row (Edit / Revoke or `endpoint killed · <when>`); revoked cards
  at 60% opacity.
- **Persona card** — Zilla Slab name, `agent · <name>` mono, published pill, *italic quoted prompt*
  with 2px brass left border, verb chips under "Advertised skills · derived from vended verbs",
  footer with well-known URL + Edit + Publish/Unpublish.
- **Friend card** — local-agent chip ⇄/→/← remote agent + handle/board, status pill, optional
  italic note (incoming), intents chips or `none negotiated`, meta line (calls · last seen /
  requested Nago), status-appropriate action buttons.

## Feed & table

- **Feed row (Board)** — panel row: optional ringing pulse dot, provider tag, source name over mono
  event type, trust badge, right-aligned stage (dot + mono label) or `todo` + **Claim** button, mono
  relative age. Enters with fade + slide.
- **Queue table (Todos)** — panel table with header strip (mono UPPERCASE 9.5px column labels), row
  hover/selected tint `#F1E6CC`, re-surfaced flash `#F3E9CF`, per-row action button styled by state
  (brass-bordered Claim, green Complete, red-tinted Retry).

## Buttons

| Kind | Style |
|------|-------|
| Primary | oxblood bg, cream text, 9px radius (`+ Vend endpoint`, `Claim under lease`, `Done`) |
| Success | green `#235C33` bg (`Approve`, `Complete · ack`) |
| Secondary | transparent bg, sand border, secondary text (`Cancel`, `Decline`, `Release`) |
| Danger-outline | transparent bg, red-tinted border `#C99`, oxblood text (`Revoke`, `Fail`, `Delete`) |
| Mono action | mono 11px, 7px radius, state-tinted bg/border (table actions, drawer buttons) |
| Disabled primary | `#B9998F` bg, not-allowed cursor |

## Forms

- **Field** — mono UPPERCASE micro-label above input; input bg `#FBF6EA`, border `#D9C48F`, 8px
  radius, 13px mono (identifiers) or Plex Sans (prose); helper line in faint mono, live previews in
  oxblood (`agent card at /.well-known/agent-card/incident-triager`).
- **Chip picker** — toggle chips; selected = tinted bg + colored border (brass family for
  queues/agents, green family for verbs/intents); unselected = input bg, muted text.
- **Toggle switch** — 38×22px track (green when on), 18px cream knob, label + mono hint beside it
  (`Published — agent card discoverable at the well-known URL`).
- **Search input** — mono placeholder `search id · source · event`, 230px.

## Overlays

- **Drawer** — right-anchored, 440px (≤92vw), raised surface, left border + long shadow, fadeUp
  entrance; header (id + status pill + × close), scrollable body, pinned footer action row. Focus
  trapped; Escape closes.
- **Modal** — centered, 520–540px, 14px radius, scrim `rgba(36,28,21,.4)`, fadeUp; header with
  Zilla Slab title + × close; footer button row (destructive action pinned left in edit modals).
- **Lease card** (in drawer) — token-amber panel: `Visibility lease · <agent>` micro-label,
  `{n}s left` mono bold, 6px progress bar (brass on `#E3D2A6`), reaper explainer line, Extend/Release.
- **Callout** — tinted panel with border for warnings/info: red-tinted for "credential shown once",
  amber for "pending until the remote operator approves".
- **Toast** — bottom-center, dark `#241C15` bg, cream mono 12px text, 10px radius, fadeUp, ~2.6s
  TTL. Announced via `aria-live`.

## Accessibility contract

Every component above ships with: text alongside color, `aria-label` on icon-only controls,
`aria-live` on dynamic regions, visible focus rings, focus trap + Escape + focus-return on
drawer/modals, and AA contrast in both themes ([SPEC-0013](../openspec/specs/operator-board/spec.md)
§ Accessibility Requirements).
