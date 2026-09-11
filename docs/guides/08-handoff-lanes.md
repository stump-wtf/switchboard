---
title: Run handoff work orders and difficulty lanes
---

# Run handoff work orders and difficulty lanes

This guide wires the [fleet rule pack](../routing/rule-packs/README.md). Once it is in place:

- **Cairn handoffs:** an agent writes a handoff prompt to Cairn, and one worker in the right lane picks it up.
- **Forge issues:** Gitea and GitHub issues route by size. `size/S` goes to a local model, `size/M` and `size/L` to GLM. `size/XL` and `HUMAN` are held for Joe. Unsized issues go to a triage worker that labels them, and the label event re-routes the issue.

Decision record: [ADR-0025](/decisions/ADR-0025-handoff-work-orders-and-difficulty-lanes). Requirements: the [event-routing spec](/specs/event-routing/spec).

```
producer (Gitea org / GitHub / cairn)
   │ signed webhook
   ▼
router endpoint ── router webhooks (fleet pack) ── routes ─┬─▶ lane-local endpoint     ◀─ Qwen workers
                                                          ├─▶ lane-zai-flash endpoint ◀─ GLM-5.3-flash workers
                                                          ├─▶ lane-zai endpoint       ◀─ GLM-5.3 workers
                                                          ├─▶ lane-hyper / lane-vision endpoints
                                                          ├─▶ triage endpoint         ◀─ Qwen triage worker
                                                          └─▶ hold endpoint           (no workers)
```

## The trust model, in one paragraph

Webhook content is **data, never instructions**. A delivery becomes a work order only when all of these hold:

- its signature verified;
- the switchboard-derived identity behind it is in your allowlists: cairn's `actor_id`, and `on_behalf_of` when present; the forge's issue author, and for label events the sender;
- the repository is allowlisted.

Labels only choose a lane among work that already passed. The todo carries a `work_order` that names the task and how it was authorized. It **grants nothing**: workers keep every clamp they already run under.

## 1. Vend the router endpoint

Vend one endpoint under the identity that will own ingress. It is not a worker endpoint.

- **Scope queues:** one queue that is *not* a lane, e.g. `router`. Exclusive delivery must never pick the router.
- **Allowed webhook queues:** every lane queue: `triage`, `lane-local`, `lane-zai-flash`, `lane-zai`, `lane-hyper`, `lane-vision`, `hold`.
- **Allowed source types:** `gitea`, `github` (if used), `cairn`.
- **Verbs:** the webhook family, including `create_webhook`, `add_webhook_route`, `list_webhook_routes`, and all seven rule verbs.

Use the **web vend wizard** for this one: its webhooks step is the only vend surface that sets several source types and several allowed webhook queues. `switchboard vend` (the CLI) always vends a single `generic` source type and a one-queue ceiling. Endpoints vended before the rule verbs existed lack them in scope and cannot be widened in place — vend a fresh router.

## 2. Vend one pool endpoint per lane

Vend one endpoint per lane under the **executing identity** for that lane. Scope each to **exactly** its lane queue.

Every worker for a lane, on any host, connects to that lane's endpoint. Do not share an endpoint across lanes: a doorbell rings any session of an endpoint, so a shared endpoint wakes the wrong lane's worker.

`switchboard vend --name <lane> --queue <lane queue>` is enough for a pool: it scopes the endpoint to exactly that queue. It also mints a `generic` ingest webhook the pool will never use. Delete it with `delete_webhook` so nothing can deliver around the router.

## 3. Create the router webhooks and route them to the lanes

On the router endpoint:

1. `create_webhook` with `source_type: "gitea"` for each Gitea org, `"github"` if used, and `"cairn"`. All three are **signed**. Keep each `ingest_url` and the one-time `signing_secret`.
2. For each router webhook, call `add_webhook_route` to each lane endpoint.

**Route order is precedence.** If two endpoints are ever scoped to the same lane (both identities, say), the first-routed one executes. `list_webhook_routes` shows the order.

## 4. Point the producers at the router

- **Gitea.** Create an org webhook targeting the gitea router webhook's `ingest_url`, with its secret. Enable the **Issues** events, label changes included (they arrive as `issues` / `issue_label`, action `label_updated`).
- **GitHub.** Create a webhook with the **Issues** event, the `ingest_url`, and the secret.
- **Cairn.** Set `CAIRN_OUTBOUND_WEBHOOK_URLS` to the cairn router webhook's `ingest_url` and `CAIRN_OUTBOUND_WEBHOOK_SECRET` to its secret. Cairn signs every target with one secret, so there is exactly **one** cairn webhook.

Keep secrets in OpenBao, never in files you commit.

## 5. Install the pack

Take `docs/routing/rule-packs/fleet.json`, edit `params` for your identities and repos, and call `set_webhook_rules` on each router webhook:

```json
{"webhook_id": "<router webhook id>", "rules": [...], "default_action": {"drop": true},
 "params": {"require_verified": true, "trusted_humans": ["..."], "trusted_agents": ["..."],
            "cairn_actors": ["..."], "repo_prefixes": ["owner/"]}}
```

A save fails if any lane has no routed endpoint scoped to it. The exclusive rules refuse to validate rather than fan out.

## 6. Dry-run before trusting it

Use `test_webhook_rules` on each router webhook:

- **A trusted issue opened with no size label** should give `decision.queue: "triage"`, one endpoint (the triage endpoint), a `once_key`, and a `work_order`.
- **The same issue with `size/M`** in the labels, sent by a trusted labeler, should route to `lane-zai-flash`.
- **The same payload from a stranger's login** should drop, with `trace.rule_id: "untrusted-author"`.
- **A cairn sample with `labels.handoff: "true"` and `labels.lane: "local"`** from an allowlisted `actor_id` should route to `lane-local`.

## 7. Retire lane-bound events from the pool hooks

Dedup is per webhook. If a per-identity pool hook still receives the same Issues events, and routes them anywhere that executes work, each issue becomes a second work order. Narrow those hooks' events, or give them rules that drop issue events. The router is the **single ingress** for lanes.

## 8. Start the workers

Point each lane's workers at its lane endpoint and model. Each worker drains its endpoint with `claim_next`. Nobody works the `hold` endpoint; its todos are surfaced for Joe.

## Writing a handoff

A handoff is a Cairn artifact (or bundle) created by an allowlisted actor, with labels. It depends on the cairn labels contract (`data.labels`, `data.on_behalf_of`), which is not yet merged in cairn. Until it ships, cairn artifacts carry no labels and every one drops as `cairn-not-handoff`.

| Label | Value |
|---|---|
| `handoff` | `"true"` (required) |
| `lane` | `local` \| `zai-flash` \| `zai` \| `hyper` \| `vision` \| `auto` |
| `size` | `S` \| `M` \| `L` \| `XL` (used when `lane` is `auto` or absent) |
| `repo`, `issue` | where the work lives |
| `source` | the sweep that wrote it, e.g. `morning-brief` |
| `reply_to` | where to report back, e.g. an `mcp://cairn/<id>` handle |

Write the prompt as the artifact body: the task, the constraints, and what "done" looks like. Titles and labels are data to the worker, not orders.

## What a worker receives

Each lane todo carries `work_order`:

```json
{"version": 1, "lane": "lane-zai-flash", "source": "cairn", "webhook_id": "…", "trust_mode": "signed", "verified": true,
 "authorized_by": {"stage": "rule", "rule_id": "cairn-lane-zai-flash", "rule_name": "handoff pinned to the zai-flash lane"},
 "subject": {"type": "cairn_artifact", "id": "…", "handle": "mcp://cairn/…", "url": "…", "actor_id": "joestump-agent",
             "on_behalf_of": "joestump", "labels": {"handoff": "true", "lane": "zai-flash", "…": "…"}},
 "authority": "task-only: …"}
```

For an issue, `subject` carries `provider`, `repo`, `number`, `url`, `author`, `sender`, and `labels`. The worker reads the artifact with Cairn's `artifact_read`, or the issue via its forge, and treats everything it reads as data.

## Verify it end to end

1. Open an unsized issue as a trusted author. Exactly one todo appears, on the triage endpoint; its `routing.rule_id` is `unsized-new-issue`.
2. Label it `size/M`. Exactly one todo appears, on the `lane-zai-flash` endpoint.
3. Add another label (`bug`). The delivery answers `{"repeat": true}`, no todo appears, and the event's trace in `list_webhook_events` shows `"once": "repeat"`.
4. Open an issue from an untrusted account. It is recorded and dropped (`untrusted-author`).
