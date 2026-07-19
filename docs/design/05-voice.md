# Charm-Web Voice

Governing: [ADR-0018](../adrs/ADR-0018-charm-web-design-language.md). The voice is calm,
technical, a little playful — an instrument, never a dashboard selling itself.

## Rules

- **Lowercase by default.** View titles, nav entries, taglines, and microcopy are lowercase
  (`endpoints`, `~/operator`, `no lines in yet · the board is quiet`). Proper nouns and the
  start of full sentences keep their case.
- **The middot is the separator.** `many lines in · each verified · patched through`. Not
  slashes, not pipes, not em dashes between fragments.
- **Address the operator as "you"**; the system refers to itself implicitly.
- **Warnings say the consequence plainly.** No hedging: *"Copy the credential now — it is shown
  once. The endpoint is the capability; revoking kills it."* State what happens, then stop.
- **Empty states explain and point forward** — `no todos match · the queue is clear`, never a
  bare "No data".
- **Key hints are load-bearing, not decoration** — every view ends with the dim
  `key action • key action` help line, and the hints come from the real keymap registry.
- **Glyphs over emoji** inside the UI: `✓ ✗ ● → ↻`. Emoji never appear in dense UI.
- **Numbers are mono and bare** — `7/min`, `83%`, `5s left`. Units in the sublabel, not bolted to
  the number.

## Terminology (unchanged domain voice)

- Endpoints are **vended**, and revoking **kills** them — scope is immutable; re-vend to change it.
- Events are **verified** then **patched through** to the durable queue.
- The reaper **re-surfaces** abandoned work; leases **drain**.
- Friendships: approving **is** the vend.
