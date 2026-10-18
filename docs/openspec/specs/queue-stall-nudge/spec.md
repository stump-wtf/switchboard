---
status: draft
date: 2026-10-15
implements: [ADR-0045]
extends: [SPEC-0011, SPEC-0024]
requires: [SPEC-0004, SPEC-0003]
related: [SPEC-0022, SPEC-0034, SPEC-0030]
---

# SPEC-0038: Queue-Stall Nudge

## Overview

This capability makes a queue that holds claimable work wake its consumer even when nothing arrives
upstream. Every other wake in switchboard is an edge — a creation, a requeue, an attach, a heartbeat
re-ring — and each of them needs a transition to fire. A consumer that claims exactly one todo per
invocation and is started by an outbound notify hook (SPEC-0024) spends its one notification and exits;
if no new inbound webhook arrives, the queue can hold real, claimable work indefinitely with no edge
left to fire. #566 asked switchboard to answer "is my queue drained", because it is the only component
that knows.

This is the level-triggered half of the wake chain, and it is deliberately narrow: it wakes the
**notify-hook transport**, because the session-attached consumer is already covered by SPEC-0011 REQ
"Doorbell Heartbeat", and it changes **no wire format**: a nudge is an ordinary `todo.ready` whose
`reason` is `stalled`.

The queue stays the ledger end to end. A lost, dropped, refused or never-configured nudge costs
latency, never work.

### Out of scope

* A consumer whose wake comes only from an inbound trigger with no notify hook: switchboard is not told
  how to reach it, and inventing a reach is the instance-wide shape ADR-0029 removed.
* The `todos.backlog` digest, which stays gated on endpoint presence (SPEC-0022).
* Re-fire-on-claim as a mechanism (ADR-0045 "Why (B) and not (A)").

## Definitions

* **Stalled queue** — an `(endpoint_id, queue)` pair whose oldest gate-passing `pending` todo is older
  than the stall threshold.
* **Nudge ledger** — the `queue_stall_nudges` row for that pair, holding `nudges` and
  `last_nudged_at`: the pacing state that makes a periodic wake safe to run.
* **Progress** — a claim on any todo in that pair after the last nudge.

## Requirements

### Requirement: Stalled Queue Detection

A queue with at least one `pending`, unclaimed todo whose delivery passes the SPEC-0011 sender gate,
whose oldest such todo is older than the stall threshold, on an `active` endpoint, and with at least one
**enabled notify hook** of that endpoint covering that queue MUST be a nudge candidate. The sender gate
and the endpoint-state test MUST be predicates of the sweep's query, not checks in the publisher.

A todo created within the stall threshold MUST NOT make its queue a candidate, and a todo in the
`quarantine` queue MUST NOT count as pending work for this purpose. The threshold is fixed by
implementation (2 minutes at time of writing), not by configuration.

#### Scenario: Pending work with no new inbound webhook

- **WHEN** a verified todo has been `pending` and unclaimed for ten minutes and nothing has arrived
  upstream since
- **THEN** the sweep names that todo, and the notify hook receives one `todo.ready` with
  `reason = "stalled"`

#### Scenario: Fresh work is not a stalled queue

- **WHEN** a todo was created seconds ago and its queue has no other pending work
- **THEN** the sweep selects nothing AND the queue's nudge count does not advance

#### Scenario: Unverified deliveries age quietly

- **WHEN** a queue's only pending todos came from an unverified delivery on an open-trust receiver
- **THEN** no nudge fires AND the todos remain `pending` and claimable by pull

#### Scenario: A queue with no receiver is not charged

- **WHEN** an endpoint has no enabled notify hook covering the queue
- **THEN** no nudge fires AND the pair's ledger row is not created or advanced, so the consumer's return
  does not find a spent budget

### Requirement: One Wake Per Queue

Each nudge MUST fire exactly one wake per `(endpoint, queue)` pair, naming that queue's oldest
gate-passing `pending` todo. It MUST NOT fire one notification per pending row. The named todo MUST
itself pass the sender gate, so sender-supplied text from an unverified row never reaches a
notification body by this path.

#### Scenario: A deep backlog costs one wake

- **WHEN** a queue holds five gate-passing pending todos past the threshold
- **THEN** the sweep returns one todo, the oldest, and the hook receives one notification

### Requirement: Wake Shape and Compatibility

A nudge MUST be delivered through the SPEC-0024 ready hook with `reason = "stalled"`, after the
statement that charged the ledger has committed, on the instance that charged it. The notification's
body and signature MUST otherwise be exactly as SPEC-0024 REQ-4 and REQ-5 define them.

The `todo_ready` NOTIFY payload MUST remain `"<endpoint_id>:<queue>"`. This capability MUST NOT add a
field, suffix or channel to that payload, so a process running an older build keeps parsing it
correctly during a rolling deploy.

A consumer MUST treat an unrecognised `reason` as an ordinary wake: read `todo_id`, claim via the
durable verbs. `reason` describes why, never whether.

#### Scenario: An older consumer is woken correctly

- **WHEN** a consumer built before this capability receives a nudge
- **THEN** it reads `meta`/body `todo_id` and claims, ignoring the unknown `reason` value

#### Scenario: Mixed versions during a deploy

- **WHEN** a newer instance and an older instance run against one database
- **THEN** the older instance's LISTEN loop parses every `todo_ready` payload it receives, and the
  newer instance's nudges are not delivered twice

### Requirement: Nudge Pacing and Loop Guard

A nudge MUST be charged in the same statement that decides it, keyed on `(endpoint_id, queue)`, and the
charge MUST re-check the due predicate at lock time so a nudge another instance committed first is
neither counted nor delivered twice.

The delay before the Nth nudge since the queue last made progress MUST widen (5 minutes, 20 minutes,
1 hour, then 6 hours at time of writing, matching SPEC-0011's ring schedule).

* A claim in the pair after the last nudge MUST make the pair due again AND reset its nudge count to 1.
* A pair with no `pending` todo left MUST have its ledger row deleted, so the next backlog starts at the
  first backoff.
* Without progress, nudges MUST stop at the cap (5 at time of writing); the todos stay `pending` and
  MUST remain visible and claimable.
* One sweep tick MUST wake at most a fixed small number of queues (4 at time of writing), least-nudged
  first, so a stalled fleet cannot start an unbounded number of consumers at once.

#### Scenario: A stalled queue backs off and stops

- **WHEN** a queue is nudged five times and nothing in it is claimed
- **THEN** the sweep stops selecting it, and its pending todos stay claimable and visible

#### Scenario: Progress keeps the short cadence

- **WHEN** a queue whose nudge count had reached 4 has a todo claimed and completed, and pending work
  remains
- **THEN** the queue is due again immediately AND its nudge count is 1, so a one-todo-per-invocation
  consumer drains the backlog at the short cadence rather than at six-hour steps

#### Scenario: A drained queue does not ring

- **WHEN** every todo in a queue has been claimed and completed
- **THEN** the sweep wakes nothing, the ledger row is gone, and the next todo to sit pending on that
  queue is nudged at the first backoff

#### Scenario: Two instances, one wake

- **WHEN** two instances run the sweep against one database within one cooldown window
- **THEN** exactly one of them fires the wake for that queue

### Requirement: Queue Wake Is Not A Transition

Firing the nudge MUST NOT change the todo: its `state`, `attempt`, `ring_attempts`, `last_ringed_at`,
`claim_owner` and lease MUST be unchanged. It MUST NOT ring the MCP channel doorbell and MUST NOT
advance the todo's ring budget, because the session-attached path is SPEC-0011's and its
per-todo gate's suppression window would otherwise swallow the wake.

#### Scenario: A nudge spends no ring budget

- **WHEN** a queue is nudged
- **THEN** the named todo's `ring_attempts` and `last_ringed_at` are unchanged, and a session attached
  to that endpoint is not rung by the nudge

#### Scenario: Nudging does not wake a quarantined queue

- **WHEN** a held delivery's queue is the `quarantine` queue
- **THEN** no nudge fires, whatever the quarantine row's age

## Scenarios (end to end)

### Scenario: The reported case

- **WHEN** a one-shot consumer is started by a notify hook, claims one todo per ADR-0029 wake and
  exits, leaving two more todos in `lane-l`
- **THEN** with no further inbound webhook, the consumer is woken again within the first backoff,
  claims the next todo, and is woken once more until the queue is empty, at which point the wake stops

### Scenario: The wake is lost

- **WHEN** the notify-hook dispatcher's queue is full at the moment of creation and the notification is
  dropped
- **THEN** the queue's pending work is still surfaced by the sweep after the threshold, with no
  consumer-side change

## Metrics

Reserved for a follow-up (ADR-0045 "Consequences"): `switchboard_queue_stall_nudges_total{queue}` and
`switchboard_queue_stall_ledger_rows`. This capability adds neither.

## Failure Modes

| Failure | Behaviour |
| --- | --- |
| Sweep query error | Logged on the reaper tick, the loop continues; the queue is still drained by pull. |
| Nudge delivery fails | SPEC-0024's retry and auto-disable rules apply unchanged; the next backoff re-tries. |
| Ledger table missing (failed migration) | The sweep errors and is logged like any store failure; nothing else in the wake chain changes. |
| Consumer woken but never claims | Backoff widens to the cap, then silence; the work stays on the board. |
