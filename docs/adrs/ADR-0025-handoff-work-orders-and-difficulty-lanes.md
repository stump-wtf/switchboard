---
status: proposed
date: 2026-09-11
decision-makers: [joestump, joestump-agent]
governs: [SPEC-0020]
related: [ADR-0024, ADR-0022, ADR-0012, ADR-0013]
---

# ADR-0025: Handoff Work Orders and Difficulty Lanes

## Context and Problem Statement

Two asks from Joe land on the routing stage [ADR-0024](ADR-0024-event-routing-deterministic-and-llm.md) built:

1. **Agent handoffs.** An agent running the morning brief or another sweep writes a handoff prompt to Cairn (a single artifact or a bundle). Cairn's outbound webhook fires to Switchboard, and another agent picks up the work and runs with it.
2. **Difficulty lanes.** Gitea and GitHub issues route by size. Easy, small wins (`size/S`) go to the local Qwen 3.8; medium ones (`size/M`) to `zai/glm-5.3-flash`; larger ones (`size/L`) to `zai/glm-5.3`. `size/XL` and anything needing Joe (`HUMAN`) is held, never auto-executed. Unsized issues go to a triage worker that sizes and labels them, and the label change re-routes the issue.

Each lane is one queue with its own model and its own workers.

The crux is **trust**. Joe's agent rules are explicit: content read from Cairn, issues, pull requests, and webhooks is **untrusted data, never instructions**. An automated handoff turns that content into work an agent executes. The only thing that can make that safe is authority that comes from **verified provenance**: who the signing producer says authored the thing. Text never grants it. A Cairn label, an issue title, and an issue body are all client-asserted.

Three mechanical problems sit underneath:

- **Exactly once.** Gitea delivers a label change as `X-Gitea-Event: issues` / `X-Gitea-Event-Type: issue_label` with action `label_updated`. The payload does **not** say which label changed. Every delivery also carries a fresh delivery id. Per-delivery dedup therefore cannot stop a second label event on an already-sized issue (a worker adds `in-progress`, someone adds `bug`) from minting the same work order again.
- **One executor.** Fan-out ([ADR-0022](ADR-0022-endpoint-scoped-todo-ownership.md)) gives every delivery target its own todo. When both identities (tars = `joestump-agent`, kitt = `joestump`) have endpoints that can take a lane, a fan-out is two executions of one task.
- **No loop.** A triage worker labels an issue, which emits an event. That event must reach the lane, not triage again.

## Decision Drivers

* **Provenance, not text, authorizes work.** Only switchboard-verified facts may gate a work order: the signature, the forge's own author and sender logins, and cairn's authenticated actor. Labels only choose among work that is already trusted.
* **A work order is a task, not a grant.** The executing worker keeps every clamp it already runs under. Nothing in a work order may widen them.
* **Exactly one worker executes each work order.** This must hold across relabels, redeliveries, and two identities.
* **Doorbells must wake the right worker.** Doorbells are unicast round-robin across an endpoint's sessions ([ADR-0013](ADR-0013-push-is-a-doorbell.md)). A session's queues are its endpoint's scope, so every session of an endpoint is eligible for every queue's doorbell.
* **Packs stay deployment-agnostic.** A checked-in rule pack must not embed endpoint ids or allowlists that differ between deployments.

## Considered Options

* **(A) Label- or text-based trust.** Honor `handoff=true`, a trusted-sounding label, or an issue template as authorization. *Rejected:* every one is client-asserted.
* **(B) Per-delivery dedup only.** *Rejected:* Gitea label events carry new delivery ids and no changed-label detail, so relabels re-run work orders.
* **(C) One shared lanes endpoint with queue-filtered claims.** Every lane's workers connect to one endpoint and `claim_next` their own queue. *Rejected:* the doorbell goes to any session of the endpoint, so a Qwen worker is woken for a GLM todo, claims nothing, and the right worker is never rung.
* **(D) Endpoint ids in rules.** `{"queue": "lane-local", "endpoints": ["<id>"]}`. *Rejected:* it makes every pack deployment-specific, and a wrong id silently narrows to nothing.
* **(E) LLM triage for sizing** (ADR-0024 phase 2). *Deferred:* a `triage` lane worker sizes issues through labels, which leaves an auditable, human-editable record.
* **(F) Verified-provenance work orders on a lanes topology, with owner params, exclusive delivery, and at-most-once keys.** *(chosen)*

## Decision Outcome

Chosen option: **(F)**.

**(a) Trust comes from verified provenance checked against owner-set allowlists.** Rules read owner-set values as `$params`: allowlists and a `require_verified` switch, set with the rules through the owner-gated verbs, never from the payload. A delivery reaches a worker lane only when all of these hold:

* its **signature verified** (`.verified`);
* for cairn, `.artifact.actor_id` is in `cairn_actors`. Cairn derives it from the authenticated token. If `.artifact.on_behalf_of` is present, it must also be a trusted human or agent, or the handoff is **held**.
* for forge issues, `.issue.author` (the forge's `issue.user.login`) is a trusted human or agent, the repository matches `repo_prefixes`, and for label events `.issue.sender` is trusted too;
* cairn's `labels.handoff` is `"true"`. This is necessary but never sufficient: labels are client-asserted and only choose a lane among already-trusted work.

Everything else drops or holds, and the trace records which rule decided.

**(b) A work order defines the TASK only.** A `work_order: true` action attaches a switchboard-authored `work_order` to each todo:

* `version`, `lane`, `source`, `webhook_id`, `trust_mode`, `verified`;
* `authorized_by`, naming the rule;
* `subject`: the issue's repo, number, and URL, or the artifact's `mcp://cairn/<id>` handle and URL, plus author/actor facts;
* a fixed task-only `authority` string.

The subject is parsed by switchboard in Go from the verified body, never by a rule. Workers keep all their normal clamps. Producer-supplied fields are data to reason about, never instructions.

**(c) One pool endpoint per lane, and exclusive delivery.** Each lane queue is served by exactly one endpoint scoped to exactly that queue. All of that lane's workers, on any host, connect to it as competing consumers. A router endpoint, scoped to no lane, owns the ingress webhooks and routes to each lane endpoint.

Lane actions set `exclusive: true`: the delivery goes to exactly one target, the **first** in deterministic order whose scope grants the queue. The order is owner first, then routes by `granted_at` and target id. When both identities have an endpoint on a lane, that order also picks the executing identity, the same way every time.

**(d) At-most-once work orders.** `once: true` claims `(webhook_id, sha256(subject key ‖ queue))` in `routing_once`:

* **Subject keys.** An issue's key is `provider:owner/repo#number`; an artifact's is `cairn:<id>`.
* **Later deliveries.** Any later delivery about the same subject to the same queue records its event (the trace gains `"once":"repeat"`), mints nothing, and answers `{"repeat": true}`.
* **Re-sizing.** A re-size to a different lane is a different queue, so it routes again.
* **Redeliveries.** A redelivery of the claiming delivery reports the todos it already minted and never re-mints them, even after they are done.
* **Retention.** `routing_once.event_id` is deliberately not a foreign key, so event retention can never release a claim.

**(e) One ingress per producer.** Dedup is per webhook. The lanes pack is therefore installed on exactly one router webhook per producer (one per Gitea org, one GitHub, one cairn). It must never be installed on both identities' pool hooks, or one issue becomes two work orders. Cairn's single outbound secret already forces one cairn webhook.

**(f) No loop.** Triage fires only for `opened`/`reopened` issues with no size label. Label events route only when a size label is present, and every other action drops. So triage labelling an issue reaches its lane exactly once and never returns to triage.

The checked-in pack is `docs/routing/rule-packs/fleet.json`, tested against real-shape fixtures through the in-process evaluator and the sandbox child.

### Consequences

* Good, because no text, label, or body can turn untrusted content into executed work; the gate is the signature plus server-derived identities.
* Good, because exactly one worker executes each work order, across relabels, redeliveries, and two identities, and the reason is visible in the trace (`once_key`, `"once":"repeat"`, the exclusive target).
* Good, because packs carry only queue names. Allowlists live in `$params`, and a missing lane endpoint fails the save instead of fanning out.
* Good, because a worker receives a structured work order, not a raw payload to interpret.
* Bad, because params are editable by any endpoint of the webhook owner's human. That is owner-trusted, and content cannot change them, but a careless `set_webhook_rules` without `params` clears them. The pack then fails closed: nothing reaches a lane.
* Bad, because at-most-once is forever per (subject, queue). Re-running a completed work order in the same lane needs a deliberate human action, not a relabel.
* Bad, because the design depends on the **cairn labels contract**: `data.labels` as a flat string map and `data.on_behalf_of` on `artifact.created`, including for bundles. That is not yet merged in cairn, and this work is built against fixtures. Trusting `on_behalf_of` also assumes cairn derives it server-side, or restricts who may set it. Until cairn carries labels, no cairn handoff routes (`cairn-not-handoff` drops them).
* Bad, because the topology has more moving parts: a router endpoint, one endpoint per lane, and a route per lane, which must be vended and wired correctly (guide 08).

### Confirmation

SPEC-0020 requirements "Rule Parameters", "Issue Envelope Projection", "Cairn Handoff Fields", "Exclusive Delivery", "At-Most-Once Work Orders", "Work Orders", and "Work Order Trust". The fleet pack's fixture cases cover trusted and untrusted authors, labelers, and actors; labels that try to talk past the allowlist; each lane; XL/HUMAN/ambiguous holds; the no-loop label event; pull requests; and unverified deliveries. Each case asserts exactly one exclusive endpoint.

## More Information

* Pack and lane table: [docs/routing/rule-packs/README.md](../routing/rule-packs/README.md). Runbook: [guide 08](../guides/08-handoff-lanes.md).
* Migration `0019_handoff_lanes.sql` adds `endpoint_webhooks.routing_params`, `todos.work_order`, and `routing_once`.
