---
status: draft
date: 2026-09-06
implements: [ADR-0024]
related: [SPEC-0001, SPEC-0007]
---

# SPEC-0020: Event Routing

## Graph Edges

- **Implements:** [ADR-0024](../../../adrs/ADR-0024-event-routing-deterministic-and-llm.md) — deterministic jq routing rules with an optional bounded LLM triage stage
- **Related:** [SPEC-0001](../webhook-ingestion/spec.md) — the push-ingestion pipeline routing slots into; [SPEC-0007](../persistence/spec.md) — todo durability contract routing must preserve

## Overview

Event routing is the stage of webhook ingestion that decides, per verified delivery, **where (or whether)** the resulting todo lands. Each self-managed webhook carries an ordered list of routing rules — jq-style filter expressions over the normalized event with actions `queue` or `drop` — evaluated first-match-wins, terminated by an explicit default. Endpoints may additionally opt into an LLM triage stage that classifies events no rule matched, constrained to the endpoint's granted queues and a per-endpoint budget. Routing runs after verification and idempotency-key derivation and before todo creation; the dedup contract is untouched by any routing outcome, including drop.

## Requirements

### Requirement: Deterministic Rule Evaluation

Each webhook MUST evaluate an ordered list of routing rules, first-match-wins, terminated by a default action. A rule consists of a jq-style filter expression evaluated against the normalized event JSON (the persisted event envelope, including `source`, `kind`, and delivery payload fields) and an action of `queue` (a named queue) or `drop`. Evaluation MUST be pure and deterministic: the same event and rule list MUST always produce the same route. Expression evaluation MUST be sandboxed — no filesystem, network, or process access — and a per-event evaluation timeout MUST apply, treating a timeout as no-match.

#### Scenario: First match wins

- **WHEN** two rules both match an event
- **THEN** the earlier rule's action applies and the later rule is not evaluated

#### Scenario: Deterministic replay

- **WHEN** the same delivery is ingested twice with the same rule list
- **THEN** both evaluations produce the same route (modulo the dedup contract, which still collapses them to one todo)

### Requirement: Rule Validation at Save Time

Rule expressions MUST be compiled when the webhook's rule list is saved. An expression that does not compile, or an action naming a queue outside the owning endpoint's granted set, MUST be rejected with an error naming the offending rule. A webhook MUST always have exactly one default action (its target queue unless configured otherwise), so an unmatched event has a defined destination.

#### Scenario: Typo in a rule is rejected, not silently inert

- **WHEN** an owner saves a rule whose expression does not compile
- **THEN** the save fails naming the rule, and the webhook's previous rule list remains in force

### Requirement: Drop Action Semantics

A `drop` action MUST consume the delivery's dedup slot (the event row persists with its idempotency key and a routing trace of the matched rule) but MUST NOT create a todo, ring a doorbell, or notify. A dropped delivery MUST remain visible in the endpoint's event history so noise stays auditable.

#### Scenario: Noise is auditable but silent

- **WHEN** a rule matching a noisy event kind drops it
- **THEN** the event appears in the endpoint's event history with its routing trace, no todo exists, and no channel notification fires

### Requirement: LLM Triage Stage (Opt-In, Bounded)

An endpoint MAY configure an LLM triage stage. When configured and no deterministic rule matches, the router MUST be invoked with a compact projection of the event (source, kind, title, size, actor, and routing-relevant fields — full payload text only if the endpoint explicitly enables it) plus the endpoint's granted queues with owner-written descriptions, and MUST return a structured answer of `{queue, confidence, reason}`. The router MUST be constrained to answering with a queue in the endpoint's granted set; any other answer MUST be treated as an error. The stage MUST fall back to the webhook's default action when the answer's confidence is below the endpoint's configured threshold, on timeout, on budget exhaustion, or on any error.

#### Scenario: Hallucinated queue cannot route

- **WHEN** the model returns a queue name outside the endpoint's granted set
- **THEN** the answer is discarded as an error, the fallback default applies, and the routing trace records the rejected answer and reason

#### Scenario: Low confidence falls back

- **WHEN** the model's confidence is below the endpoint's threshold
- **THEN** the event takes the default action and the routing trace records the model's answer and the threshold that rejected it

### Requirement: Cost and Latency Bounds

LLM triage MUST enforce a hard per-call timeout and per-endpoint budgets (events triaged per minute and per day, configured by the endpoint owner, with operator-level ceilings). When a budget is exhausted, unmatched events MUST take the default action without a model call until the budget window resets. Deterministic rules MUST NOT consume model budget and MUST NOT be subject to model availability.

#### Scenario: Budget exhaustion degrades to default

- **WHEN** the endpoint's daily triage budget is spent and an unmatched event arrives
- **THEN** the event takes the default action immediately and the routing trace records `budget_exhausted`

### Requirement: Routing Trace

Every routed event MUST record how it was routed: the matched rule's index and action, or the LLM decision (model, queue, confidence, reason) or the fallback cause (`no_match_default`, `low_confidence`, `timeout`, `error`, `budget_exhausted`, `hallucinated_queue`). The trace MUST be visible on the event in history and on any todo it produced.

#### Scenario: Every todo explains itself

- **WHEN** a human or agent inspects a todo produced by a routed delivery
- **THEN** the routing trace names the rule or model decision that put it in that queue

### Requirement: Isolation and Tenant Safety

A routing rule or LLM answer MUST NOT be able to target a queue outside the owning endpoint's granted set, modify the delivery's trust mode or verified flag, alter the derived idempotency key, or access another endpoint's events or queues. Rule expressions MUST evaluate against the event alone with no external effects.

#### Scenario: Cross-tenant routing is impossible

- **WHEN** a rule or model answer names a queue granted to a different endpoint
- **THEN** the route is rejected exactly as any non-granted queue would be

## Security Requirements

- **Expression sandboxing**: jq evaluation MUST run in a sandbox without I/O, network, or arbitrary function execution; evaluation is bounded by node-count and wall-clock limits.
- **Egress control**: enabling LLM triage is an explicit endpoint-owner decision naming a provider and model (via the runtime provider registry); full payload text is off by default and its enablement is recorded on the endpoint.
- **No privilege escalation**: routing never changes trust mode, verification results, or ownership; dropped events keep their audit record.
- **Injection resistance**: event content (including producer-controlled fields) is data inside the jq evaluation and the LLM prompt; the router's structured-output schema MUST be enforced by validation, never by prompt instruction alone.

## Accessibility Requirements

Not applicable — no new UI is introduced; rule and triage management surface through the existing MCP tools and operator board.
