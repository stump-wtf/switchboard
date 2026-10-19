---
status: proposed
date: 2026-10-19
decision-makers: Proposed by joestump-agent, pending Joe
extends: [ADR-0002, ADR-0021]
related: [ADR-0022, ADR-0023, ADR-0034, ADR-0043]
---

# ADR-0046: Interrupt-Lease TTL — A Deadline For A Wait Whose Holder May Be Gone

**Status note:** this ADR is **proposed**. It ships with an implementation so the choice can be argued
against working code rather than prose; nothing here is settled until Joe accepts it. The SPEC-0018
open question it answers, and the requirement text it implies for SPEC-0018 and SPEC-0006, are listed
under Consequences rather than applied: accepted records stay as accepted until this ADR is accepted.

## Context and Problem Statement

`InterruptTodo` moves a claimed todo to `input-required` or `auth-required` and **deliberately retains the
owner and the lease** (SPEC-0018 scenario "Interrupt states return to claimed"): the same worker is
supposed to come back and `ResumeTodo` it, and nothing else may take the todo while the client is being
waited on.

That leaves the row with no alarm of its own.

* The lease reaper's predicate needs `state='claimed'`, so an interrupt-state row is invisible to it.
* The claim predicate covers pending, lapsed claimed and due failed, so the row is not claimable either.
* A row in an interrupt state **cannot be heartbeated**: `HeartbeatTodo` is guarded on
  `state='claimed'`. So the retained `lease_expires_at` is a clock set by the claim, it keeps expiring on
  that original deadline, and nothing can move it. (`ResumeTodo`'s own comment says the worker "was
  expected to heartbeat across the interrupt" — that expectation cannot be met today, and this ADR says
  so rather than quietly granting the capability: see Consequences.)

If the holder dies mid-interrupt, the todo therefore parks forever: not reaped, not claimable, not
terminal, and — since retention skips the A2A states — not pruned. The only recovery is somebody calling
`resume` for a worker that no longer exists.

`docs/openspec/specs/a2a-tasks/design.md` lists exactly this as an unresolved Open Question of SPEC-0018,
and names "leaves it interrupted indefinitely" as an acceptable answer. So the policy has never been
decided, which is why #536's audit ended here rather than in code, and why #594 says a lane must state
the proposal instead of burying a default in a `WHERE` clause. Migration 0017 already dead-lettered this
same shape for **revoked** endpoints; the live-endpoint half — the holder crashed, the endpoint is fine —
is still open, and it is the whole of #594.

**The decision is three questions:** how long a wait is a wait, what past that deadline means, and how an
operator says something else.

## Decision Drivers

* **Crash safety is one mechanism.** ADR-0002 made the reaper the answer to "the worker is gone". An
  interrupt whose holder is gone is the same failure with a different state, so it should be the same
  sweep with the same interval and the same log line, not a second loop.
* **The wait is legitimate.** `input-required` waits on a human or an out-of-band client. A deadline
  short enough to catch a crashed holder and a slow human at the same time would silently break
  SPEC-0018's scenario, so it has to be far longer than any lease.
* **The queue is the ledger.** Losing a wake costs latency and never work (ADR-0013, ADR-0029). The same
  instinct says the *default* past a deadline is to put the work back where a consumer can reach it, not
  to end it.
* **Attempts already bound the pain.** SPEC-0003's `max_attempts` is the existing "this keeps failing"
  stop. A reclaim should consume an attempt and dead-letter at the cap, exactly like a lapsed lease, or
  an interrupt loop would retry forever.
* **The counter has to stay honest.** SPEC-0034 REQ-14 reconciles every `reaped` attempt close with one
  `LeaseExpired` increment. An interrupt reclaim closes an attempt as `reaped`, so it must count there
  too; the new counter is a cause marker beside it, not a replacement for it.
* **No new instance-wide knob.** A per-tenant setting or the settings table, not an environment variable
  that belongs to nobody (the shared-receiver lesson).

## Considered Options

* **(A) Leave it interrupted indefinitely** — SPEC-0018's stated acceptable answer: document the parking
  and let an operator resume or cancel by hand.
* **(B) Deadline, then requeue** — reclaim past a TTL into `pending`, consuming an attempt and
  dead-lettering at the cap. *(chosen)*
* **(C) Deadline, then fail** — transition past the TTL to `failed`, which is the shape SPEC-0018's open
  question describes.
* **(D) Let another holder take the row over** while it is still in the interrupt state.
* **(E) Per-endpoint interrupt TTL**, mirroring ADR-0043's `default_lease_ttl_seconds`.

## Decision Outcome

Chosen option: **(B) Deadline, then requeue**, as a second predicate on the existing reaper tick, with a
24-hour default TTL, one settings override that can switch the sweep off, and one new counter naming the
cause.

> A lapsed lease says a worker stopped working. A lapsed interrupt says nobody answered. Both mean nobody
> is coming back for this row — only the second one takes a day to find out.

**The default TTL is 24 hours**, which is deliberately `lease.MaxTTL`: the longest claim an endpoint can
be given. An interrupt always outlives every claim that could still be alive to resume it, so the sweep
can never reap a wait that a live, legitimately-leased holder is still entitled to. It is also a working
day, which is roughly how long "the client will answer" means before it means "nobody is going to".

**Past it, the row goes back to `pending`** with `owner` and the lease cleared, its open attempt closed
`reaped`/`requeued`, and the interrupt's detail left in `result` so the next claimant reads what was being
asked instead of starting blind. At `attempt >= max_attempts` the same statement dead-letters it
(`failed`, attempt closed `dead_lettered`) and writes `result.error = "interrupt_expired"` with a one-line
detail, the way migration 0017's dead-letters explain themselves to whoever finds the row months later.

**The operator override is one settings row**, `interrupt_lease_ttl_seconds`, read the way `sse_retry_ms`
is read: a documented default, and a value that does not parse logs and keeps the default. **Any
non-positive value switches the sweep off**, which turns option (A) — SPEC-0018's other acceptable answer —
into an operator's choice instead of an accident. A positive value below `lease.MinDefaultSeconds` (60s)
is raised to that floor: a deadline shorter than the shortest lease an endpoint may be handed would make
the interrupt state strictly worse than simply working through.

### Why (B) and not (C), (A) or (D)

**(C) fail** is what SPEC-0018's open question assumed, and it is the more destructive of the two. The
todo's payload is still exactly as valid as the moment it was enqueued — the *wait* expired, not the
*work*. Failing it needs a human to re-enqueue, which puts a board column between a crashed process and
progress, and it makes the ordinary case (a holder that restarted) cost the same as the pathological one.
The attempt cap already provides (C)'s outcome for a row that keeps interrupting and never gets answered:
after `max_attempts` reclaims, it is `failed`, with a cause.

**(A) indefinitely** is a legitimate policy and it is retained — as the override, not the default. As the
default it means an unbounded set of rows that are neither in flight nor finished, growing quietly, invisible
to the reaper and to retention, and reported by nothing.

**(D) takeover** would let a second worker resume a wait whose answer was addressed to somebody else's
session, which is what the retained `owner` exists to prevent (SPEC-0018's scenario is *the same worker*
returns). Requeueing is the safe half of takeover: after the deadline the row is claimable by anyone, but
only after it has stopped claiming to be owned.

**(E) per-endpoint TTL** is a reasonable later step and it is not needed now: the deadline guards against
abandonment, which is a property of the deployment, not of one consumer's work. Adding a column plus a
validated endpoint setting for it (ADR-0043's shape) buys nothing until someone's waits genuinely differ.

## Mechanics

**The fact the policy needs was never stored**, so migration 0031 adds `todos.interrupted_at`: stamped by
`InterruptTodo`, cleared by `ResumeTodo`, by `CancelTodo`, and by the sweep itself. It is nullable, the
backfill stamps existing interrupt rows with `updated_at` (the conservative approximation: it can only make
an already-stranded row due sooner, never hide one), and a partial index on
`interrupted_at WHERE state IN ('input-required','auth-required')` keeps the tick an index probe.

**No new loop.** `ReapInterrupted` runs from `ReapExpired`, on the existing 30s tick, after the lease
predicate — same context-managed sweep, one log line, one interval. Like every other timer-driven sweep it
is deliberately not endpoint-scoped (SPEC-0003's background-sweep exemption): crash recovery must not
depend on somebody being logged in.

```
ReapExpired(ctx)
  ├─ state='claimed' AND lease_expires_at < now()            → requeue / dead-letter   (ADR-0002)
  └─ state IN ('input-required','auth-required')
     AND interrupted_at < now() - interruptLeaseTTL          → requeue / dead-letter   (this ADR)
```

**Counters.** Each reclaimed row counts one `switchboard_todo_leases_expired_total{queue}` — required by
SPEC-0034 REQ-14, since the attempt it closes is a `reaped` close — plus one
`switchboard_interrupt_leases_reclaimed_total{queue}`, which is the one that says *why*: how much of the
duplicate-work signal is a wait that outlived its holder rather than a worker losing a race.

**The board sees it.** A reclaimed row fires the transition hook with its committed state and, when it
lands in `pending`, the ready hook with reason `requeued` — the same path a lapsed lease uses, so a
consumer and the SSE board need no new vocabulary.

### Consequences

* **One additive column**, no rewrite, no backfill of consequence; rollback is
  `ALTER TABLE todos DROP COLUMN interrupted_at`.
* **`updated_at` stops being a proxy for "when did this wait start"**, which is the reason a stamp of its
  own was worth a migration: any future path that touches an interrupted row (the A2A handler writing an
  answer's `result`, say) would otherwise extend the deadline by accident.
* **SPEC-0018's Open Question is answered** and SPEC-0018 gains a requirement naming the deadline and its
  override; SPEC-0006's lease/crash-safety requirement gains the sentence that an interrupt lease is aged
  by its own clock. Those spec edits are **not** in this PR — they land when this ADR is accepted.
* **A live holder still cannot extend a long wait.** `HeartbeatTodo` is guarded on `state='claimed'`, so
  a worker that is very much alive and waiting on a slow client past the TTL loses the row. That is a
  deliberate non-change: granting heartbeat in interrupt states widens who may hold a lease, which is
  ADR-0043's territory and a decision of its own. Until it is taken, the TTL is the maximum wait, and an
  operator who needs longer sets `interrupt_lease_ttl_seconds` (or 0, and the waits are unbounded as
  today). This is the follow-up most worth doing.
* **Revoked endpoints are unchanged**: their stranded rows are still dead-lettered by the revocation
  cascade (and by migration 0017 for rows that predate it). This sweep does not reach for them, because a
  deadline that requeues work nobody can claim would turn a dead endpoint into a churn machine.
* **Nothing changes for a todo that is resumed normally**, or for any todo that never interrupts.

### Confirmation

1. `INFO reaped expired leases count=N` on a tick where an interrupt actually aged out, and nothing at all
   when waits are healthy.
2. `switchboard_interrupt_leases_reclaimed_total{queue}` is 0 on a deployment where interrupts get
   answered, and rises only when holders die — and `switchboard_todo_leases_expired_total` rises with it,
   one for one, since each reclaim closes a `reaped` attempt.
3. `SELECT count(*) FROM todos WHERE state IN ('input-required','auth-required')` stays bounded on a
   long-lived instance instead of growing with every crashed worker.
4. Setting `interrupt_lease_ttl_seconds` to `0` and re-running the tick reclaims nothing: the "leave it
   interrupted indefinitely" answer is reachable without a deploy.
