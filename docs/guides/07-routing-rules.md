---
title: Route events with jq rules
---

# Route events with jq rules

Every delivery to a webhook used to become a todo on that webhook's target queue — verbatim,
unfiltered. That stops working once producers are noisy: cairn announces **every** paste, a Gitea
org hook announces every CI run. **Routing rules** let the webhook's owner decide, per delivery,
*where* the todo lands — or whether there is a todo at all.

A rule is a jq filter plus an action. Rules run in order and **the first match wins**; anything
unmatched takes the webhook's **default action**. With no rules and no default, a webhook behaves
exactly as it always did.

```
verify → idempotency key → resolve targets → ROUTE (rules → default) → todo(s) | drop
```

## Actions

| Action | Effect |
|---|---|
| `{"queue": "forge"}` | Create the todo in `forge` on **every** delivery target (owner + routes). |
| `{"queue": "handoff", "endpoints": ["<id>"]}` | Create it only on those targets — a subset of the webhook's existing delivery targets. |
| `{"drop": true}` | Record the event (visible in history, dedup slot spent) but create no todo and ring no doorbell. |
| `{"queue": "lane-s", "exclusive": true}` | Create it on exactly **one** target: the first (owner first, then routes by grant time) whose endpoint scope includes the queue. |
| `{"queue": "lane-s", "once": true}` | At most once per subject (issue or cairn artifact) per queue: later deliveries about it are recorded with `"once": "repeat"` and answer `{"repeat": true}`. |
| `{"queue": "lane-s", "work_order": true}` | Attach a switchboard-authored, semi-trusted `work_order` (lane, verified provenance, authorizing rule, subject, authority) to each todo. |

The last three combine, and are only valid with `queue`. They are what handoff lanes use — see
[guide 08](08-handoff-lanes.md).

A rule can only **narrow** where a delivery lands, never widen it. The queue must be the webhook's
target queue or one of its owner's allowed webhook queues; every `endpoints` entry must already be a
delivery target — add it with `add_webhook_route` first, which carries its own ownership and
friendship checks. Both are re-checked on every delivery: if a route is revoked or the queue ceiling
shrinks after you saved a rule, a matching delivery takes the default instead (and the trace says
`rule_not_granted`).

A dropped delivery **stays dropped**: if the producer redelivers it after you changed the rules, it
is not re-processed into work.

## What a rule sees

Rules evaluate against one JSON document, the **routing envelope**. Top-level fields are decided by
switchboard, so a producer cannot forge them; everything the producer sent lives under `.payload`
and `.headers`.

| Path | Value |
|---|---|
| `.source` | the webhook's source type: `github`, `gitea`, `cairn`, `generic`, … |
| `.kind` | event kind — cairn's signed body `kind`, else `X-GitHub-Event` / `X-Gitea-Event` / `X-Cairn-Event`, else `null` |
| `.webhook_id` | the receiving webhook |
| `.trust_mode` | `signed` or `token` |
| `.verified` | `true` only when the signature verified |
| `.content_type` | the request `Content-Type`, or `null` |
| `.size` | payload size in bytes |
| `.headers` | sanitized request headers, **lower-cased** names (secrets read `«redacted»`) |
| `.payload` | the body parsed as JSON, or `null` when it is not JSON |
| `.artifact` | cairn only (`null` otherwise): `event_id`, `kind`, `created_at`, `id`, `handle` (`mcp://cairn/<id>`), `url`, `title`, `share_type`, `channel`, `model`, `actor_id`, `on_behalf_of`, `expires_at`, `tags` (cairn's string list), `metadata` |
| `.issue` | Gitea/GitHub `issues` events only (`null` otherwise, pull requests included): `provider`, `action`, `event_type`, `repo`, `number`, `title`, `url`, `state`, `author`, `sender`, `labels` (names), `label` (GitHub's changed label), `body_size`, `label_event`, `key` |

`.issue` reads the same on both forges: Gitea's label change (`issue_label`, action `label_updated`)
and GitHub's (`labeled`) both set `.issue.label_event`, with the current labels in `.issue.labels`.

The first output of your filter decides the match with jq truthiness: anything except `false` and
`null` matches; a filter that outputs nothing does not.

Rules can also read **`$params`**, an object you save alongside the rules (`set_webhook_rules`'s
`params`, up to 16 KiB). Keep allowlists there instead of splicing names into every expression:
`.issue.author as $a | any($params.trusted_humans[]; . == $a)`. Only the webhook owner's verbs
change params — a delivery cannot. `set_webhook_rules` replaces params with the rules; omitting
them clears them.

Rules are sandboxed. `env`/`$ENV`, `input`/`inputs`, `input_filename`, `debug`, `stderr`, `halt`,
`halt_error`, `now`, `localtime`, `strflocaltime`, and `import`/`include` are refused at save time.
Each rule gets 50 ms and a delivery's whole rule list 250 ms; evaluation runs in a separate,
memory-capped process. A rule that errors, times out, or blows its budget is treated as no-match
and recorded on the trace — it never fails the delivery. Limits: 32 rules, 4096-byte expressions.

## The tools

All seven are webhook self-management verbs on your vended endpoint, gated by scope and by owning
the webhook (any endpoint of the same human may manage it).

| Tool | Does |
|---|---|
| `list_webhook_rules` | the rules in order, the default, and the queues/endpoints actions may reach (`grant`) |
| `set_webhook_rules` | replace the whole list (and default) atomically |
| `add_webhook_rule` | insert one rule at a `position` (default: last) |
| `update_webhook_rule` | change a rule's name, expression, or action |
| `move_webhook_rule` | reorder — order is precedence |
| `remove_webhook_rule` | delete a rule (idempotent) |
| `test_webhook_rules` | dry-run: route a sample `payload` or one of this webhook's stored `event_id`s, with the saved or candidate rules — saves nothing |

A save that fails — a typo, a forbidden function, a queue or endpoint the webhook cannot reach —
names the offending rule and leaves the previous rules in force.

## Authoring loop

1. `list_webhook_rules` to see the `grant`.
2. Pull a real delivery id from `list_webhook_events` (the history now shows each event's
   `webhook_id` and `routing`).
3. `test_webhook_rules` with that `event_id` and your **candidate** `rules`. The result carries the
   `decision`, the `trace`, and the `envelope` the rules saw — write paths against that, not
   against a guess. Candidates are validated exactly like a save, so a dry-run that passes is a
   save that will pass.
4. `set_webhook_rules` with the list you tested.

Every todo a routed delivery creates carries the same trace in its `routing` field:

```json
{"stage": "rule", "rule_index": 7, "rule_id": "cairn-lane-m", "rule_name": "handoff pinned to lane:m",
 "action": {"queue": "lane-m", "exclusive": true, "once": true, "work_order": true}}
```

or, for the default, `{"stage": "default", "cause": "no_match_default", "action": {…}}`, with any
rule `faults` listed alongside.

## Worked example: cairn agent handoff

Cairn announces every artifact over one outbound webhook list, signed with **one** secret. Switchboard
mints a secret per signed webhook, so the shape that works is **one** cairn webhook with routes to
every lane pool, and rules that pick the pool. A handoff is an artifact tagged `handoff`, with
`lane:s|m|l|vision|auto` and `size:s|m|l|xl` choosing where it goes.

1. On the router endpoint, `create_webhook` with `source_type: "cairn"`. It is `signed`: keep the
   revealed `signing_secret` for cairn's `CAIRN_OUTBOUND_WEBHOOK_SECRET`, and the `ingest_url` for
   `CAIRN_OUTBOUND_WEBHOOK_URLS`.
2. `add_webhook_route` from that webhook to each lane pool endpoint.
3. `set_webhook_rules` — in full, that is the [fleet pack](../routing/rule-packs/README.md); its cairn
   half looks like this:

```json
{
  "webhook_id": "<cairn webhook id>",
  "params": {"require_verified": true, "cairn_actors": ["joestump-agent"]},
  "rules": [
    {"id": "cairn-not-handoff", "expr": ".source == \"cairn\" and (any((.artifact.tags // [])[]; . == \"handoff\") | not)",
     "action": {"drop": true}},
    {"id": "cairn-untrusted-actor",
     "expr": ".source == \"cairn\" and ((.artifact.actor_id // \"\") as $a | any(($params.cairn_actors // [])[]; . == $a) | not)",
     "action": {"drop": true}},
    {"id": "cairn-lane-m", "expr": ".source == \"cairn\" and any((.artifact.tags // [])[]; . == \"lane:m\")",
     "action": {"queue": "lane-m", "exclusive": true, "once": true, "work_order": true}},
    {"id": "cairn-triage", "expr": ".source == \"cairn\"",
     "action": {"queue": "triage", "exclusive": true, "once": true, "work_order": true}}
  ],
  "default_action": {"drop": true}
}
```

An artifact by `joestump-agent` tagged `["handoff", "lane:m"]` becomes one todo, on the `lane-m`
pool only. The same tags from any other actor are recorded and dropped: tags choose a lane, they
never grant trust.

Cairn's `data.tags` and `data.on_behalf_of` come from the cairn-handoff work; until cairn emits
them, `.artifact.tags` is `null` and nothing routes as a handoff.

Cairn deliveries verify strictly: the `X-Cairn-Signature` HMAC over the body, a signed `event_id`
(the dedup key — a replay collapses onto the original) and a signed `created_at` inside the
five-minute replay window. A tampered body, a stale replay, or an `X-Cairn-Event-Id` header that
disagrees with the body is a 401 and nothing is stored.

## Before you start: older endpoints

Endpoints vended before routing shipped were granted the verb list of their day: they lack the seven
rule verbs in their scope and `cairn` in their allowed source types. Endpoint scope is immutable
([SPEC-0007](/specs/identity/spec)) and there is no verb that widens it: re-vend the endpoint, rotate
its consumer's credential, and revoke the old one. Guide 08 covers the operator-only interim of
writing validated rules directly.

> Deeper detail: [ADR-0024 — Event routing](/decisions/ADR-0024-event-routing-deterministic-and-llm),
> [ADR-0025 — Handoff work orders and difficulty lanes](/decisions/ADR-0025-handoff-work-orders-and-difficulty-lanes),
> the [event-routing spec](/specs/event-routing/spec), and [guide 08](08-handoff-lanes.md) for the
> lanes runbook.
