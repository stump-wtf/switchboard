---
status: proposed
date: 2026-10-20
decision-makers: Proposed by joestump-agent, pending Joe
extends: [ADR-0007, ADR-0002]
related: [ADR-0013, ADR-0022, ADR-0029, ADR-0035, ADR-0045]
---

# ADR-0047: Per-Queue Pause — Quiesce Delivery, Never Intake

**Status note:** this ADR is **proposed**. It ships with an implementation so the choices can be
argued against working code rather than prose; nothing here is settled until Joe accepts it. It
answers [issue #581](https://gitea.stump.rocks/stump.wtf/switchboard/issues/581), whose thread had
no decisions in it; the three open choices the issue named — granularity, the claim response while
paused, and resume semantics — are decided here.

## Context and Problem Statement

An operator draining a downstream consumer (upgrade, restart, incident triage) has no way to tell
Switchboard "stop handing out work for now" without pulling a lever that is too big:

* **Disabling the endpoint or webhook** drops or fails inbound deliveries — the opposite of durable.
* **Stopping the consumer process** does not stop the wakes: the doorbell and the ready hooks keep
  firing, and for a consumer the harness starts per doorbell, every wake is a fresh run starting
  exactly while the operator is trying to quiesce ([ADR-0045]'s one-todo-per-invocation consumer,
  seen from the other side; harness#828 is the same story on the consumer end).

The queue is the one component that is authoritative about both halves of the question: intake goes
into it, delivery comes out of it. ADR-0035 already chose the queue as the place to bound *volume*
because "a todo becomes work only when it is claimed"; the same argument says the queue is where a
pause belongs.

The three choices the issue left open:

1. **Granularity.** Per queue, per endpoint, or per endpoint-and-queue?
2. **The claim response while paused.** Hand out nothing, or answer "paused" explicitly on the wire?
3. **Resume.** Re-ring the backlog how, and how loudly?

## Decision

### 1. Pause is per queue, fleet-wide

Queues are a global namespace (SPEC-0003): the counts, the board and the metrics all read them that
way, and `endpoints.scope_queues` is a grant *onto* that namespace, not a private copy. A pause of
`inbox` is therefore a property of `inbox`, keyed by its name alone in `queue_pause`. Per-endpoint
pause would multiply one operator decision across every endpoint that shares a queue name — and an
operator quiescing a shared consumer means every consumer of that queue. Per-endpoint control
already exists at a different layer: the endpoint's queue scope, set at vend.

The quarantine queue refuses a pause: it is already never delivered, and a pause there would imply
its contents wait to be handed out.

### 2. While paused: claim hands out nothing; visibility lives in the reads

`claim` and `claim_next` gain no new wire value. The claim predicates gain one clause — a candidate
must not sit on a paused queue — evaluated **inside the locking statement** that picks or locks the
row. That keeps the pause race-free against concurrent claims (a pause cannot be slipped past by a
claim that read the state a statement earlier) and reuses the `FOR UPDATE SKIP LOCKED` machinery
rather than racing it (ADR-0002). `claim_next` on a paused queue answers exactly as it does on an
empty one; a consumer needs no new branch, and a client written before this ADR is unaffected.

"Pause where?" must not be a mystery, so the state is rendered on every read surface instead:

* MCP `list_todos` gains `paused_queues`, the granted queues currently paused;
* `GET /api/v1/queues` lists every known queue with its counts and a `paused` flag with reason and
  time, and a paused queue is *known* even when it holds no todos;
* `QueueStats` (the metrics seam) carries the flag, so a stalled-looking queue with a growing
  `oldest_pending` and `paused=1` reads as deliberate, not broken.

Enforcement has two halves. The **claim predicates are the authoritative half** — delivery cannot
escape them. The wake paths (the in-process ready/doorbell hooks) check the same table best-effort
and fail *open*: a failed probe costs at most one wasted wake, while failing closed could silently
strand a live queue behind a transient read error. Wakes are hints (SPEC-0011); the claim is the
contract.

The doorbell reads (`RingUnclaimed`, `RingOnAttach`, `PendingDoorbellTodos`) and the stall sweep
(ADR-0045) gain the same predicate: a paused queue must not spend ring or nudge budget on a consumer
the operator has deliberately stilled, and resume re-rings anyway.

### 3. Resume re-rings the backlog once, through the ordinary chain

Resume deletes the pause row and then fires the same two wake edges a creation would: a queue-wide
`todo_ready` notification (every instance re-rings its attached sessions' pending rows) and the
ready hook naming the oldest pending todo with a new reason, `resumed`. A consumer that does not
know the reason value treats it as an ordinary wake, exactly as with `stalled`. Nothing is
scheduled, persisted, or re-fired: one resume, one wake pair. If it is missed, the queue is not
silent — the stall sweep picks the backlog up on its ordinary schedule, which the pause had held
quiet.

Pause state lives in Postgres, so it survives restarts and applies across every instance at once;
it is deliberately not replicated to consumers as an event. Resume of a never-paused queue is not
an error; it reports that nothing was lifted.

**In-flight claims are not interrupted.** The ask is a drain, not an abort: work already handed out
runs to its conclusion (heartbeat, complete, fail, release all keep working). A queue is live the
moment it is resumed, and nothing auto-resumes.

## Decision Drivers

* Nothing in flight is lost: intake persists, in-flight claims finish, paused rows keep their dedup
  slots and spend no attempts.
* One authoritative chokepoint: delivery is enforced where claims are decided, in the same statement
  that locks the row — not in eight consumers' "quiesce" reimplementations.
* The wire does not change: no new claim response, no new notification type, additive fields only.
* A paused queue is legible: every read surface says *paused*, with a reason, rather than looking broken.
* The wake chain is reused, not duplicated: resume is an enqueue-shaped wake, so the stall sweep,
  the doorbell and the hooks all keep working unchanged after resume.

## Considered Options

* **Per-endpoint pause.** More granular, but it multiplies one operator decision across endpoints
  and implies per-endpoint queue copies the global namespace does not have. Rejected.
* **An explicit `paused` answer from `claim_next`.** Louder for a confused consumer, but it adds a
  wire value every client must tolerate and duplicates what the reads already say. The reads are
  where "why is nothing moving" is answered. Rejected for now; additive to revisit.
* **A durable resume replay** (mark rows as owed-a-wake, retry until delivered). The stall sweep is
  already the durable backstop for exactly this gap (ADR-0045); a second ledger would be a second
  way to be wrong. Rejected.
* **Pausing via MCP too.** The operator story is the ask; agents pausing their own delivery would
  let a misbehaving endpoint hide work from its own board. CLI + human API first; an MCP verb can
  be scoped later if a real consumer needs it. Deferred.

## Consequences

* **Positive.** One command drains a consumer cleanly: `switchboard queue pause inbox`, wait for the
  in-flight run, upgrade, `switchboard queue resume inbox`. Nothing is dropped at any step; the
  board never lies about why a queue is still.
* **Negative.** `fireReady`/`fireDoorbell` now probe `queue_pause` before firing — one indexed
  primary-key read per wake on a process with hooks wired. Fleet-wide pause enforcement adds a
  `NOT EXISTS` to the claim predicates; it is a PK probe beside the predicates already there.
* **Risk.** A queue paused by name affects every endpoint draining that name — intended, but worth
  stating: there is no shadow copy of a queue per endpoint.

## Verification

Store tests cover the lifecycle end to end: paused intake persists while claims are refused; the
doorbell and ready hooks stay silent while paused; resume wakes exactly once with reason `resumed`
and the backlog claims; the stall sweep is silent while paused and reclaims the queue after resume;
the paused flag reaches `QueueStats`. The API tests cover the operator surface, including the
quarantine refusal. See `internal/store/queue_pause_test.go` and `internal/server/api_queue_test.go`.
