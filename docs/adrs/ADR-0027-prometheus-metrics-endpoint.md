---
status: accepted
date: 2026-09-15
decision-makers: Joe Stump
extends: [ADR-0022]
related: [ADR-0025]
---

# ADR-0027: Switchboard Exposes Prometheus Metrics, Led by Queue Liveness

## Context and Problem Statement

On 2026-09-14 every agent worker draining Switchboard's `forge` queue stopped at
the same moment: their shared model provider's weekly quota emptied. The queue
kept accepting deliveries perfectly — routing healthy, work orders minted,
`verified: true` — and nothing consumed them for roughly twenty hours. The
backlog reached fifty todos before a human asked a question that happened to
surface it.

Nothing in Switchboard was broken, and that is the point. Every signal
Switchboard emitted was green, because Switchboard's own health and *the queue
being served* are different properties and only the first was observable.

The condition was visible the whole time in one pair of numbers:

```
pending: 50   claimed: 0
```

`pending > 0` with `claimed == 0`, sustained, means **no live consumer**. It is
categorically different from slow workers or a routing bug. Nobody saw it
because reading it required calling `list_todos` and counting by hand.

Switchboard has no metrics surface at all: no `/metrics`, no Prometheus
dependency, no counters. The fleet already runs VictoriaMetrics and Grafana, and
already scrapes services that expose an endpoint. The question is what to expose
and under what auth posture — not whether to.

## Decision Drivers

* The 2026-09-14 outage was invisible for ~20h and would have been obvious on a
  graph. That specific condition must be a first-class metric, not something
  derived by an operator who already suspects it.
* Switchboard observes **claims, not workers**. A worker that never successfully
  claims is indistinguishable from no worker at all. Metrics must make that
  distinction visible from the queue side, because it cannot be made from the
  worker side by anything Switchboard controls.
* `/metrics` carries per-queue and per-endpoint information — who is draining
  what, how much work exists for whom. That is closer to operational data than
  to public telemetry.
* The fleet's existing AI gateway pins `require_auth_for_metrics_endpoint: true`
  for exactly this reason: its `/metrics` carries per-key spend. Consistency
  across the fleet matters more than convenience.
* Switchboard runs on a public-facing host. An unauthenticated endpoint there is
  a different proposition from one on an internal box.
* Lease expiry is a silent correctness problem, not just a performance one: a
  lapsed lease returns a todo to `pending` and a second worker redoes work the
  first is still doing. It is invisible in the tracker — one claim, one
  completion — and shows only as wasted compute.

## Considered Options

* **No metrics; keep using `list_todos` ad hoc.** Status quo. Rejected: it is
  what produced a twenty-hour blind spot, and it cannot alert.
* **Log-derived metrics via Loki.** Rejected: it infers state from event text
  rather than reading it, and gauges like "how many are pending right now" are
  exactly what logs model worst.
* **`/metrics` in Prometheus text format, authenticated.** Chosen.
* **Push to a gateway.** Rejected: Switchboard is a long-lived service, not a
  batch job; pull matches the fleet's existing scrape model.

## Decision Outcome

Switchboard exposes **`GET /metrics`** in Prometheus text format, served by
`prometheus/client_golang`'s `promhttp`, **requiring authentication** — the same
posture as the fleet's AI gateway, for the same reason.

The metric set is led by **queue liveness**, and the two gauges that would have
caught 2026-09-14 are mandatory:

```
switchboard_queue_todos{queue,state}      gauge    pending / claimed / done / failed
switchboard_queue_oldest_pending_seconds{queue}  gauge
```

`pending > 0 AND claimed == 0`, sustained, is then a Grafana alert expression
rather than a thing an operator has to think to check. The oldest-pending age is
the companion: a queue can have claims and still be falling behind, and the head
of the queue ageing is what says so.

Beyond liveness, the set covers the lifecycle a reader needs to tell *why* a
queue is not moving — deliveries arriving and their verdicts, routing decisions
including drops, claims and completions, and **lease expiries**, which are the
duplicate-work signal that is otherwise invisible.

SPEC-0022 defines the exact names, labels and types.

### Consequences

* Good: the failure that cost twenty hours becomes a graph and an alert, and
  satisfies the "alert on pending with zero claimed" action item raised in that
  incident's postmortem.
* Good: lease expiry becomes measurable, so duplicate work stops being invisible.
* Good: routing drops become countable, which makes a wrong jq rule — the kind
  that installs green and matches nothing — observable rather than silent.
* Bad: a new dependency (`prometheus/client_golang`) and a small always-on
  bookkeeping cost.
* Bad: cardinality risk. Labels are restricted to bounded sets (queue, state,
  provider, verdict). Endpoint and todo identifiers are deliberately **not**
  labels; per-endpoint detail belongs in the API, not in a time series.
* Neutral: authentication means the scrape config carries a credential, as it
  already does for the AI gateway.

## More Information

* Incident that motivated this: the `forge` queue undrained for ~20h on
  2026-09-14, with the model provider's quota — not Switchboard — as root cause.
* SPEC-0022 (metrics) defines the surface.
* The gauge pair above is the machine-readable form of the operator guidance in
  the queue-draining guide: before debugging why a queue is not draining, check
  whether anything is claiming at all.
