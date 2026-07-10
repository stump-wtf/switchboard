---
title: Screens
---

# Screens

The five operator-board views from the full build-out canvas, specified normatively in
[SPEC-0013](../openspec/specs/operator-board/spec.md). All views share the top bar + nav rail chrome
and the `/events` SSE live-update stream.

## 1 · Board (`/`)

*"The Board — many lines in · each verified · patched through"*

The landing view: a live operator's-eye picture of ingress.

- **Header row** — Zilla Slab title + mono tagline; right-aligned trust legend pills (● signed
  ● token ● open ● queue).
- **Stat band** — Throughput tile (events/min + 24-bucket activity chart, "last 36s → now"), and a
  stacked column of three tiles: **in flight · being worked**, **awaiting claim** (tints red when
  nonzero), **verified sources %**.
- **Incoming lines feed** — newest-first, ~8 rows visible. A row is born *ringing* (pulsing amber
  dot, `verifying…`), becomes `patched → todo` (claimable), then `claimed · <agent>`, then `done ✓`.
  Pending rows offer a **Claim** button.

## 2 · Todos (`/todos`)

*"Durable queue — claim under a lease · complete with an ack · dedup by idempotency key · at-least-once"*

The queue ledger, filterable and searchable.

- **Reaper pill** — `lease reaper active · re-surfaces abandoned work` with pulsing dot.
- **Filter pills** — All / Pending / Claimed / Done / Failed, each `Label · count`; search box
  (`search id · source · event`).
- **Table** — id · source·event (+ `dedup ×N`) · trust · status (+ lease/retry sub-line) · agent ·
  age · action. Contextual actions: Claim / Complete / Retry. Reaper re-surfaces flash the row and
  raise a toast (`td_8f2a · lease expired, re-surfaced to queue`).
- **Row click** opens the **detail drawer**:
  - claimed → lease card (countdown, progress bar, Extend / Release, reaper explainer);
  - failed → retry card (`retry with backoff · attempt N`, `↻ {n}s`);
  - metadata grid (trust / agent / attempts / age), idempotency key + dedup line, payload as
    escaped pretty JSON, lifecycle timeline (`received · verified at boundary` → `todo created ·
    durably stored` → `claimed under lease` → `completed · ack sent`, with failed variants);
  - footer: **Claim under lease** / **Complete · ack** + **Fail** / **Retry now** / `✓ completed ·
    acked to source`.

## 3 · Endpoints (`/endpoints`)

*"Vended MCP endpoints — humans are the accountable principals · each endpoint is a scoped,
revocable capability · revoke = kill the endpoint"*

- **Endpoint cards** — agent name + persona, status pill, scoped-queue chips, allowed-verb chips,
  masked cred + last-seen, Edit / Revoke. Revoked cards dim with `endpoint killed · <when>`.
- **+ Vend endpoint** modal — agent name, bound persona picker, queue chips, verb chips; submit
  disabled until name + ≥1 queue + ≥1 verb. On mint: success screen with the **MCP endpoint URL**
  (`https://<host>/mcp/<slug>`), the **credential shown once** (red callout: *"Copy the credential
  now — it is shown once. The endpoint is the capability; revoking kills it."*), and ready-to-paste
  HTTP `.mcp.json` wiring ([SPEC-0014](../openspec/specs/mcp-transport/spec.md)).

## 4 · Personas (`/personas`)

*"Personas · A2A Agent Cards — one agent, many least-privilege faces · a human-authored prompt plus
a verb subset · advertised as an A2A Agent Card"*

- **Persona cards** — name, backing agent, published/draft pill, italic quoted system prompt,
  "advertised skills" verb chips, well-known URL (`/.well-known/agent-card/<slug>`), Edit +
  Publish/Unpublish.
- **New/Edit modal** — name (live URL preview), backing-agent chips, prompt textarea, verb-subset
  chips (bounded by the vended verbs), Published toggle (hint: *"agent card discoverable at the
  well-known URL"* / *"not advertised · visible only here"*); Delete pinned left in edit mode.

Ships with [SPEC-0009](../openspec/specs/personas/spec.md); hidden from the rail until enabled.

## 5 · Friends (`/friends`)

*"Friends · agent-to-agent — agents discover each other over A2A · no one talks by default · links
are mutual and non-transitive — both operators must approve"*

- **View toggle** — Cards / Ledger; filter pills All / Incoming / Outgoing / Active / Blocked.
- **Cards** — `local-agent ⇄ remote-agent` (arrow direction encodes status), handle + board host,
  status pill, request note (incoming), intents chips (`status.query`, `todo.handoff`,
  `artifact.fetch`, `event.subscribe`) or `none negotiated`, meta line, actions:
  Approve/Decline · Withdraw · Revoke · Unblock.
- **Ledger** — grouped sections with dot headers (Incoming requests / Outgoing · pending / Active
  friendships / Blocked) and dense rows.
- **Add friend modal** — amber callout (*"The link stays pending until the remote operator approves
  it too. Non-transitive — your other friends' agents get nothing."*), local-agent chips, remote
  handle input (`@jordan/deploy-watcher` → live resolution `deploy-watcher @ jordan.sb.local`),
  intent chips, message textarea, **Send request →**.

Ships with [SPEC-0010](../openspec/specs/friending/spec.md); hidden from the rail until enabled.

## Cross-view behaviors

- **Live everywhere** — every count, row, stage, and tile updates over SSE; a reload always renders
  authoritative PostgreSQL state.
- **Toasts** — background transitions (agent claims/completes, reaper re-surfaces, vend/revoke,
  friendship changes) announce themselves bottom-center.
- **Overlays** — one drawer/modal at a time; scrim click, ×, and Escape close; focus is trapped and
  returned.
