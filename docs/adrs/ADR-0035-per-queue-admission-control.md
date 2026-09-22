---
status: proposed
date: 2026-09-22
decision-makers: Joe Stump
extends: [ADR-0007, ADR-0022]
related: [ADR-0002, ADR-0013, ADR-0027, ADR-0028, ADR-0029]
---

# ADR-0035: Per-Queue Admission Control — Budgets Counted in Postgres and Enforced Inside the Claim

## Context and Problem Statement

Switchboard already makes dispatch *correct*. `claim_next` picks a row with `FOR UPDATE SKIP LOCKED`, so concurrent workers never take the same todo ([ADR-0002](ADR-0002-postgres-persistence-and-retention.md)). `max_attempts` bounds how many times one todo is retried, the lease reaper recovers work from a crashed worker, and ingest dedupes redeliveries on the webhook and delivery id ([ADR-0007](ADR-0007-todos-as-core-primitive.md)). Nothing bounds *volume*. No setting says "this queue may start at most N pieces of work per day" or "at most K at once". #160 lists per-consumer concurrency limits and queue-depth backpressure as deliberately deferred "until there is a problem to point at".

There is now a problem to point at. A self-hosting customer's agent-operations plan specifies an investigation budget as an acceptance target: 100 agent investigations per day, **counted durably and enforced before dispatch**, where shared dispatchers, concurrent requests and retries must not multiply the allowance, with the counter's scope and day boundary recorded. Exhausting the budget must keep the work and show the backlog and the next action; it must not drop work or silently raise the ceiling. A budget-read failure must not bypass the budget. The customer built this themselves, and their own plan notes that a budget helper or a count read alone does not prove safe admission under concurrency. They are right, and they had to be right on their own because Switchboard offered nothing.

Our own fleet hit the consumer side of the same problem on 2026-09-14 ([ADR-0028](ADR-0028-prometheus-metrics-endpoint.md)): a provider's weekly quota emptied and every worker stopped at once. A spend ceiling existed only at the model provider, where it fails as an outage instead of a deferral.

The obvious implementations each fail one of the customer's clauses:

* **Count, then claim.** Under Postgres `READ COMMITTED`, two workers that both read "99 claimed today" both claim, and the day ends at 101. That is the customer's "a count read alone" failure.
* **A limit in each worker or each Harness.** Ten workers with a limit of ten each is a limit of a hundred. Shared dispatchers multiply it.
* **An in-memory token bucket.** It resets on restart and is per replica, so it is neither durable nor shared.
* **Charging each todo once.** A todo can be claimed up to `max_attempts` times (default 5), plus lease takeovers. Charging it once lets retries multiply real dispatches by up to five.

**How should Switchboard bound how much work a queue dispatches, per window and at once, so that the bound holds across concurrent claimers, replicas, restarts and retries, and is exceeded by nothing, while over-budget work is kept and explained rather than dropped?**

## Decision Drivers

* **Enforce where dispatch happens.** A todo becomes work only when it is claimed. Doorbells and notify hooks are hints ([ADR-0013](ADR-0013-channels-push-delivery.md), [ADR-0029](ADR-0029-outbound-todo-webhooks.md)). The claim is the one place every consumer passes through, whatever wakes it.
* **Atomic with the claim.** The check, the charge and the claim commit in one transaction, or none of them does.
* **Nothing multiplies it.** Not concurrent claimers, not a second replica, not retries, not lease takeovers, not a restart.
* **Durable.** Counters live in Postgres beside the todos they count.
* **Never drop.** Over-budget work stays `pending`, keeps its dedup slot, spends no attempt and never dead-letters.
* **Legible.** A deferred todo says why and when it becomes eligible, and so do the claim response, the board and the metrics.
* **Fail closed.** If admission cannot be evaluated, the claim is refused, never waved through.
* **An agent cannot raise its own ceiling.** The budget is a control on agents, so agents may read it and never write it.
* **Multi-tenant by construction.** A budget belongs to exactly one owner scope and governs only that owner's queue ([ADR-0022](ADR-0022-endpoint-scoped-todo-ownership.md); Teams, ADR-0038). There is no instance-wide budget a user could reach.
* **Zero change without a policy.** A queue with no policy behaves exactly as it does today, and pays at most one indexed lookup for the feature.

## Considered Options

* **(A) Per-queue admission policies enforced inside the claim transaction**, with a locked policy row and a durable per-window counter. *(chosen)*
* **(B) Consumer-side budgets only**, in Harness (`max_runs_per_day`, `max_concurrent`; Harness ADR-0027) or in each worker.
* **(C) Admission derived at claim time from `todos.claimed_at`**, with no lock and no counter table.
* **(D) Admission at ingest**: refuse or drop deliveries once a queue is over budget.
* **(E) An advisory-lock or in-memory token bucket in the server process.**

## Decision Outcome

Chosen option: **"(A) Per-queue admission policies enforced inside the claim transaction"**, because it is the only option in which the bound is a property of the database commit rather than of the callers' good behaviour. Every clause of the requirement then holds by construction: durable, enforced before dispatch, and not multiplied by concurrency, replicas or retries.

> **A budget that is checked anywhere other than inside the claim is a suggestion.**

### The policy

A policy attaches to one queue, as a queue is identified under Teams (ADR-0038): `(endpoint_id, queue)` for an endpoint's queue today, and `(team_id, queue)` for a team queue once ADR-0038 ships. It carries:

* `max_in_flight`: the most todos of the queue that may be `claimed` at once;
* `max_claims_per_window`: the most claims the queue may admit in one window;
* `window`: a period (`hour`, `day` or `week`), an IANA time zone, and a start (`HH:MM`, plus a weekday for `week`).

At least one limit is required. A limit of `0` is an explicit pause: the queue admits nothing, and says so. That is the kill switch the customer's plan asks for.

The endpoint's owner scope configures an endpoint-queue policy. For a team queue, the team role that ADR-0038 lets configure limits (its admins) does. Policies are set on the web UI, the operator API and the operator CLI, never by a vended endpoint credential. Agents see the policy that governs them on every claim response and through a read-only verb. Every change is audited: who, when, before and after.

Several sessions on one endpoint are competing consumers of one endpoint queue, as each fleet lane is, so they share one budget. A human who wants one budget across *several* endpoints drains a shared team queue (ADR-0038). Separate endpoint queues hold separate todos by design: a fan-out copy on another endpoint is another piece of work, governed by that endpoint's owner.

### What is charged: every committed claim, never refunded

One unit of the window budget is charged for **every committed claim**:

* the first claim;
* a claim after a retry's backoff elapses;
* a claim after an operator's "Retry now";
* a claim that takes over a lapsed lease.

Nothing refunds a unit: not `complete`, not `fail`, not `release`, not the reaper, and not a policy edit. A claim that admission refuses charges nothing. An ingest redelivery that dedupes onto an existing todo is not a claim and is never charged, so "transport retries are not new attempts" holds at the layer that already enforces it.

**A retry counts.** This is deliberate, and it is the decision the customer's wording forces. A claim is a dispatch, and a dispatch is what costs: a worker starts, a model runs, and the lapsed worker in a takeover may still be running too. If each todo were charged once, the real number of dispatches in a window would be up to `max_attempts` times the budget, which is exactly the multiplication the requirement forbids. Charging per claim makes the worst case equal the limit, whatever the number of workers, attempts or takeovers. It also makes the counter auditable: charged units equal committed claims, the same event `switchboard_todos_claimed_total` counts (SPEC-0023 REQ-3). Attempt history (ADR-0039) records one attempt per committed claim, so the two agree on what an attempt is. No refunds, because a refund on `release` would let a worker loop claim, spend and release forever at no cost.

The cost is that a flaky todo spends more than one unit. That is visible in `switchboard_todo_attempts_total`, and the owner sizes the budget as investigations times expected attempts. We prefer an honest meter that sometimes surprises the owner to one that undercounts spend.

### How it is enforced: one transaction, one lock order

A claim on a queue with a policy runs as a single transaction:

1. Pick the candidate row as today (`FOR UPDATE SKIP LOCKED` for `claim_next`, `FOR UPDATE` for `claim`). `claim_next` first leaves out any queue that a cheap, unlocked read already shows is exhausted.
2. Lock the queue's policy row, `FOR UPDATE`, waiting if needed. This serializes admissions for that queue and nothing else.
3. Under that lock, read `in_flight` (the queue's `claimed` todos, not counting the candidate itself when the claim takes over its lapsed lease) and the current window's counter.
4. If either limit is reached, roll back, record the refusal, and let `claim_next` go on to its other granted queues.
5. Otherwise claim the todo and upsert the window counter by one, then commit.

The lock order is always the todo row, then the policy row, and never the reverse. Candidate picks use `SKIP LOCKED` and never wait while holding a policy lock, and a refused queue's locks are released before `claim_next` tries the next queue, so no transaction ever holds two policy locks. Together these rule out deadlock. The bound holds across replicas because the lock and the counter are both in Postgres. A queue with no policy never locks or writes an admission row; it pays one indexed lookup that finds nothing.

The in-flight figure is read from `todos` under the lock, not kept as a counter. Every transition that raises it (a claim, or a takeover, which is a claim) takes the lock. Transitions that lower it (`complete`, `fail`, `release`, the reaper) can only make the read more permissive. So there is no drift to reconcile. The window count is kept as a counter because claims age out of `todos` through retention, and the count must survive them.

### Windows are fixed, declared, and zoned

Windows are fixed calendar windows: "a day starting 00:00 in `America/Los_Angeles`". They are not a rolling 24 hours. The customer asks for the day boundary to be *recorded*. With a fixed window, "resets at" is a single instant that the board, the claim response and a sleeping worker can all use. With a rolling window it is a moving target that needs a per-claim log. The counter row is keyed by `(policy, window_start)`. Boundaries are computed on local wall-clock time in an embedded zone database, with the same rules as presence shifts (SPEC-0022), so a daylight-saving day is 23 or 25 hours long and never skipped or repeated.

A fixed window allows a burst at the boundary: a full budget spent at 23:59, then another at 00:00. `max_in_flight` bounds that burst, and the two limits are meant to be used together.

### Deferred work stays pending and says why

Over-budget todos stay in `pending`. There is no new state, because a new state would break every consumer's state machine and every existing count by state (SPEC-0023 REQ-2). A deferred todo's `attempt`, `max_attempts` and `next_retry_at` are untouched. It never dead-letters for being deferred, keeps its dedup slot, and retention never prunes it.

Its admission status is derived when it is read, never stored, so it can't go stale: `state` (`admitted` or `deferred`), `reason` (`window_exhausted`, `in_flight_full`, `paused` or `admission_unavailable`) and `next_eligible_at`. `next_eligible_at` is exact for a window (the next boundary). For in-flight it is an upper bound (the earliest live lease expiry), because a slot usually frees sooner when a worker completes.

The status appears in four places:

* **`claim_next`**: it skips deferred queues and keeps scanning the others. When nothing is claimable but something is deferred, it answers `empty: true` together with a `deferred` list of queue, reason, pending count and `next_eligible_at`. A worker can sleep until that time instead of polling, and `empty: true` alone keeps meaning "no work".
* **`claim`** of a specific todo in a deferred queue: it returns a new error code, `deferred`, carrying the same detail.
* **A successful claim on a budgeted queue**: it returns the remaining headroom.
* **`list_todos`, the todo drawer and the board**: they show the status on each pending todo, and the queue header shows used, limit and "resets at".

### Doorbells and hooks wait for admission

Admission is enforced before dispatch, and a dispatcher is part of dispatch. So Switchboard does not ring a doorbell for a todo in a deferred queue: not on creation, not on attach, and not on the doorbell heartbeat. It does not call a notify hook for one either (ADR-0029; SPEC-0024). A Harness trigger (Harness ADR-0021) is therefore never woken to start a process that can't claim anything. When the queue reopens, it rings once per endpoint and queue that has push-eligible pending work, subject to presence ([ADR-0027](ADR-0027-endpoint-presence-clock-in-clock-out.md)). A queue reopens at a window boundary, when an in-flight slot frees, or when the policy is raised or removed. The reopen sweep is a background sweep, exempt from endpoint scoping on the same terms as the reaper.

### Fail closed

If the policy for a candidate's queue can't be read or evaluated, the claim is refused with reason `admission_unavailable`. That covers a database error and a stored zone that the embedded database no longer knows. The todo stays pending, the failure is counted and shown on the board, and nothing is admitted on a guess. The two errors are not symmetric: a wrong refusal costs latency, and a wrong admission is the budget breach the feature exists to prevent.

### Metrics without tenant labels

The metrics follow SPEC-0023's cardinality and absence rules (REQ-5, REQ-6). There is no owner, endpoint or team label, and the queue label is capped with `__other__`. The `/metrics` surface is the operator's, so it carries aggregates that mean something when summed across tenants:

* policies by status;
* deferred todos by reason;
* units charged;
* refusals by reason.

Per-queue usage and limits are tenant data, so they live on the board and the API, visible to the owner scope only.

### Consequences

* Good, because the customer's clauses hold by construction. The limit is exceeded by nothing, because the database commit is the check.
* Good, because it covers every consumer: Crush, Claude Code, Harness one-shots, relay attempts and hand-written scripts alike, and none of them has to cooperate.
* Good, because deferred work is retained and explained on every surface. "Why isn't my agent working?" has an answer with a time on it.
* Good, because doorbells and hooks stop waking consumers for work they can't claim, which saves model turns on exactly the queues that are trying to save them.
* Good, because a limit of `0` gives each queue a pause switch that holds across every worker at once.
* Bad, because claims on a budgeted queue serialize on one row lock. At the target scale (hundreds a day) this is noise. A queue claiming thousands per minute would feel it, and should use `max_in_flight` alone with a separately tuned policy, or none.
* Bad, because charging retries means a flaky todo spends budget, which some owners won't expect. Mitigated by the docs, the attempts metric and the headroom on every claim response.
* Bad, because there is now one more reason a todo can sit pending. Mitigated by making the reason explicit wherever a todo is shown.
* Bad, because fixed windows allow a boundary burst. Mitigated by `max_in_flight`.
* Neutral: Harness budgets (Harness ADR-0027) stay useful for a single harness's cost and usage-limit backoff. This ADR bounds the queue across all consumers. The two compose rather than overlap.

### Confirmation

* With a window limit of 10 and 50 concurrent `claim_next` callers spread over two server instances, exactly 10 claims commit and 40 callers see `deferred`.
* A todo that fails, waits out its backoff and is claimed again is charged twice. A takeover of a lapsed lease is charged, and a `release` refunds nothing.
* With the window exhausted, todos stay `pending` with `attempt` unchanged, are never dead-lettered, and report `reason: window_exhausted` with `next_eligible_at` at the declared boundary.
* At the boundary, the queue reopens, exactly one doorbell rings per endpoint and queue with pending work, and no doorbell or notify hook fired while it was deferred.
* With the policy table unreadable, claims on that queue are refused with `admission_unavailable`, and claims on queues without a policy are unaffected.
* Two endpoints of different owners that both drain a queue named `investigations` have independent counters, and neither owner can read the other's.
* A vended endpoint credential can't create, change or delete a policy.

## Pros and Cons of the Options

### (A) Per-queue admission policies enforced inside the claim transaction

* Good, because the check and the charge are one commit, so concurrency, replicas and retries can't multiply the limit.
* Good, because it needs no cooperation from consumers and holds for every claim path, including the operator's board claim.
* Good, because queues without a policy take no lock and pay only one indexed lookup.
* Bad, because it adds a lock to the claim path of budgeted queues, and a table, a migration and a reopen sweep.
* Bad, because the claim transaction grows more complex. `claim_next` has to skip exhausted queues without starving the others.

### (B) Consumer-side budgets only

* Good, because it is already being designed for Harness (Harness ADR-0027), costs Switchboard nothing, and can see provider usage data that Switchboard can't.
* Bad, because shared dispatchers multiply it: every harness and every worker carries its own allowance.
* Bad, because it covers only consumers that implement it. A hand-written worker or a second Harness host bypasses it.
* Bad, because it can't keep a doorbell or a hook from waking a consumer for work it will refuse.

### (C) Admission derived from `todos.claimed_at`, without a lock

* Good, because there is no new table: count the claims in the window and compare.
* Bad, because it is the count-then-claim race. Two claimers read the same count and both commit.
* Bad, because retention deletes terminal todos, so the count shrinks as history is pruned. On a long window, or with aggressive retention, the budget would silently grow.

### (D) Admission at ingest

* Good, because it stops work before it becomes a todo and needs no change to the claim path.
* Bad, because it drops work or refuses deliveries, which the requirement forbids ("never drop").
* Bad, because ingest is the wrong layer: a todo created within budget can still be retried five times, so dispatches aren't bounded.
* Bad, because a refused delivery pushes the backlog into the producer's retry logic, where no one can see it.

### (E) Advisory lock or in-memory token bucket

* Good, because it is fast and simple in a single process.
* Bad, because an in-memory bucket resets on restart and is per replica, so it is neither durable nor shared.
* Bad, because an advisory lock still needs a durable counter to be correct, and then it is option A with a less inspectable lock.

## Architecture Diagram

```mermaid
sequenceDiagram
  participant W as worker (any client)
  participant S as switchboard claim path
  participant T as todos row
  participant P as admission policy row
  participant C as window counter

  W->>S: claim_next(queues)
  S->>S: unlocked read: drop queues already exhausted
  S->>T: pick oldest claimable, FOR UPDATE SKIP LOCKED
  alt queue has no policy
    S->>T: claim (attempt+1, lease)
  else queue has a policy
    S->>P: SELECT ... FOR UPDATE (serializes this queue only)
    S->>T: count claimed in (scope, queue), excluding takeover candidate
    S->>C: read (policy, window_start)
    alt within max_in_flight and max_claims_per_window
      S->>T: claim (attempt+1, lease)
      S->>C: upsert used = used + 1
    else over a limit, or policy unreadable
      S-->>S: roll back; record refusal (window_exhausted, in_flight_full, paused, admission_unavailable)
      S->>T: try the next granted queue, or give up
    end
  end
  S-->>W: todo with headroom, or empty with deferred queues and next_eligible_at
  Note over S,C: Doorbells and notify hooks are withheld while a queue is deferred,<br/>and ring once when it reopens (window boundary, freed slot, raised policy).
```

## More Information

* **Composes with the other products.**
  * **Harness:** budgets and usage-limit backoff (Harness ADR-0027) cap one harness. When `claim_next` answers `deferred`, Harness should park until `next_eligible_at` rather than poll or count it as a failure. Relay attempts (Harness ADR-0025; attempt history, ADR-0039) claim once per attempt, so each attempt is charged, which is what bounds a relay loop's spend.
  * **Cairn:** no change. The queue digest (ADR-0034) reports budget-deferred work with its reasons, which is the "budget-deferred work" line the customer's daily sweep asks for.
* **Parallel records.** Teams and the queue-identity contract are in ADR-0038 / SPEC-0033. Attempt history is in ADR-0039 / SPEC-0034. The notify-hook spec is SPEC-0024. Notification sinks and the digest are in ADR-0034 / SPEC-0029. They are cited here in prose and become front-matter edges once they merge.
* **Out of scope.** Operator-imposed ceilings on a tenant (for example, an instance maximum of claims per owner per day) are a different control. ADR-0038 lets the operator bound tenant usage but not configure it, and that belongs in its own record. Priority and fair share across queues stay deferred, as #160 says.
* **Why not a new todo state.** `deferred` is a property of the queue at a moment, not of the todo. The same todo is deferred at 23:59 and claimable at 00:00 with nothing written to it. Storing it would mean a sweep rewriting rows at every boundary, and states that disagree with the policy between sweeps.
* Implementation: [SPEC-0030](../openspec/specs/admission-control/spec.md).
