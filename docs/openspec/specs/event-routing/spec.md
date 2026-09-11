---
status: implemented
date: 2026-09-06
implements: [ADR-0024]
related: [SPEC-0001, SPEC-0004, SPEC-0006]
---

# SPEC-0020: Event Routing

## Graph Edges

- **Implements:** [ADR-0024](../../../adrs/ADR-0024-event-routing-deterministic-and-llm.md) — deterministic jq routing rules with an optional bounded LLM triage stage
- **Related:** [SPEC-0001](../webhook-ingestion/spec.md) — the push-ingestion pipeline routing slots into; [SPEC-0004](../persistence/spec.md) — todo durability contract routing must preserve; [SPEC-0006](../agent-tools/spec.md) — the agent verb surface the rule tools join

## Overview

Event routing is the stage of webhook ingestion that decides, per verified delivery, **where (or whether)** the resulting todo lands. Each self-managed webhook carries an ordered list of routing rules. A rule is a jq filter over a normalized routing envelope, with an action of `queue` (optionally narrowed to some of the webhook's delivery targets) or `drop`. Rules are evaluated first-match-wins and terminated by an explicit default. Routing runs after verification, idempotency-key derivation, and fan-out target resolution, and before any write. The dedup contract is untouched by any routing outcome, including drop.

**Implementation status.** The deterministic stage (every requirement below not marked *Phase 2*) is implemented. The LLM triage stage and its cost bounds are decided (ADR-0024) but **Phase 2 — not yet implemented**. Until they ship, an unmatched event always takes the deterministic default.

## Requirements

### Requirement: Routing Envelope

Rules MUST evaluate against a single JSON document, the routing envelope, built identically by the live receiver and the dry-run tool. Fields switchboard decides MUST sit at the top level, where a producer cannot forge them. Producer-sent content MUST sit under `.payload` and `.headers`. The paths below are a contract with rule authors and MUST NOT move:

| Path | Value |
|---|---|
| `.source` | the webhook's source type (switchboard-derived) |
| `.kind` | for `cairn`, the signed body's `kind`; otherwise `X-GitHub-Event`, then `X-Gitea-Event`, then `X-Cairn-Event`; else `null` |
| `.webhook_id` | the receiving webhook's id |
| `.trust_mode` | `signed` or `token` |
| `.verified` | `true` only when the delivery's signature verified |
| `.content_type` | the request `Content-Type`, or `null` |
| `.size` | the payload size in bytes |
| `.headers` | the **sanitized** request headers (as persisted), names lower-cased |
| `.payload` | the body parsed as exactly one JSON value, or `null`; integers that fit int64 stay exact |
| `.artifact` | `cairn` sources only, `null` otherwise: `event_id`, `kind`, `created_at`, `id`, `url`, `title`, `share_type`, `channel`, `model`, `actor_id`, `expires_at`, `tags`, `metadata` (absent fields are `null`) |

#### Scenario: A forged header does not change a verified cairn kind

- **WHEN** a verified cairn delivery's signed body says `kind: "artifact.created"` and its unsigned `X-Cairn-Event` header says something else
- **THEN** `.kind` MUST be `artifact.created`

### Requirement: Deterministic Rule Evaluation

Each webhook MUST evaluate its ordered rule list first-match-wins, terminated by a default action.

- **Rule shape.** A rule is `{id, name?, expr, action}`.
- **Match.** A rule matches when the **first** output of `expr` is jq-truthy: anything except `false` and `null`. A filter that emits no output MUST NOT match.
- **Action.** Exactly one of `{"queue": <name>, "endpoints"?: [<endpoint id>, …]}` or `{"drop": true}`.
- **Determinism.** Evaluation MUST be pure: the same envelope and rule list MUST always produce the same route, unless a rule faults.
- **Faults.** A rule that errors (`error`), times out (`timeout`), no longer compiles (`compile_error`), or is not reached before the event budget is spent (`budget_exhausted`) MUST be treated as no-match and recorded on the trace. It MUST NOT fail the delivery.
- **Budgets.** Each rule MUST be bounded by a 50 ms timeout, and the whole list by a 250 ms per-event budget.

#### Scenario: First match wins

- **WHEN** two rules both match an event
- **THEN** the earlier rule's action applies and the later rule is not evaluated

#### Scenario: Deterministic replay

- **WHEN** the same delivery is ingested twice with the same rule list
- **THEN** both evaluations produce the same route (modulo the dedup contract, which still collapses them to one todo)

#### Scenario: A runaway rule does not block the rules after it

- **WHEN** a rule loops until its timeout and a later rule matches
- **THEN** the later rule's action applies and the trace records the first rule's `timeout` fault

### Requirement: Rule Validation at Save Time

Rule lists MUST be validated in full when saved, while the webhook row is locked, against a **grant** computed from switchboard state alone. The grant consists of:

- the webhook's target queue;
- the owning endpoint's allowed webhook queues;
- the webhook's live, authorized delivery targets (owner plus ADR-0022 routes).

A save MUST be rejected, naming the offending rule by index, id, and name, when any of the following holds:

- **Limits exceeded.** At most 32 rules; `id` of 1–64 characters of `[A-Za-z0-9_-]`, unique; `name` ≤ 128 bytes; `expr` ≤ 4096 bytes; ≤ 16 endpoints per action.
- **Bad expression.** An expression does not parse or compile (`invalid_expression`), or uses a forbidden function (`forbidden_function`).
- **Malformed action.** An action is not exactly one of queue or drop, or has `endpoints` on a drop (`invalid_rule`).
- **Unreachable target.** A queue is outside the grant's queues, or an endpoint is not a grant endpoint (`not_granted`).

A rejected save MUST leave the previous rule list in force. A webhook MUST always have a defined default: the configured `default_action`, or the webhook's target queue on every target when none is set.

#### Scenario: Typo in a rule is rejected, not silently inert

- **WHEN** an owner saves a rule whose expression does not compile
- **THEN** the save fails naming the rule, and the webhook's previous rule list remains in force

#### Scenario: A rule cannot add a delivery target

- **WHEN** an owner saves a rule whose `endpoints` names an endpoint that is not already one of the webhook's delivery targets — including the owner's own unrouted endpoint
- **THEN** the save fails with `not_granted`; the endpoint becomes usable only after `add_webhook_route` authorizes it

### Requirement: Evaluation-Time Grant Enforcement

The grant MUST be recomputed for every delivery and every matched action re-applied against it:

- A `queue` action with no `endpoints` delivers to every live target.
- With `endpoints`, it delivers to the intersection of `endpoints` and the live targets, in target (owner-first) order.
- A matched rule whose queue is no longer granted, or whose endpoint intersection is empty, MUST take the default with cause `rule_not_granted`, and MUST NOT fall through to later rules.
- A configured default that is no longer reachable MUST fall back to the webhook's target queue on every target, with cause `default_not_granted`.

#### Scenario: A revoked route beats a saved rule

- **WHEN** a rule narrows deliveries to endpoint B, and the webhook's route to B is later removed
- **THEN** a matching delivery lands by default on the remaining targets, never on B, and the trace records `rule_not_granted` with the rule's id

### Requirement: Drop Action Semantics

A `drop` MUST persist the event row with its idempotency key, `webhook_id`, and routing trace, spending the delivery's `(source, external_id)` dedup slot. It MUST NOT create a todo, publish to the hub, ring a doorbell, or notify. The receiver MUST answer HTTP 202 with `{"todos": [], "created": 0, "dropped": true, …}` and MUST NOT echo the trace to the producer. A redelivery of a dropped event MUST stay dropped even if the rules have since changed. A dropped delivery MUST remain visible in event history.

#### Scenario: Noise is auditable but silent

- **WHEN** a rule matching a noisy event kind drops it
- **THEN** the event appears in the endpoint's event history with its routing trace, no todo exists, and no channel notification fires

#### Scenario: A dropped delivery stays dropped

- **WHEN** a dropped event is redelivered after the owner removed the rule that dropped it
- **THEN** no todo is created and no second event row is written

### Requirement: Routing Trace

Every routed event MUST record how it was routed:

```
{stage: "rule" | "default", cause?, rule_index?, rule_id?, rule_name?, action, faults?: [{rule_index, rule_id?, cause, detail?}]}
```

- `cause` applies to the `default` stage and is one of `no_match_default`, `rule_not_granted`, or `default_not_granted`.
- A sandbox-level fault uses `rule_index: -1`.

The trace MUST be written atomically with the event (`events.routing_trace`) and with each todo the delivery produced (`todos.routing_trace`). It MUST be visible in event history (`list_webhook_events`, `get_webhook_event`) and on todos returned by the todo verbs (`routing`).

#### Scenario: Every todo explains itself

- **WHEN** a human or agent inspects a todo produced by a routed delivery
- **THEN** the routing trace names the rule (or the default and its cause) that put it in that queue

### Requirement: Rule Management Tools

The agent surface MUST expose `list_webhook_rules`, `set_webhook_rules`, `add_webhook_rule`, `update_webhook_rule`, `move_webhook_rule`, and `remove_webhook_rule` as members of the webhook verb family ([SPEC-0006](../agent-tools/spec.md)).

**Authorization and error codes.**

- Every verb is gated by the endpoint's verb allowlist.
- Every verb requires the calling endpoint's **human** to own the webhook. Unknown, malformed, and another human's webhook ids MUST all return `not_found`, and the three MUST be indistinguishable.
- Validation failures MUST surface routing's code verbatim (`invalid_expression`, `forbidden_function`, `invalid_rule`, `too_many_rules`), except `not_granted`, which MUST surface as `forbidden`.
- `update_webhook_rule` and `move_webhook_rule` on an unknown rule id MUST return `rule_not_found`. `remove_webhook_rule` on an absent rule MUST succeed.

**Behavior.**

- Rule ids MUST be minted when omitted.
- Every mutation MUST be a read-modify-validate-write under a row lock.
- Every result MUST return the full rule list, the default, and the current grant (`queues`, `endpoints`).

#### Scenario: Another human's webhook is opaque

- **WHEN** an agent calls any rule verb naming a webhook owned by another human
- **THEN** the server responds `not_found`, and that human's rules are neither revealed nor changed

### Requirement: Routing Dry-Run

The surface MUST expose `test_webhook_rules`. It routes, without persisting anything, exactly one of:

- a sample `payload`, with optional `headers`, treated as a delivery that passed the webhook's verification;
- the `event_id` of an event that arrived **on that webhook**.

It MUST use the saved rules, or candidate `rules` plus an optional candidate `default_action`. Candidates MUST be validated exactly as a save and MUST NOT be saved. The dry-run MUST use the same router as the receiver.

It MUST return the `decision` (`drop`, `queue`, `endpoints`), the `trace`, and the evaluated `envelope`, unless `omit_envelope` is set. An `event_id` from any other webhook MUST return `not_found`. Supplying both or neither input MUST return `invalid_argument`.

#### Scenario: Candidate rules are tried, not saved

- **WHEN** an owner dry-runs candidate rules against a stored event of the webhook
- **THEN** the decision reflects the candidates and `list_webhook_rules` still returns the saved list

#### Scenario: Another webhook's delivery is unreachable

- **WHEN** an owner names, in a dry-run of their webhook, an event id recorded on a different tenant's webhook
- **THEN** the server responds `not_found` and returns nothing of that event

### Requirement: LLM Triage Stage (Opt-In, Bounded)

**Phase 2 — not yet implemented.** This requirement stands as specified. No implemented code path invokes a model today.

An endpoint MAY configure an LLM triage stage. When configured and no deterministic rule matches, the router MUST be invoked with a compact projection of the event plus the endpoint's granted queues with owner-written descriptions:

- The projection covers source, kind, title, size, actor, and routing-relevant fields. Full payload text is included only if the endpoint explicitly enables it.
- The router MUST return a structured answer of `{queue, confidence, reason}`.
- The router MUST be constrained to answering with a queue in the endpoint's granted set. Any other answer MUST be treated as an error.

The stage MUST fall back to the webhook's default action when the answer's confidence is below the endpoint's configured threshold, on timeout, on budget exhaustion, or on any error. The `llm_triage` config itself MUST be validated at save time with the same granted-set constraint as rule actions: a triage queue outside the endpoint's granted set MUST be rejected when saved.

#### Scenario: Hallucinated queue cannot route

- **WHEN** the model returns a queue name outside the endpoint's granted set
- **THEN** the answer is discarded as an error, the fallback default applies, and the routing trace records the rejected answer and reason

#### Scenario: Low confidence falls back

- **WHEN** the model's confidence is below the endpoint's threshold
- **THEN** the event takes the default action and the routing trace records the model's answer and the threshold that rejected it

### Requirement: Cost and Latency Bounds

**Phase 2 — not yet implemented** (applies to the LLM stage; the deterministic stage's bounds are specified above).

LLM triage MUST enforce a hard per-call timeout and per-endpoint budgets: events triaged per minute and per day, configured by the endpoint owner, with operator-level ceilings. When a budget is exhausted, unmatched events MUST take the default action without a model call until the budget window resets. Deterministic rules MUST NOT consume model budget and MUST NOT be subject to model availability.

#### Scenario: Budget exhaustion degrades to default

- **WHEN** the endpoint's daily triage budget is spent and an unmatched event arrives
- **THEN** the event takes the default action immediately and the routing trace records `budget_exhausted`

### Requirement: Isolation and Tenant Safety

A routing rule, or any evaluation result, MUST NOT be able to:

- target a queue outside the grant, or an endpoint that is not one of the webhook's live delivery targets;
- modify the delivery's trust mode or verified flag;
- alter the derived idempotency key;
- access another endpoint's events or queues.

This MUST hold at save time and again at every delivery, including for a rule row that bypassed save-time validation. Rule expressions MUST evaluate against the envelope alone, with no external effects.

#### Scenario: Cross-tenant routing is impossible

- **WHEN** a rule, or a default, names an endpoint owned by another human that is not a delivery target of the webhook
- **THEN** the save is refused; and if such a rule were stored anyway, a matching delivery lands only on the webhook's own targets, with cause `rule_not_granted`

## Security Requirements

- **Expression sandboxing.** Expressions compile with no environment loader, no input iterator, no module loader, and no custom functions. The following are refused at save time: `env`, `$ENV`, `input`, `inputs`, `input_filename`, `debug`, `stderr`, `halt`, `halt_error`, `now`, `localtime`, `strflocaltime`, and `import`/`include`. Undefined variables (e.g. `$__loc__`) fail to compile.
- **Out-of-process evaluation.** Production evaluation MUST run in a child process, not in the server process, because in-process jq cannot bound memory:
  - The child is the same binary, re-executed.
  - Its environment is empty: no inherited DSN, OAuth secrets, or keys.
  - Its stderr is discarded.
  - A watchdog exits it when runtime-mapped memory exceeds its limit (default 128 MiB).
  - The parent MUST kill it at a hard deadline (event budget + 750 ms).
  - At most two children run concurrently. A delivery that cannot get a slot within one second MUST route by default with a `sandbox_busy` fault.
  - Any child failure MUST route by default with a `sandbox_failure` fault.
  - The child MUST return only the index of the matching rule and any faults, behind a protocol prefix. The parent MUST apply the action from its own configuration and grant, and MUST ignore an out-of-range index.
  - A webhook with no rules MUST NOT start a child.
- **Egress control** *(Phase 2)*: enabling LLM triage is an explicit endpoint-owner decision naming a provider and model (via the runtime provider registry); full payload text is off by default and its enablement is recorded on the endpoint.
- **No privilege escalation.** Routing never changes trust mode, verification results, or ownership. Dropped events keep their audit record.
- **Injection resistance.** Event content, including producer-controlled fields, is data inside the jq evaluation and any future LLM prompt. The trace is not echoed to producers.

## Accessibility Requirements

Not applicable — no new UI is introduced; rule management surfaces through the MCP tools.
