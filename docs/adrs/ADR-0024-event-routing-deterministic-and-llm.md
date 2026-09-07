---
status: proposed
date: 2026-09-06
decision-makers: [joestump, joestump-agent]
governs: [SPEC-0020]
related: [ADR-0003, ADR-0007, ADR-0012, ADR-0014, ADR-0022]
---

# ADR-0024: Event Routing — Deterministic (jq) Rules with an Optional LLM Router

## Context and Problem Statement

Every delivery that reaches a self-managed webhook ([ADR-0012](ADR-0012-agents-self-manage-webhooks.md), [SPEC-0001](../openspec/specs/webhook-ingestion/spec.md)) becomes a todo on the webhook's target queue — verbatim, unfiltered. That was right when deliveries were scarce forge events a human had subscribed to on purpose. It is wrong now that first-class producers are coming online: cairn emits an `artifact.created` for **every** paste ([cairn #175](https://gitea.stump.rocks/stump.wtf/cairn/pulls/175)), Gitea org hooks deliver every review request and comment, and agent-to-agent traffic arrives over the same generic ingests. A queue that turns everything into a todo drowns the workers: the todo contract ([ADR-0007](ADR-0007-todos-as-core-primitive.md)) promises that a todo is *work someone should act on*, and today the producer decides that, not the receiver.

The receiver is the right place to decide. Producers should stay dumb (one event shape, no knowledge of consumer queues); the endpoint owner already holds the trust relationship and the queue topology. What is missing is a **routing stage** between "delivery verified and normalized" and "todo created" that can say: this goes to queue X, this goes to queue Y, and this is not a todo at all.

Two kinds of routing decisions exist in practice. Most are **mechanical**: `workflow_run` noise from the Gitea provider, artifact pastes under 1 KB, events from a retired actor — a deterministic predicate over the event's JSON decides them. Some are **judgment calls**: "is this paste actually a request for review, or just notes?" — exactly the question an LLM answers well and a jq expression cannot. The routing stage should support both, with the deterministic layer authoritative and the LLM layer optional, bounded, and never the sole path to a queue.

## Decision Drivers

* **The todo is a promise.** [ADR-0007](ADR-0007-todos-as-core-primitive.md)'s contract — at-least-once, deduplicated, someone acts on it — only holds if reaching a queue means something. Routing must be able to **drop** as a first-class action, not just redirect.
* **Determinism where possible.** A route that depends on a model call is a route that can flip between retries, cost money per event, and leak payload text to a third party. The bulk of routing decisions are structural and must be decided by code.
* **Judgment where needed.** Producers cannot anticipate every consumer's notion of "needs review." An optional model-based stage lets an endpoint owner say "anything I haven't matched, have a model triage it into my queues" without the producer changing.
* **Tenant isolation.** Endpoints are vended per principal ([ADR-0008](ADR-0008-human-principal-vended-endpoints.md)) and todos are endpoint-pinned ([ADR-0022](ADR-0022-endpoint-scoped-todo-ownership.md)). A routing rule — deterministic or model-chosen — must never be able to move an event to a queue or endpoint the webhook's owner cannot already reach.
* **The dedup contract must not move.** Idempotency keys are derived at receive ([ADR-0014](ADR-0014-ingestion-adapters-push-pull.md)). Routing decides *where (or whether)* the todo lands, not *whether this delivery was already seen*. A dropped event must still record its dedup slot so a redelivery is not re-processed.

## Considered Options

* **(A) Producer-side filtering** — producers only emit "important" events (cairn grows a `ring=true` flag; the Gitea org hook narrows its event list).
* **(B) Deterministic-only routing** — per-ingest ordered rules with jq-style filters and `queue`/`drop` actions; no model involvement, ever.
* **(C) Deterministic rules first, optional LLM triage as a bounded fallback stage** — jq rules evaluate first; unmatched events may go to a per-endpoint-configured LLM router that must answer in a constrained format, with low-confidence or errored triage falling back to a configured default. *(chosen)*
* **(D) LLM-only routing** — every event is triaged by a model.

## Decision Outcome

Chosen option: **"(C) deterministic jq rules first, optional bounded LLM fallback."**

Routing becomes an explicit stage in the ingestion pipeline, after verification and normalization and before todo creation:

```
verify → normalize → derive idempotency key → ROUTE (rules → optional LLM → default) → create todo | drop
```

Each self-managed webhook gains an ordered **rule list**. A rule is a jq-style filter expression evaluated against the normalized event JSON, with an action: deliver to a named queue (within the endpoint's granted set) or **drop**. First match wins; an explicit default action (the webhook's current target queue, or drop) terminates evaluation. Rules are validated at save time — a rule whose expression does not compile is rejected, so a typo can never silently route everything to the default. Expressions evaluate in a sandboxed jq interpreter (gojq) with no filesystem, network, or function-extension access.

When no rule matches **and** the endpoint has opted in, the event goes to the **LLM router**: a single model call presenting a compact projection of the event (kind, source, title, size, actor — never the full body unless the endpoint explicitly allows it) and the endpoint's queue descriptions, requiring a structured answer `{queue, confidence, reason}`. The router may only answer with queues in the endpoint's granted set; a hallucinated queue, a low-confidence answer, a timeout, or any error falls back to the deterministic default. Cost and latency are bounded by per-endpoint caps (events per minute, per-day budget) and a hard per-call timeout; when the budget is exhausted the default applies. Every LLM route records the model's queue, confidence, and reason on the event's routing trace, so a human can audit why a todo exists.

Events that route to **drop** still persist their event row (dedup slot spent, delivery visible in history) but create no todo and ring no doorbell — noise stays auditable without becoming work.

### Consequences

* Good, because the todo contract is restored: reaching a queue means a rule or a triage decision said so, and both are recorded.
* Good, because producers stay dumb — one event shape, no consumer topology, no `ring` flags to forget.
* Good, because deterministic rules are free, instant, and auditable; the common 90% of routing never touches a model.
* Good, because the LLM stage is opt-in, budgeted, constrained to granted queues, and always has a deterministic fallback — it cannot make routing *less* reliable than option (B).
* Bad, because rule lists are per-webhook configuration that someone must write; a badly-ordered rule list silently shadows later rules (first-match-wins makes ordering load-bearing).
* Bad, because the LLM stage sends event metadata (and optionally payload text) to a model provider — an egress decision the endpoint owner must make knowingly, per endpoint, with a stated model.
* Bad, because two routing layers means two failure modes to observe (a wrong jq expression; a confident-but-wrong model) — the routing trace on every event is what keeps both debuggable.

### Confirmation

SPEC-0020 scenarios cover: first-match-wins ordering, compile-time rule validation, drop semantics with dedup-slot persistence, LLM constrained-output enforcement, hallucinated-queue and low-confidence fallback, budget exhaustion, and the guarantee that an LLM answer naming a non-granted queue is treated as an error, never a route. The routing trace (matched rule or LLM decision) is asserted on every routed event.

## More Information

* Spec: [SPEC-0020 — Event Routing](../openspec/specs/event-routing/spec.md); design: [design.md](../openspec/specs/event-routing/design.md).
* Contrast with the producer-side alternative: cairn's `ring=true` idea (discussed 2026-09-06) remains useful as *advisory metadata* — a producer may set `data.review_requested: true` that a consumer's rules can match on — but it is a hint, not the routing mechanism.
* Implementation note: `gojq` (pure-Go jq) is the candidate interpreter; the LLM call reuses the runtime provider registry ([ADR-0020](ADR-0020-runtime-provider-registry.md)) so endpoints name a provider + model rather than an HTTP endpoint.
