---
status: proposed
date: 2026-10-15
decision-makers: Proposed by joestump-agent, pending Joe
extends: [ADR-0013, ADR-0029]
related: [ADR-0022, ADR-0027, ADR-0030, ADR-0034, ADR-0035]
---

# ADR-0045: Queue-Stall Nudge — A Level-Triggered Wake For A Queue Nobody Is Claiming From

**Status note:** this ADR is **proposed**. It ships with an implementation so the choice can be argued
against working code rather than prose; nothing here is settled until Joe accepts it. The behaviour is
recorded as [SPEC-0038](../openspec/specs/queue-stall-nudge/spec.md) in `draft`, and the spec edits it
implies for SPEC-0011 and SPEC-0024 are listed under Consequences rather than applied.

## Context and Problem Statement

Every wake in switchboard is an **edge**. A todo is created → the instance that committed it fires the
ready hook; a lease expires or a retry comes due → the reaper fires it when the row lands back in
`pending`; a session attaches → the attach catch-up rings it; a todo has sat unclaimed and this
instance has a session for its endpoint → the doorbell heartbeat rings it
([SPEC-0011](../openspec/specs/channels/spec.md) REQ "Doorbell Heartbeat"). Each of those is a
transition, and every one of them is also, in [ADR-0013](ADR-0013-channels-push-delivery.md)'s words,
a hint over a durable queue.

An outside report (#566, from the public mirror's stump-wtf/switchboard#29) found the hole in that
shape: **a queue can hold claimable work with no edge left to fire.** A consumer that deliberately
claims one todo per invocation — because a fixed external timeout means grabbing a second item late in
the budget gets it killed mid-task — spends its one notification, exits, and is started again only by
the next inbound webhook. Meanwhile the queue still has work in it, nothing upstream changes, and no
edge fires. The reporter's workaround was for the consumer to re-check `list_todos` before exiting and
re-trigger its own next invocation: every consumer reimplementing "is my queue drained", when
switchboard is the one component that actually knows.

The 2026-10-13 lane-m hand-back stopped on a real constraint, and it is the one this ADR has to design
around. The obvious implementation — fire `todo_ready` on claim and let the LISTEN loop's
`nudgeDoorbells` handle it — lands on the `doorbellGate`, which both the creation hook and that loop
share and which suppresses a given todo id for a minute. A wake fired on claim would be suppressed for
precisely the pending todos it was meant to announce. The hand-back read that as "needs a design record
before code". It was right about the record and wrong about the refusal: the missing decision is a lane's
to propose, so it is proposed here.

Two mechanisms were on the table in the thread: re-fire on claim, and a periodic "pending older than N
seconds with nobody claiming". **Which one, what the threshold is, and how the wake is deduped without
reusing the doorbell's gate** is the decision.

## Decision Drivers

* **The queue is the ledger.** A nudge is a hint. Losing one, or never configuring one, must cost
  latency and never work ([ADR-0013](ADR-0013-channels-push-delivery.md);
  [ADR-0029](ADR-0029-outbound-todo-webhooks.md)).
* **A wake costs a model turn.** switchboard has repeatedly learned this: `ringSweepLimit`, the
  round-robin over endpoints, and the 5-ring budget all exist because a sweep that wakes everything is
  a token bomb. A periodic wake needs a loop guard or it becomes a self-inflicted incident.
* **One sender gate.** The SPEC-0011 gate — delivery event exists AND verified, or token-trust
  self-managed ingest — lives in the wakeup query, not the publisher, so no path can bypass it. A
  second notification path with a looser gate is the one attackers use.
* **A receiver filter, learned twice.** `RingUnclaimed` had to add one after 1,442 rows on revoked
  endpoints absorbed every sweep: waking into an empty room spends pacing state the consumer needs
  when it comes back.
* **Rolling deploys and mixed versions.** An older process must stay able to read what a newer one
  emits, in both directions.
* **No new instance-wide anything, and no new knob.** Nothing env-configured that belongs to no tenant
  (the shared-receiver lesson), and no setting an operator has to understand to get sane pacing.

## Considered Options

* **(A) Re-fire on claim.** When a claim succeeds and the queue still has pending work, fire another
  wake for the queue.
* **(B) Periodic queue-stall sweep.** A timer asks the database which queues hold pending work older
  than a threshold with nothing claimed since the last ask, and re-fires the wake for those. *(chosen)*
* **(C) Document the self-retrigger.** Leave it to consumers, as today.
* **(D) Ship `todos.backlog` now.** The digest notification SPEC-0024 REQ-9 already reserves.

## Decision Outcome

Chosen option: **(B) Periodic queue-stall sweep**, as a fifth duty of the existing reaper tick, with
the wake going out over the ready hook — the notify-hook transport — under a new `reason` value.

> A doorbell rings a session. A notify hook rings a door nobody is behind yet. A stall nudge rings the
> same door again, because the hallway still has work in it.

### Why (B) and not (A)

Re-fire on claim is cheap, and it is *insufficient*, for three reasons that are each independently
disqualifying:

1. **It cannot recover a lost wake.** The notify-hook dispatcher persists nothing: a restart loses
   in-flight notifications, and a full queue drops one. A queue whose last wake was lost stays silent
   forever under (A), which is the exact failure the report is about.
2. **It only fires while someone is claiming.** The consumer that was killed mid-task, the queue
   whose last item was claimed by a run that then died, and work enqueued by hand or by a second
   instance all produce no claim, so no wake.
3. **It hits the constraint the hand-back named.** A NOTIFY fired at claim time is read by the LISTEN
   loop, whose per-todo gate has 60 seconds of amnesia — suppressing the nudge for exactly the pending
   rows it targets.

(B) subsumes what (A) is good at: a queue that is making progress is a queue the sweep keeps a short
cadence on (see the progress rule below), which is the useful half of "re-fire on claim" without its
blindness to lost wakes.

### Mechanics

**Sweep, not subscription.** `store.NudgeStalledQueues` runs on the reaper tick (30s), alongside lease
reaping, the retry scheduler, the doorbell heartbeat and endpoint expiry — the same context-managed
loop, no new goroutine, no new config. The tick only bounds detection lag; every interval that matters
lives in the ledger.

**The unit is `(endpoint_id, queue)`, not the todo.** Queues are not rows, so the pacing state gets a
table of its own, keyed by the pair and cascading with its endpoint:

```
queue_stall_nudges(endpoint_id uuid → endpoints ON DELETE CASCADE,
                   queue text, nudges int, last_nudged_at timestamptz,
                   PRIMARY KEY (endpoint_id, queue))
```

**What counts as stalled.** The queue's **oldest gate-passing pending** todo is older than
`queueStallAfter` (2 minutes), the endpoint is `active`, and at least one **enabled notify hook** of
that endpoint covers that queue (empty filter = every queue). The age test is on the oldest row rather
than any row so a just-landed todo is never evidence that nobody is coming; two minutes is a race
guard, not a latency target — a healthy consumer that has not gotten to the row yet must not be
double-woken.

**One wake names one todo.** The notification shape already carries `todo_id` (SPEC-0024 REQ-5), so the
wake names the queue's oldest gate-passing pending row instead of firing one POST per pending row. A
50-row backlog costs one wake; a consumer that wants the rest pulls, which is what `claim_next` is for.

**The wake is an ordinary `todo.ready` with `reason: "stalled"`.** The store fires the existing ready
hook with `ReadyStalled` after the charging statement has committed, so the notify-hook dispatcher
re-runs all of its own fire-time checks — endpoint live, queue in the endpoint's current scope, the
hook's queue filter, the SSRF re-validation, the signing — untouched. **No wire format changes.** The
`todo_ready` NOTIFY payload stays exactly `"<endpoint_id>:<queue>"`; the sweep does not use that channel
at all, because an appended reason marker there would be mis-parsed by an older process, which splits on
the first colon and would read `"<queue>:stalled"` as the queue name. A new NOTIFY channel would be
readable but pointless: the ledger already makes the nudge once-per-cooldown **cluster-wide**, so the
instance that charged the queue is the one that should deliver it, and outbound HTTP is not instance-
scoped the way a session registry is.

For a consumer, that means: a reason value it does not know is an ordinary wake — read `todo_id`, claim,
drain. `reason` is a hint about *why*, not a discriminator for *whether*, and SPEC-0011's rule that a
consumer must not branch on a single untrusted field is the same rule applied to ourselves.

### The loop guard

Four rules, all of them in the one statement that charges the queue:

| Rule | Why it is there |
| --- | --- |
| Widening backoff: nudge N waits 5m, 20m, 1h, then 6h | Reuses the heartbeat's schedule, so an operator holds one schedule in their head and a consumer sized for one is sized for both. |
| Progress resets it: a claim in that pair after the last nudge makes the queue due again **and** puts `nudges` back to 1 | A consumer that is claiming is alive; the backlog is simply longer than one invocation. Without this rule a deep queue drains at six-hour steps, which is worse than the silence we set out to fix. |
| A drained queue's row is deleted by the sweep | The next backlog starts at the short delay instead of inheriting a spent budget. |
| `nudges` caps at 5 without progress | Five wakes spread across roughly a day with not one claim is not a starvation problem — the consumer is broken, revoked, or never meant to drain this. A sixth spends a model turn on the same bad news, and the work is still on the board and still pullable. |

Progress is a *claim*, not a completion: the wake's job is to get a claim, and the pattern under
discussion claims one todo and exits. Completion is not needed to prove the consumer is alive.

**Concurrency.** The ledger write is `INSERT … ON CONFLICT (endpoint_id, queue) DO UPDATE … WHERE
<due predicate re-checked>`, so the charge and the decision are one statement and an instance that
committed its nudge first makes the second instance's update match nothing. That is exact-once per
cooldown, narrower than `RingUnclaimed`'s "two sweeps only rarely select the same row", because this is
one keyed row rather than a window over many.

**Tenancy and gate.** System-wide sweep on a timer, with the exemption
SPEC-0003 already states for `RequeueDueRetries` and `RingUnclaimed`: no caller, no credential, nothing
read out, nothing moved; each wake is scoped by the row's own `endpoint_id` and the dispatcher
re-checks scope before the dial. The sender gate is applied twice — to the pending rows that make a
queue a candidate **and** to the representative todo chosen to name the wake — so an unverified row's
title and source never reach a notification body from this path.

### Consequences

* **New table**, additive, no backfill; rollback is `DROP TABLE queue_stall_nudges`, which loses
  pacing state only. Every todo stays pending and claimable throughout.
* **`reason` becomes a small open vocabulary**: `created`, `requeued`, `stalled`. On acceptance this
  ADR amends **SPEC-0024 REQ-5** (the `reason` field's allowed values) and **REQ-6** (the ready hook's
  trigger list gains a wake that is not a transition), and **SPEC-0011** gains a requirement naming the
  half the heartbeat does not cover: a queue whose consumer has no session. Those edits are **not** in
  this PR — the records stay as accepted until the ADR is accepted.
* **`todos.backlog` stays reserved.** The digest is the right shape for a *session-attached* consumer
  clocking in; it is gated on endpoint presence (SPEC-0022) and this ADR does not pre-empt it. If the
  digest ships later, a stalled queue is one more reason to send one and the ledger is unchanged.
* **Nothing changes for a healthy queue.** No new configuration; an operator who changes nothing gets
  the same behaviour as today plus a queue that cannot silently rot.
* **Not covered on purpose:** a queue whose only consumer is an *inbound* trigger with no notify hook
  has no receiver filter to satisfy, and stays woken by whatever starts it. Adding the nudge for those
  would mean switchboard starting things it was never told how to reach, which is the instance-wide
  shape ADR-0029 tore out.
* **Follow-up worth doing, not in this PR:** a `switchboard_queue_stall_nudges_total{queue}` counter so
  the sweep is visible without reading logs, and folding the sweep into the queue-digest work when
  SPEC-0022 lands.

### Confirmation

How to tell it is working, in order of cheapness:

1. `INFO nudged stalled queues count=N` on the reaper tick, and nothing at all when queues are healthy.
2. A notify hook receiver sees a `todo.ready` whose `reason` is `stalled` for a queue it left pending,
   and does **not** see one every 30 seconds.
3. `SELECT endpoint_id, queue, nudges, last_nudged_at FROM queue_stall_nudges` — rows climbing to 5 and
   then stopping on a queue nobody drains; rows gone once the queue drains.
4. The DB-backed tests in `internal/store/queue_stall_test.go`, which are the four scenarios
   [SPEC-0038](../openspec/specs/queue-stall-nudge/spec.md) states.

## Pros and Cons of the Options

### (A) Re-fire on claim

Cheap, near-zero latency after a claim, no new table. Blind to a lost or never-delivered wake, blind to
a queue with no claimant at all, and it collides with the doorbell gate the hand-back named. It buys a
few seconds of latency on the one path that already has a consumer alive.

### (B) Periodic queue-stall sweep *(chosen)*

Closes the gap for every cause, including the ones nobody noticed yet, reuses the gate, the dispatcher's
fire-time checks and the reaper tick, and its ledger is one small table that also documents itself in
SQL. Costs: detection lag up to the tick plus the threshold, a periodic query, and a loop guard that has
to be right or the fix becomes noise.

### (C) Document the self-retrigger

Zero code, and every consumer writes the same racy "am I drained" check against a store it reaches
through an MCP round trip, with a window between the check and the exit. The report's whole argument
against it holds.

### (D) Ship `todos.backlog` now

The digest is a better notification, and it needs presence to know whom to spare. Shipping it now means
either inventing presence inside this PR or sending digests that will be wrong the moment presence
lands. A stall nudge with a `todo_id` needs neither.

## Architecture Diagram

```
                      ┌──────────────────────────────────────────────┐
  inbound webhook ──▶ │ ingest → todo committed → ready hook(created)│──▶ notify hook ──▶ consumer
                      └──────────────────────────────────────────────┘
                              │ (edge: fires once)
  reaper tick (30s) ──▶ NudgeStalledQueues
                              │
        oldest gate-passing pending row older than 2m?
        endpoint active AND an enabled notify hook covers the queue?
        past backoff(nudges) OR claimed since last nudge?  ── no ─▶ quiet (queue is the ledger)
                              │ yes: charge (endpoint,queue) in the same statement
                              ▼
                      ready hook(stalled) ──▶ notify dispatcher ──▶ consumer wakes, claims one
```
