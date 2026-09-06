# Design: Event Routing

## Context

Ingestion today is linear: verify → normalize → derive idempotency key → create todo on the webhook's target queue (`internal/ingest/selfmanaged.go`, per [SPEC-0001](../webhook-ingestion/spec.md)). Every delivery becomes a todo. That held while deliveries were scarce forge subscriptions; it breaks with volume producers (cairn `artifact.created` for every paste; Gitea org hooks; agent chatter on generic ingests). [ADR-0024](../../../adrs/ADR-0024-event-routing-deterministic-and-llm.md) adds a routing stage between idempotency derivation and todo creation: deterministic jq rules first, an optional LLM triage fallback, an explicit default, and a routing trace on every event.

## Goals / Non-Goals

### Goals

- Per-webhook ordered rules (jq filter + `queue`/`drop` action) evaluated sandboxed, first-match-wins
- Compile-time validation of rules; default action always defined
- Opt-in per-endpoint LLM triage with structured output, granted-queue constraint, budget, timeout, deterministic fallback
- Routing trace persisted on the event and surfaced on the todo
- Dedup contract untouched: routing outcomes never create or consume extra dedup slots

### Non-Goals

- Producer-side filtering (cairn `ring=true` et al.) — producers may set advisory metadata (`data.review_requested`) that rules can match, but the mechanism lives here
- Cross-endpoint or broadcast routing (fan-out to multiple queues) — one route per delivery in this iteration
- Pull-adapter routing ([ADR-0014](../../../adrs/ADR-0014-ingestion-adapters-push-pull.md) pull family) — the same stage applies once pull adapters exist; nothing here is push-specific except the entry point
- A rules UI in the operator board (MCP/API surface first; board later)

## Decisions

### Routing slot: after idempotency, before todo creation

**Choice**: the router runs inside the ingestion pipeline immediately after the idempotency key is derived and before any todo write.
**Rationale**: the dedup contract ([SPEC-0007](../persistence/spec.md)) is defined on (endpoint, idempotency key) and must not depend on routing outcomes; running routing after dedup would let a redelivery of a dropped event skip its drop rule. Routing reads the normalized event; it never re-derives identity.
**Alternatives considered**:
- Routing at claim time (workers filter their queue): scatters policy across consumers, breaks the todo-is-a-promise invariant, and cannot drop silently.
- Routing in the producer: rejected at ADR level — producers stay dumb.

### Rules: jq via gojq, first-match-wins, compiled at save

**Choice**: rules are stored on the webhook row as an ordered JSON array `{name, expr, action}` where `expr` is a jq filter returning a boolean (truthy = match) and `action` is `{"queue": "name"}` or `{"drop": true}`. Expressions compile with [gojq](https://github.com/itchyny/gojq) at save time (reject non-compiling expressions and non-granted queues) and evaluate per event under a 50 ms wall-clock and node-count cap.
**Rationale**: jq is already the lingua franca of webhook filtering (Joe asked for EventBridge-style power with jq ergonomics); gojq is pure Go, embeddable, and trivially sandboxed (no I/O opcodes). Compile-at-save moves every typo from silent runtime misroute to a loud API 400.
**Examples**:

```jq
# Drop CI chatter from the Gitea provider
.kind == "workflow_run"
# Human-requested reviews go to forge
.kind == "review_requested"
# Only sizeable human markdown pastes are worth an agent's time
.source == "cairn" and .data.share_type == "markdown" and .data.actor_id != null and (.data.size // 0) > 1024
```

**Alternatives considered**:
- EventBridge-pattern JSON matching: less expressive (no derived values like `.data.size > N` comparisons without pattern extensions); jq is a superset for our needs.
- CEL/expr-lang: equally capable, but jq matches the operator skill set and the ecosystem's webhook-filtering conventions.

### Default action

**Choice**: the webhook's `target_queue` remains the default unless the owner sets `default_action` (`queue` or `drop`) on the webhook. Save-time validation requires exactly one default.
**Rationale**: existing webhooks keep today's behavior with zero migration; opt-in tightening is per-webhook.

### LLM triage: registry-backed, schema-validated, always-fallback

**Choice**: `llm_triage` config on the endpoint: `{provider, model, queues: [{name, description}], min_confidence, prompt_budget_per_minute, prompt_budget_per_day, include_payload: false}`. The call uses the runtime provider registry ([ADR-0020](../../../adrs/ADR-0020-runtime-provider-registry.md)). The prompt presents the compact projection and the queue list; the model must answer `{"queue": "...", "confidence": 0..1, "reason": "..."}`. Validate with a strict JSON-schema check: unknown queue → error path; confidence < `min_confidence` → fallback; any transport/timeout/budget failure → fallback. Fallback action is the webhook's default; the trace records which fallback fired.
**Rationale**: constrained to granted queues, the worst case of a bad model day is "everything takes the default," i.e. option (B)'s behavior. Registry reuse means no new credential surface.
**Alternatives considered**:
- Letting the model pick among all queues endpoint-wide: violates least surprise — owners subscribe queues to an endpoint deliberately.
- Embeddings/classifier instead of generative triage: cheaper, but cold-start labeling per endpoint is worse UX for an opt-in MVP; revisit if budgets bite.

### Drop semantics: spend the dedup slot, keep the receipt

**Choice**: on `drop`, persist the event row (with routing trace) and the (endpoint, idempotency key) dedup entry; create no todo; no ring, no notification.
**Rationale**: without spending the slot, a producer's redelivery re-runs routing on a flappy rule and can double-process; without the event row, "where did my webhook's deliveries go?" becomes unanswerable. Drops are visible in history, just silent.

### Trace: one JSON column, written with the event

**Choice**: `routing_trace` JSON on the event row: `{"stage":"rule","rule_index":0,"action":{"queue":"forge"}}` or `{"stage":"llm","model":"...","queue":"...","confidence":0.83,"reason":"..."}` or `{"stage":"fallback","cause":"low_confidence", ...}`. Todos carry the same trace via the event join.
**Rationale**: a todo that cannot explain why it exists trains owners to distrust the queue; writing the trace atomically with the event keeps history honest.

### Rule management surface

**Choice**: extend the MCP agent-tools surface (SPEC for agent tools) with `webhook_rules_get/set` (the set call validates and stores atomically) plus the corresponding REST endpoints for the operator board later.
**Rationale**: endpoints are agent-self-managed ([ADR-0012](../../../adrs/ADR-0012-agents-self-manage-webhooks.md)); rules are webhook config, so they ride the same surface and auth.

## Architecture

```mermaid
sequenceDiagram
    participant P as Producer (Gitea/cairn/agent)
    participant I as selfmanaged ingest
    participant R as Router
    participant J as gojq sandbox
    participant L as LLM (registry)
    participant Q as todo queue
    P->>I: POST /webhooks/w/{token}
    I->>I: verify → normalize → idempotency key
    I->>R: normalized event + webhook rule list + default
    R->>J: evaluate rules in order
    alt rule matches
        J-->>R: match (first)
    else no match and llm_triage configured and budget remaining
        R->>L: compact projection + queue descriptions
        L-->>R: {queue, confidence, reason}
        R->>R: validate queue ∈ granted, confidence ≥ min
    end
    R-->>I: route = queue | drop | default(+fallback cause)
    I->>Q: create todo (or drop: event row only, dedup slot spent)
    Note over I,Q: routing_trace written with the event; surfaced on the todo
```

## Risks / Trade-offs

- **Rule-order sensitivity** → first-match-wins is documented, rules carry names, the trace names the matched index; a future lint can warn when a later rule is fully shadowed by an earlier one.
- **LLM cost runaway** → per-minute and per-day budgets with operator ceilings; budget exhaustion degrades to default, never to queue-starvation.
- **Model egress of payload text** → off by default; enabling `include_payload` is an explicit, recorded per-endpoint decision.
- **gojq divergence from real jq** → we support the pure-filter subset; the save-time compiler is the compatibility contract (if gojq compiles it, it routes).
- **Two routing layers to debug** → the trace distinguishes stages and causes on every event; no silent default.

## Migration Plan

Additive: new nullable columns (`webhooks.rules`, `webhooks.default_action`, `endpoints.llm_triage`, `events.routing_trace`) with a zero-value migration meaning "route to target_queue as today." No behavior change until an owner saves rules or opts into triage. Rollback = clear the new columns.

## Open Questions

- Should `drop` also suppress channel doorbells for retried redeliveries of the same key, or is dedup alone sufficient? (Likely dedup is sufficient — dedup runs first.)
- Shadowed-rule linting in `webhook_rules_set` (warn when a rule can never match) — cheap and useful; defer to first iteration of operator tooling?
- Should cairn set advisory `data.review_requested` metadata so cairn owners can write a single obvious rule? (Producer-side, out of scope here, but the rule surface should make it natural.)
