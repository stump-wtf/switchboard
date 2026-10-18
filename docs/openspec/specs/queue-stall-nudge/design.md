---
status: draft
date: 2026-10-15
implements: [ADR-0045]
---

# Design: Queue-Stall Nudge

## Context

The wake chain is a set of edges over a durable queue. A consumer with no session — the class
[ADR-0029](../../../adrs/ADR-0029-outbound-todo-webhooks.md) exists for — has one edge per todo, and
nothing between them. #566 reported the resulting hole and the workaround every consumer had to write.
The lane-m hand-back on 2026-10-13 stopped on a specific collision: the LISTEN loop's
`nudgeDoorbells` and the creation hook share the `doorbellGate`, whose per-todo one-minute window would
suppress a claim-fired wake for exactly the rows it targets.

Existing pieces this reuses:

* `store.SetTodoReadyHook` → `internal/notifyhook` dispatcher: sender-gated, endpoint/scope/hook
  re-checked per attempt, SSRF re-validated, signed, retries, auto-disable (SPEC-0024).
* The reaper tick: context-managed, error-tolerant, already carries four maintenance sweeps.
* `endpoints.state`, `notify_hooks.enabled/queues`, and the `todos` partial pending index on
  `(endpoint_id, queue, created_at)`.

## Goals / Non-Goals

### Goals

- A queue with claimable work wakes its consumer with no new inbound webhook, at a bounded rate.
- One wake per `(endpoint, queue)` per cooldown across a multi-instance deployment, decided and
  charged in one statement.
- Nothing new on the wire, and nothing a consumer has to be rebuilt for.
- A healthy or drained queue costs nothing: no wake, no log line, no pacing charge.

### Non-Goals

- Lowering first-wake latency for a queue whose consumer is already alive.
- Replacing the doorbell heartbeat, the backlog digest, or presence.
- Any new configuration surface.

## Decisions

### A table for a queue that is not a table

Queues are a `text` column, so the pacing state gets its own keyed table rather than columns on `todos`
(a queue-level counter on a row-level table would need every row updated to pace one queue, which turns
a sweep into a backlog-sized UPDATE) and rather than an in-process map (which resets on restart, so a
consumer that restarts often never backs off, and which is per-instance, so N instances wake N times).

`PRIMARY KEY (endpoint_id, queue)` with `ON DELETE CASCADE` inherits the endpoint's ownership exactly
like `notify_hooks` does: no owner columns, no second cleanup on revoke, nothing tenant-wide to audit.

### One statement decides, charges and names

`NudgeStalledQueues` is one query: drain-reset → group pending work per pair → test due against the
ledger → cap the number of pairs → pick one representative todo each → `INSERT … ON CONFLICT DO UPDATE
… WHERE <due again>` → select the named rows.

Both CTEs that feed two consumers are `MATERIALIZED`, for the reason `RingUnclaimed` documents: inlined
as subqueries they can land on the inner side of a nested loop and be rescanned per row.

The `ON CONFLICT … WHERE` re-check is what makes the nudge exactly-once per cooldown rather than
"rarely" once: a pair another instance just charged fails the predicate, `RETURNING` yields nothing, the
pair is not in the final select, and no hook fires. `RingUnclaimed` cannot do this — it charges a window
over many rows — so it settles for narrowed exclusivity.

The drain delete is a data-modifying CTE, so it runs every tick whatever the rest selects. It is bounded
by the ledger's own size, which is bounded by the number of queues with pending work.

### The representative todo is gated twice

A pair qualifies on any gate-passing pending row; the todo named in the wake is chosen among
gate-passing pending rows only. The second application is not redundancy: the notification body carries
the todo's `kind`, `source` and `summary` (neutralised and length-capped, but sender-supplied), so the
row that names the wake must be one the sender gate vouches for. Unverified rows can make a queue
stalled — they are real pending work — but they never lend their text to the wake.

### The wake is a ready hook, not a NOTIFY

Firing the ready hook keeps every fire-time check the dispatcher already performs, including the ones
this sweep cannot see from SQL (the endpoint's current queue scope, the hook's health, the SSRF
re-resolution at dial). Emitting `pg_notify` instead would route through `nudgeDoorbells` — the channel
doorbell path the hand-back warned about — and its payload format is parsed positionally: `strings.Cut`
on the first colon means any appended marker lands inside the queue name for an older process. A new
channel would avoid that but buys nothing, because the ledger already makes the wake once-per-cooldown
cluster-wide and outbound HTTP is not instance-scoped.

### Progress is a claim, not a completion

The wake's contract is "there is something here for you to claim". A consumer that claims one todo and
exits has proven it is alive; requiring a completion would pace a slow-but-healthy consumer as if it
were stuck, which is the failure mode this whole feature exists to remove.

### Where the sweep runs

On the reaper tick, as a fifth duty, rather than a new goroutine: the tick already owns the same
lifetime and error posture, and 30 seconds is far below every interval in the ledger, so the tick only
bounds detection lag. The intervals themselves are constants beside the query, matching
`ringBackoff`, so an operator reads one schedule off the log lines.

## Risks

| Risk | Mitigation |
| --- | --- |
| A stalled queue wakes a consumer forever | Widening backoff, a 5-nudge cap, and progress/drain resets that only fire when the queue is genuinely moving. |
| A stalled fleet wakes many consumers at one tick | `queueStallSweepLimit = 4`, least-nudged pair first. |
| An unverified sender gets free doorbells naming its rows | Gate applied to the representative todo as well as the candidate rows. |
| Sweep cost on a large `todos` table | Candidate scan rides the partial pending index; the per-pair progress probe is an `EXISTS` that short-circuits behind the two time tests; grouping happens before the ledger join, so the ledger is touched once per queue with old pending work. |
| Pacing state outlives usefulness | Row deleted when the pair has no pending rows; cascade when the endpoint goes. |
| Two instances double-wake | `ON CONFLICT … WHERE` re-check at lock time. |

## Testing

`internal/store/queue_stall_test.go` (DB-backed, skipped without `SWITCHBOARD_TEST_DATABASE_URL`)
covers the spec's scenarios: ring with no new webhook, quiet while work is fresh, drained queue silent
with its budget reset, backoff steps and the cap, progress reset, receiver filter, sender gate, one wake
per queue. `internal/server/reaper_test.go` binds the tick wiring and the error path against a fake
store.
