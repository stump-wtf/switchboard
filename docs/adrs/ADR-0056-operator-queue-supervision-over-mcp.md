---
status: proposed
date: 2026-10-30
decision-makers: Proposed by joestump-agent, pending Joe
extends: [ADR-0048, ADR-0047, ADR-0042]
related: [ADR-0022, ADR-0039, ADR-0045, ADR-0050, SPEC-0006, SPEC-0007, SPEC-0014]
---

# ADR-0056: Operator Queue Supervision over MCP

**Status note:** this ADR is **proposed**. It ships with an implementation so the choices can be argued
against working code rather than prose; nothing here is settled until Joe accepts it. It answers
[issue #582](https://gitea.stump.rocks/stump.wtf/switchboard/issues/582), imported from the
[outside report](https://github.com/stump-wtf/switchboard/issues/33), whose thread carries no
maintainer decision — only the intake acknowledgement. The report is a problem statement: the tool
names, the scope mechanism and the requeue semantics are decided here.

## Context and Problem Statement

Switchboard's MCP surface is a worker's surface: `list_todos`, `claim`, `claim_next`, `heartbeat`,
`complete`, `fail`, `release` and the webhook self-management verbs. An agent supervising a queue —
the pipeline owner watching three consumers, the harness operator mid-upgrade — has nothing to read
that answers "is this queue stalled or just quiet", nothing that shows what happened to a todo, and
no way to quiesce, retry or take over a lease without the dashboard or raw API calls.

The load-bearing half of the request is what must *not* happen: the operator verbs must not appear on
a consumer credential. An endpoint that can pause the queue, release another consumer's lease or
requeue dead letters is an endpoint whose worker can stop the pipeline it works for.

## Decision

### 1. A verb family of its own, granted the way ADR-0047 already grants

No new credential kind, no new scope field, no second authority. The supervision verbs join the
per-endpoint verb grant (`verbs.go`) as a family:

| Family | Verbs |
|---|---|
| `QueueReadVerbs` | `queue_stats` |
| `QueueControlVerbs` | `pause_queue`, `resume_queue`, `drain_queue`, `requeue_todo`, `release_lease` |

The family is **not** in `AllVerbs()`, **not** in `DefaultVendVerbs()`, and implied by nothing: no
grant a consumer can name without saying so ("all", `todo`, `webhook-admin`, the plain default)
resolves to any of them, and the friend-grantable list does not carry them. A vend reaches them only
by naming a verb or the `queue-operator` preset. `scopeGuard` knows the family, so a call to a verb
the endpoint does not hold answers the stable `forbidden` code, which is the refusal an outside test
can exercise. The read verb is separable from the control verbs, which is the report's "read-only and
state-changing should be separately grantable".

Deliberate deferral: the vend wizard's chip list is built from `AllVerbs()`, so the family is not a
chip there, and an OAuth consent screen renders an unknown verb as itself. Both are display surfaces
for the consumer path; widening them is a UI change with its own tests, not a reason to fold the
family into the default grant.

### 2. Tenant boundary: the human's tenant, narrowed by the queue grant

`QueueStats` stays what it is — the fleet-wide scrape read behind the Prometheus gauges, served to
the scrape credential and never to an endpoint credential. For an MCP endpoint, a count is a
disclosure, so the surface reads two new seams, both carrying `ownedByHuman`, the predicate the
Board's reads carry:

- `QueueLivenessForHuman(ownerHumanID, queues)` — per granted queue, this human's rows only, with a
  zero row for a granted queue that has never carried work, and `paused` read from `queue_pause`.
- `OutstandingLeasesForHuman(ownerHumanID, queues, limit)` — the live claims, holder and expiry,
  soonest-expiring first, clamped at 200.

Per-todo verbs read the target with `GetTodoItem(ownerHumanID, id)` before any write, then enforce
the queue grant on the row it names: another tenant's todo is `not_found`, not `forbidden`, for the
same reason the Board's drawer answers that way — a 403 would let the tool enumerate other tenants'
work. A cross-tenant supervision surface is not part of this decision; a human supervises their own
queues, and the queue grant is the intra-tenant narrowing.

### 3. Pause, drain and resume are ADR-0048's switch, three verbs deep

`pause_queue` and `resume_queue` call `PauseQueue` and `ResumeQueue`. There is no second quiesce
mechanism, and a drain does not invent one: `drain_queue` pauses first, then re-reads the held
leases every two seconds until none is outstanding or its wait runs out (default 30s, ceiling 300s,
`wait_seconds: 0` for a snapshot). Either way the queue is left paused and the result says so; a
drain never releases a lease itself — releasing is a separate verb a caller must hold.

### 4. A requeue resets the attempt counter, because that is the only answer that works

`requeue_todo` moves a **failed** row back to `pending` through `RetryTodoOperatorOwned` — the
Board's "Retry now" — which clears owner, lease, result, `next_retry_at` and sets `attempt = 0`.
Keeping the counter would look more conservative and would be a trap: the claim predicates refuse a
row at `attempt >= max_attempts`, so a dead letter requeued with its budget spent would go straight
back to dead. The fresh budget is therefore the point, and the ADR says it out loud rather than
leaving it to a diff. A row in any other state is refused with `conflict` before the write, naming
the state it found; the quarantine queue is refused by the store, as everywhere else.

### 5. A lease release reaches the row under another endpoint's lease

`release_lease` reads the row, checks the queue grant, and calls `ReleaseTodoOperatorOwned` with the
holder the row itself names: the operator does not have to know the holder's credential, and cannot
name a different one. A row that is not `claimed` is refused with `conflict` and untouched. This is
the Board's "Release" action over MCP, with the same boundary, and it exists because a killed run
leaves a lease held until the reaper notices — minutes in, minutes out.

### 6. The audit line

Every state-changing supervision call writes one structured line: endpoint slug, the human behind the
credential, the agent id, the verb, the target (`queue=…`, or `todo=… queue=… holder=…`) and the
operator's `reason`. A release also closes the attempt row as `released`, so the todo's own history
shows the hand-back. No credential and no payload enters either.

### 7. The stalled verdict

`queue_stats` answers the report's question in one call, and says how it reads the counts. With
`paused` false, `pending > 0`, `claimed == 0` and the oldest pending row at least
`stalled_after_seconds` old (default 300), the row is `stalled`; `pending == 0` is `quiet`;
`claimed > 0` is `working`; a pause is `paused` before anything else can be, because it explains the
age; anything else is `waiting`. The threshold is a parameter, not a secret, and the counts always
ride alongside the label so a caller can disagree with it.

## Considered Options

- **An `admin` scope on the vended credential.** A second axis of authority next to the verb grant:
  two places to get wrong, and every existing gate would have to consult both. ADR-0047's grant is
  already the axis, and it already supports "by name only".
- **Expose the fleet-wide `QueueStats`.** One fewer read, and it would disclose other tenants'
  volumes to any endpoint holding the verb.
- **`drain_queue` releases stale leases.** A convenience that turns a read-and-wait into a silent
  mutation, and decides for another endpoint that its run is gone.
- **Requeue without resetting attempts.** Refused above: it produces a dead letter that cannot be
  claimed, which is the failure the verb exists to fix.

## Consequences

- An operator endpoint is a thing an operator vends on purpose. Audits can flag one with
  `GrantHasQueueOperator`.
- The consumer surface is unchanged, byte for byte: same tools, same default grants, same
  `tools/list` for every endpoint that was not handed a supervision verb.
- The store grows two reads and no writes; the mutations reuse the seams the Board has driven since
  the tenant fix, so the new surface cannot be wider than the surface it mirrors.
- If Joe prefers different names, the rename is verbs.go, the guide's table and the ADR. The
  boundaries — family not implied, tenant-scoped reads, one pause switch, attempts reset — are what
  the implementation rests on.
