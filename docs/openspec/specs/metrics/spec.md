---
status: draft
date: 2026-09-15
implements: [ADR-0028]
related: [ADR-0022, ADR-0025]
---

# SPEC-0023: Prometheus Metrics

## Overview

Switchboard exposes a Prometheus text-format endpoint at `GET /metrics`, led by
the queue-liveness gauges that make "nothing is draining this queue" visible
without an operator thinking to look.

The organising idea is that **Switchboard observes claims, not workers**. It
cannot report whether a worker process is alive, and must not pretend to. What it
can report exactly is how much work exists, how much is held, how old the head of
the queue is, and how the lifecycle is moving. A reader combines those to
conclude "no live consumer" — a conclusion Switchboard supports rather than
asserts.

## Requirements

### REQ-1: The endpoint

`GET /metrics` MUST serve Prometheus text format (`text/plain; version=0.0.4`)
via `prometheus/client_golang`'s `promhttp` handler.

It MUST require authentication. An unauthenticated request MUST receive `401`
and MUST NOT disclose metric names or values. The accepted credential is a
dedicated scrape credential, presented as a bearer token. It is a new
credential class, not a reused one: the service recognises no static shared
credential today, operator surfaces authenticate per-human, and nothing
existing can authorize a scrape.

The scrape credential is configured as `SWITCHBOARD_METRICS_TOKEN` and MUST be
at least 32 bytes; a shorter value MUST fail configuration at startup. With the
variable unset, the endpoint MUST refuse every request: it is closed by default,
never open.

The endpoint MUST NOT be part of any endpoint-scoped grant: it is an operator
surface, not an agent surface. A vended endpoint credential MUST NOT authorize it.

The Go collectors (`go_*`, `process_*`) MUST be registered, because process
restarts and memory growth are part of reading any of the rest.

### REQ-2: Queue liveness — the mandatory pair

```
switchboard_queue_todos{queue,state}             gauge
switchboard_queue_oldest_pending_seconds{queue}  gauge
```

`state` is one of `pending`, `claimed`, `done`, `failed`. Every known queue MUST
be reported for every state, **including zero values** — an absent series is
indistinguishable from a series that is genuinely zero, and this pair exists
precisely to make a zero legible.

The known queues are every queue a todo has ridden plus every queue scoped on an
endpoint, so a queue that has no todos yet still reports four zero-valued series.
Only these four states are reported. Todos in the A2A states (`canceled`,
`rejected`, `input-required`, `auth-required`) are not counted; extending `state`
to cover them is a possible later addition, not a requirement of this spec.

`switchboard_queue_oldest_pending_seconds` MUST report the age of the oldest
`pending` todo, or `0` when none is pending.

These two satisfy the incident condition directly:

```promql
switchboard_queue_todos{state="pending"} > 0
  and ignoring(state) switchboard_queue_todos{state="claimed"} == 0
```

sustained over a window longer than a normal claim gap.

### REQ-3: Lifecycle counters

```
switchboard_todos_created_total{queue,source}        counter
switchboard_todos_claimed_total{queue}               counter
switchboard_todos_completed_total{queue,outcome}     counter   # outcome: complete|fail
switchboard_todo_leases_expired_total{queue}         counter
switchboard_todo_attempts_total{queue,attempt_bucket} counter  # 1|2|3+
```

`switchboard_todo_leases_expired_total` is REQUIRED and is not a performance
metric. A lapsed lease returns a todo to `pending` and a second worker redoes
work the first is still doing; the tracker shows one claim and one completion,
so the duplication is otherwise invisible. Any non-zero rate here is a finding.

A lease expires by two paths, and both MUST count: the reaper finding a lapsed
lease, and a claim that takes over a lapsed lease before the reaper runs. The
takeover is the same duplicate-work signal, reached faster. A single lapse MUST
count once, whichever path reaches it first. A todo the reaper dead-letters at
its attempt cap counts as a lease expiry only, not also as `outcome="fail"`,
because no claimant reported a failure.

`source` MUST be bounded: the webhook source types Switchboard accepts, plus
`operator` (the push API), `dev`, and `friend` (friend handoffs). Any other value
is reported as `__other__`. A friend handoff's persona name is free text and MUST
NOT become a label.

### REQ-4: Ingest and routing

```
switchboard_webhook_deliveries_total{provider,trust_mode,verdict} counter
   # verdict: accepted|rejected|dropped
switchboard_routing_decisions_total{webhook,rule_id,action}       counter
   # action: queue|drop
switchboard_webhook_verify_failures_total{provider,reason}        counter
```

`switchboard_routing_decisions_total` MUST count **drops** as well as routes. A
jq rule that matches nothing installs green and behaves identically to a rule
that was never added; a counter stuck at zero is how that becomes visible.

`rule_id` is a server-minted, stable, opaque id (`rule_<24 hex>`), not
operator-chosen; at most 32 exist per webhook, so it is acceptable as a label.
`webhook` is likewise server-minted but unbounded across the fleet, so it is
held to REQ-5: distinct values MUST be capped, with overflow aggregated under
`webhook="__other__"`. Rule *names* MUST NOT be used — they are free text.

Webhooks arrive only at `POST /webhooks/w/{token}`. Each delivery MUST count
exactly one `verdict`: `accepted` when the event persisted (an idempotent
redelivery included), `dropped` when routing dropped it, and `rejected`
otherwise. A refusal the sender caused records exactly one `reason`. A refusal
the server caused, such as a store error or no deliverable target, records none,
so `rejected` minus `verify_failures` is the server's own share. `reason` is one
of `missing_signature`, `malformed_signature`, `stale_timestamp`,
`bad_signature`, `malformed`, `event_id_mismatch`, `too_large`, `unreadable`,
`unknown_webhook`, `not_configured`, `unsupported_source`, or `__other__`; the
client-facing rejection message is free text and MUST NOT become a label.

A routing decision is counted once per persisted delivery, not once per target.
A decision no rule made is reported as `rule_id="default"`.

### REQ-5: Cardinality

Labels MUST be drawn from bounded sets. The following MUST NOT appear as labels:
todo id, endpoint id, actor id, artifact handle, webhook secret or URL, or any
user-supplied tag. Per-item detail belongs in the API and the database.

`queue` is operator-defined and therefore unbounded in principle; implementations
MUST cap the number of distinct queue label values reported and MUST surface the
overflow as a single `queue="__other__"` series rather than growing without limit.

### REQ-6: Honest absence

A metric the service cannot currently compute MUST be omitted, never reported as
zero. A zero and an unmeasured value are indistinguishable once scraped, and this
whole spec exists because an unmeasured condition looked like a healthy one.

Where a collector fails, it MUST increment
`switchboard_metrics_collection_errors_total{collector}` so a broken collector is
itself visible rather than silently flattening a graph.

The error series MUST be present at `0` from the first scrape, so an alert on its
`increase()` works before anything has failed. Collectors are gathered
concurrently, so a failure's increment MAY first appear in the following scrape;
an alert on it SHOULD use a window of a few scrapes.

## Scenarios

### Scenario: the 2026-09-14 outage, as it would have appeared

Workers die on a provider quota. Deliveries keep arriving.

* `switchboard_queue_todos{queue="forge",state="pending"}` climbs 33 → 50
* `switchboard_queue_todos{queue="forge",state="claimed"}` sits at `0`
* `switchboard_queue_oldest_pending_seconds{queue="forge"}` climbs past 20 hours
* `switchboard_webhook_deliveries_total` keeps incrementing — proving ingest
  healthy and isolating the fault to consumption

The alert fires within minutes instead of a human noticing the next morning.

### Scenario: duplicate work from a lapsed lease

A worker claims a todo and does not heartbeat through a long operation.

* `switchboard_todo_leases_expired_total{queue}` increments
* `switchboard_todos_claimed_total{queue}` exceeds
  `switchboard_todos_completed_total{queue}` by a growing margin
* `switchboard_todo_attempts_total{attempt_bucket="2"}` increments

None of this is visible in the tracker today.

### Scenario: a routing rule that matches nothing

An operator adds a jq drop rule with a wrong event-kind string.

* `switchboard_routing_decisions_total{rule_id="…",action="drop"}` stays at zero
* the queue it was meant to protect keeps growing

A rule that works and a rule that is silently wrong are otherwise identical from
outside.

## Out of Scope

* Worker process health. Switchboard cannot see it; the harness that supervises
  workers reports its own.
* Per-todo timing histograms. Deferred until the gauges above are in use.
* Tracing. Cairn owns trace capture.
