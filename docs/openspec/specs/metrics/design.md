---
status: draft
date: 2026-09-15
implements: [ADR-0027]
---

# Design: Prometheus Metrics

## Shape

One collector package, registered on a dedicated registry rather than the default
global one, so a stray `prometheus.MustRegister` elsewhere in the tree cannot
collide or panic at init.

Two families, deliberately implemented differently:

**Counters** are incremented inline at the events they describe — a delivery
verified, a todo claimed, a lease reaped. They live next to the code that already
knows the outcome, and they cost a lock-free atomic add.

**Gauges** (`switchboard_queue_todos`, `switchboard_queue_oldest_pending_seconds`)
are NOT incremented inline. They are computed at scrape time by a custom
collector that runs one aggregate query per scrape:

```sql
SELECT queue, state, count(*), max(now() - created_at) FILTER (WHERE state='pending')
FROM todos GROUP BY queue, state
```

Inline gauge maintenance would drift: every path that moves a todo between states
would have to remember to adjust, and a missed decrement is a permanently wrong
graph that looks plausible. A scrape-time query is authoritative by construction —
it cannot disagree with the table it reads.

## Why a scrape-time query is affordable

One grouped count per scrape, at a 15–60s interval, against an indexed
`(queue, state)` pair. If the todos table ever grows to where that is not free,
the fix is a partial index or a materialised summary — not inline counters, which
trade a real correctness property for a saving we have not yet needed.

The collector MUST apply a timeout shorter than the scrape interval and, on
timeout or error, omit the gauges and increment
`switchboard_metrics_collection_errors_total` (SPEC-0022 REQ-6). It MUST NOT
report stale or zero values, because a flat line at zero is exactly the reading
this whole surface exists to prevent.

## Zero-value series

Emitting every `(queue, state)` combination including zeros is a requirement, not
a stylistic choice. PromQL cannot alert on the absence of a series nearly as
cleanly as on a series equal to zero, and the condition being alerted is
literally `claimed == 0`. The collector therefore enumerates known queues from
the queue registry, not from whatever happens to appear in the count query.

## Auth

`/metrics` is handled before endpoint-scoped authorization, with its own check
against the dedicated scrape credential. It deliberately does not participate in the
vended-endpoint grant model: an agent endpoint's token authorizes queue work, and
fleet-wide operational counts are not queue work.

Returning `401` with no body keeps an unauthenticated prober from learning which
queues exist — queue names are operator-chosen and can be descriptive.

## Cardinality control

The store's known-queue enumeration is the source of truth for the label set:
every distinct queue name the store knows — queues todos have ridden plus queues
scoped onto vended endpoints. A cap (default 50)
is applied at collection; queues beyond it aggregate into `queue="__other__"`.
The cap and the overflow bucket exist because `queue` is operator-defined, and an
operator scripting queue-per-repo would otherwise turn a bounded label into an
unbounded one without ever being warned.

## Testing

* A test asserting `pending > 0, claimed == 0` produces the exact series a
  Grafana alert would read — the incident condition, pinned as a test.
* A test that every known queue reports all four states including zeros.
* A collector-failure test asserting the gauges are **omitted** and the error
  counter increments, rather than zeros being emitted.
* An auth test: unauthenticated `401`, vended-endpoint token `401`, scrape
  credential `200`.
* A cardinality test: more queues than the cap collapses the tail into
  `__other__` rather than emitting unbounded series.
