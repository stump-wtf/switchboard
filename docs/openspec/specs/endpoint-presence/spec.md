---
status: draft
date: 2026-09-17
implements: [ADR-0027]
requires: [SPEC-0003, SPEC-0006, SPEC-0007, SPEC-0011, SPEC-0014]
---

# SPEC-0022: Endpoint Presence (Clock In / Clock Out)

## Overview

Every vended endpoint has a **presence**: whether its agent is taking doorbells right now. An agent
clocks out to stop being rung, and clocks in to be rung again. Its human can do the same, or set a
standing weekly **shift**. While an endpoint is out, Switchboard holds its doorbells: todos are still
created and can still be pulled, but no push is sent and no heartbeat ring is spent. When the endpoint
comes back in, it receives one **digest** doorbell summarizing what waited. See
[ADR-0027](../../../adrs/ADR-0027-endpoint-presence-clock-in-clock-out.md).

This spec defines presence, the verbs, the operator surface, and the digest. The delivery changes it
depends on are amendments to [SPEC-0011](../channels/spec.md): REQ "Push Notification Shape" (the
digest), REQ "Scope-Filtered Fan-Out" (the presence filter), and REQ "Doorbell Heartbeat" (delivery
accounting). The self-verb exemption is an amendment to [SPEC-0006](../agent-tools/spec.md) REQ
"Scope Enforcement at the Boundary" and [SPEC-0007](../vended-endpoints/spec.md) REQ "Scoped
Capability Enforcement".

Terms:

* **In / out**: the endpoint's effective presence.
* **Shift**: an optional weekly schedule stored on the endpoint.
* **Override**: an `in` or `out` presence set by the agent or operator, with an optional end.
* **Boundary**: an instant at which the shift's answer changes.
* **Held todo**: a pending, push-eligible todo on an endpoint that is out.

## Requirements

### Requirement: Presence Model

Each endpoint MUST carry: an optional `shift`; an optional override of `presence_override` (`in` or
`out`), `override_until` (timestamp or null) and `override_by` (`agent` or `operator:<human_id>`);
and `presence_seen` (the effective presence last acted upon, REQ "Clock-In Digest").

The effective presence at time `t` MUST be decided, in order:

1. an override whose `override_until` is null or after `t` and which has not been ended by a boundary
   (see below) decides;
2. otherwise, if a shift is set, `in` when `t` is inside the shift and `out` when it is not;
3. otherwise, `in`.

An override MUST end at the first shift boundary after it was set, even if `override_until` is later
or null. Effective presence MUST be computable from stored columns and the clock alone, so every
instance reaches the same answer without coordinating. An endpoint with no shift and no override MUST
behave exactly as before this spec.

#### Scenario: Default is in

- **WHEN** an endpoint has no shift and no override
- **THEN** its effective presence is `in`

#### Scenario: A shift boundary ends an override

- **WHEN** an endpoint with shift `Mon-Fri 09:00-13:00` clocks out at 10:00 Monday with no end time
- **THEN** it is out until 13:00 Monday, out until 09:00 Tuesday by shift, and in at 09:00 Tuesday

#### Scenario: Override without a shift

- **WHEN** an endpoint with no shift clocks out with no end time
- **THEN** it stays out until it clocks in or its operator sets presence

### Requirement: Shift Schedule

`shift` MUST use the weekly-window grammar Harness defines for `operating_hours`: an optional `TZ=` or
`CRON_TZ=` zone prefix, then `;`-separated windows of an optional day spec (`Mon`–`Sun`, ranges that
may wrap, comma lists) and `HH:MM-HH:MM`. The start is inclusive, the end exclusive, `24:00` is valid
only as an end, and an end at or before its start runs into the next day. Membership MUST be decided
on local wall-clock time in the zone, so a DST transition shortens or lengthens a window rather than
skipping or repeating it. The zone database MUST be embedded, so validation does not depend on the
host. A blank, malformed or unknown-zone value, or a window whose start equals its end, MUST be
rejected with `invalid_argument` naming the field.

Only the endpoint's owning human MAY set or clear a shift. Setting one MUST NOT require a re-vend, and
a shift MUST NOT be part of the endpoint's scope.

#### Scenario: Owner sets a shift

- **WHEN** the owning human sets `shift = "TZ=America/Los_Angeles Mon-Fri 09:00-13:00"` on an active
  endpoint
- **THEN** the endpoint is in on weekdays from 09:00 to 13:00 Pacific, out otherwise, and its
  credential and scope are unchanged

#### Scenario: Invalid shift

- **WHEN** the owner sets `shift = "Mon 09:00-09:00"`
- **THEN** the request fails with `invalid_argument` naming `shift`, and the stored shift is unchanged

### Requirement: Presence Verbs

Every authenticated endpoint session MUST be able to call three verbs, whether or not they appear in
the endpoint's `scope.verbs` (REQ "Self Verbs Are Unscoped"):

* **`clock_out {}`**: set an `out` override with `override_by = agent`. With a shift, it lasts until
  the first shift boundary after it was set — the next boundary, whether that boundary is a shift
  start or a shift end, exactly as REQ "Presence Model" defines and not merely the next shift start.
  Without one, it has no end.
* **`clock_in {for?}`**: `for` is an optional duration, default `1h`, at most `8h`. When the shift
  (if any) would put the endpoint out, set an `in` override ending at the earlier of now plus `for`
  and the next shift start. Otherwise, clear any `out` override. `for` over `8h`, zero, negative or
  unparseable MUST be refused with `invalid_argument`.
* **`presence {}`**: return `presence` (`in`/`out`), `source` (`shift`, `agent`, `operator`,
  `default`), `until` (when effective presence next changes, or null), `shift` (or null), and
  `pending` (the endpoint's push-eligible pending todo count).

`clock_out` and `clock_in` MUST return the same shape as `presence`, reflecting the new state. None of
the three verbs MAY accept an endpoint, queue or todo argument. They MUST act only on the caller's own
endpoint, and MUST be idempotent: repeating one changes nothing further.

#### Scenario: Agent clocks out before stopping

- **WHEN** an agent with no shift calls `clock_out`
- **THEN** the result reports `presence = "out"`, `source = "agent"`, `until = null`, and new
  push-eligible todos on its endpoint ring no session

#### Scenario: Late clock-in outside a shift

- **WHEN** an agent with shift `Mon-Fri 09:00-13:00` calls `clock_in {for: "2h"}` at 20:00 Monday
- **THEN** it is in until 22:00 Monday and out again afterwards

#### Scenario: Clock-in crossing a shift start

- **WHEN** an agent with shift `Mon-Fri 09:00-13:00` calls `clock_in {for: "2h"}` at 08:00 Monday, a
  window in which the shift would put it out
- **THEN** it is in until 09:00 Monday, the earlier of 10:00 (now plus `for`) and the next shift
  start, and the shift decides from 09:00 onward

#### Scenario: Clock-in inside a shift only clears a clock-out

- **WHEN** an agent with shift `Mon-Fri 09:00-13:00` calls `clock_out` at 10:00 Monday and then calls
  `clock_in` at 11:00 Monday
- **THEN** the `out` override is cleared and it is in for the remainder of the shift, ending at
  13:00 Monday when the shift puts it out again

#### Scenario: Clock-out expires at the next boundary, not the next shift start

- **WHEN** an agent with shift `Mon-Fri 09:00-13:00` calls `clock_out` at 10:00 Monday
- **THEN** its `out` override ends at 13:00 Monday, the first boundary after it was set, and is
  governed by the shift from then until 09:00 Tuesday

#### Scenario: Clock-in cap

- **WHEN** an agent calls `clock_in {for: "9h"}`
- **THEN** the call fails with `invalid_argument` and presence is unchanged

#### Scenario: Repeated clock-out

- **WHEN** an agent that is already out calls `clock_out` again
- **THEN** the result is the same state, and no digest or other doorbell is sent

### Requirement: Self Verbs Are Unscoped

`clock_in`, `clock_out` and `presence` are **self verbs**. The boundary check MUST allow them for any
session authenticated to an active endpoint, MUST advertise them in `tools/list` for every such
session, and MUST NOT add them to, or require them in, any stored `scope.verbs`. A self verb MUST NOT
read or change any other endpoint, any todo, or any webhook, and MUST NOT widen what any other verb
may do. A revoked endpoint's sessions are closed (SPEC-0007), so self verbs share revocation with
everything else.

#### Scenario: Endpoint vended before presence existed

- **WHEN** an endpoint whose scope lists only `list_todos` and `claim` calls `tools/list`
- **THEN** the result lists `list_todos`, `claim`, `clock_in`, `clock_out` and `presence`, and
  `clock_out` succeeds

#### Scenario: Self verb cannot target another endpoint

- **WHEN** a session calls `clock_out` with an extra `endpoint` argument
- **THEN** the call is refused with `invalid_argument` and no endpoint's presence changes

### Requirement: Held Doorbells

While an endpoint is out:

* todo creation, dedup, routing and work orders MUST be unchanged;
* `PublishTodoReady` MUST NOT ring any of its sessions (SPEC-0011 REQ "Scope-Filtered Fan-Out");
* the heartbeat sweep MUST NOT select its todos, and MUST NOT change their `ring_attempts` or
  `last_ringed_at` (SPEC-0011 REQ "Doorbell Heartbeat");
* every pull and lease verb (`list_todos`, `claim`, `claim_next`, `heartbeat`, `complete`, `fail`)
  MUST behave exactly as when it is in;
* the reaper and retry scheduling MUST be unchanged.

An instance MUST observe a presence change made on another instance within 5 seconds of the
broadcast, and within 30 seconds in all cases, when the periodic evaluator is the fallback.

#### Scenario: Todo arrives while out

- **WHEN** a verified delivery creates a todo on an endpoint that is out, and that endpoint has a
  session with an open stream
- **THEN** the todo is `pending`, no `notifications/claude/channel` is sent, and its `ring_attempts`
  stays 0

#### Scenario: Pull while out

- **WHEN** an agent that is out calls `claim_next`
- **THEN** it receives a pending todo exactly as it would when in

### Requirement: Clock-In Digest

When an endpoint's effective presence changes from `out` to `in` (a `clock_in`, an operator change, a
shift start, or an override ending), Switchboard MUST send one **digest doorbell** to one session of
that endpoint on each instance that hosts an open-stream session for it and either claimed the
transition or received its broadcast. The digest MUST be sent only
when the endpoint has at least one push-eligible pending todo.

The transition MUST be decided once across all instances and restarts: an instance MUST claim it with
a conditional update of `presence_seen` from `out` to `in` and act only if the update changed a row.
An `in` to `out` change MUST update `presence_seen` the same way, without a digest, so that the next
return is detected. A periodic evaluation (at least every 30 seconds) MUST detect transitions nobody triggered, such as a
shift start or override expiry.

`presence_seen` MUST advance independently of whether any session could receive the digest, and a
transition whose digest is therefore never sent MUST NOT be re-delivered later. Two rules keep that
from losing the summary:

* An out-to-in transition that no instance can deliver because no instance hosts a session for the
  endpoint is recorded and dropped. The endpoint is `in` and the return is covered by the reconnect
  digest, which fires when the first stream opens — so nothing is left unsurfaced.
* A hosting instance that misses the broadcast loses that transition's digest for its own session:
  it cannot claim (the row already moved) and no later evaluation re-delivers it. The summary is not
  left unsurfaced — the backlog stays on the board, and the next doorbell, or the reconnect digest
  if the session drops, surfaces it. The cost is one lost digest, not a late one.
* If an instance's conditional update loses the claim (another instance already advanced
  `presence_seen`, including across a restart), it MUST NOT broadcast and MUST NOT send a digest of
  its own; whether *it* hosts a session is irrelevant, because the claim is what keeps delivery
  at-most-once per session, not session locality. An instance that hosts a session for an endpoint
  whose `presence_seen` already agrees with the effective presence has nothing to deliver for that
  transition.

An override expiring during an instance's restart is therefore decided by whichever instance next
runs the evaluator: the conditional update is on the row, not on process state, so a restart cannot
double-decide a transition or skip one it never observed.

The broadcast that tells the other instances MUST be emitted by the claiming instance and by it
alone, after its conditional update has returned a row. An instance that lost the claim MUST NOT
broadcast, or a single transition produces one broadcast per evaluating instance and every hosting
instance sends a digest for each — the "exactly one" property is established by the claim, so the
notification must inherit that single ownership rather than be emitted independently of it. Each
instance that receives an `in` broadcast and hosts a session for that endpoint emits at most one
digest for it, so a transition yields one digest per hosting instance and never two on one instance.
Sessions of one endpoint each live on exactly one instance, so no session can receive the same
transition's digest twice.

When an endpoint that is `in` gains its first open notification stream on an instance, and it has at
least one push-eligible pending todo, that instance MUST send a digest with reason `reconnect`, at
most once per endpoint per 10 minutes per instance.

After sending a digest, the instance MUST set `last_ringed_at` to the digest time on every todo the
digest counted, without incrementing `ring_attempts`. Only a digest actually written to a session
marks its todos (REQ "Presence Error Handling and Audit").

The rows marked MUST be the rows counted, and the mark MUST NOT be recomputed from a second,
independent read of the backlog. Counting and marking are separated by a network write, so a todo
created or claimed in between must not be silently swept into the mark or dropped from it:

* A todo created after the count is not marked, and is rung on its own normal schedule — it was never
  summarized, so the digest did not tell the agent about it.
* A todo claimed or completed between the count and the mark MUST NOT have `last_ringed_at` written
  by the digest, because `last_ringed_at` is only meaningful for a `pending` row and the claim
  already took it out of the sweep's candidate set.
* The mark MUST be scoped to the counted ids, not to a queue or endpoint predicate, so a row that
  arrived in the meantime cannot be marked by accident.

Because the mark sets `last_ringed_at` without spending an attempt, a todo the digest summarized
waits its normal backoff from the digest time before the sweep considers it again. That is the point:
the digest just told the agent the backlog is there, so re-ringing it one row at a time would spend
exactly the turns presence exists to save. It cannot starve a todo, because the digest is sent only
when the endpoint is `in` and a session can receive it — the same condition under which the sweep
runs — and every summarized todo's next backoff step still fires. It cannot double-ring a todo
either, because the mark moves `last_ringed_at` forward, so the sweep's backoff predicate is false
for that todo until the backoff elapses again.

#### Scenario: Shift start

- **WHEN** an endpoint with shift `Mon-Fri 09:00-13:00` has 7 pending todos at 09:00 Tuesday and one
  session with an open stream
- **THEN** that session receives exactly one digest with `meta.reason = "shift_start"` and
  `meta.pending = "7"`, and none of the 7 todos is rung individually until its backoff from 09:00
  elapses

#### Scenario: Two instances see the same transition

- **WHEN** two instances evaluate the same shift start concurrently
- **THEN** exactly one claims the transition, only the claimant broadcasts it, and no session
  receives two digests for it

#### Scenario: Two instances host sessions for one transition

- **WHEN** two instances both host a session for one endpoint, and that endpoint's presence goes
  from `out` to `in`
- **THEN** each instance delivers one digest to one of its own sessions, and neither instance's
  session receives a second digest for the same transition

#### Scenario: Reconnect after a night off

- **WHEN** an endpoint with no shift and no override has 3 pending todos, no session all night, and a
  session opens its stream at 09:00
- **THEN** that session receives one digest with `meta.reason = "reconnect"` and `meta.pending = "3"`

#### Scenario: Reconnect loop

- **WHEN** a crashing agent reconnects every 30 seconds with pending todos waiting
- **THEN** it receives at most one reconnect digest per 10 minutes

#### Scenario: Nothing waiting

- **WHEN** an endpoint clocks in with no pending push-eligible todos
- **THEN** no digest is sent

#### Scenario: Digest does not starve or double-ring a todo

- **WHEN** a digest counts a todo and then marks it
- **THEN** that todo's `last_ringed_at` moves to the digest time and its `ring_attempts` is
  unchanged, so the sweep waits a further backoff from the digest and does not ring it twice for the
  same window

#### Scenario: A todo arriving during the digest is not swept into the mark

- **WHEN** a todo is created after the digest counted the backlog but before the mark is written
- **THEN** that todo's `last_ringed_at` is unchanged by the digest, and it is rung on its own normal
  schedule because the digest never summarized it

#### Scenario: Transition claimed while no session can receive it

- **WHEN** an endpoint's override expires while it has no session on any instance
- **THEN** the transition is recorded and no digest is sent, and the return is covered by the
  reconnect digest when the first stream opens rather than by a retroactive transition digest

#### Scenario: An override expires during a restart

- **WHEN** an instance restarts across the moment an `out` override would have ended, and the
  evaluator runs again after it comes back
- **THEN** the conditional update on `presence_seen` decides the transition exactly once, because the
  claim is a row update and not process state, and no second digest is produced for it

### Requirement: Operator Presence Controls

The operator API MUST expose, under the same OAuth guard as every other `/api/v1` route:

* `GET /api/v1/endpoints/{ref}/presence`, returning the `presence` verb's shape;
* `PUT /api/v1/endpoints/{ref}/presence` with `{"presence": "in"|"out", "until": RFC3339|null}`,
  setting an override with `override_by = operator:<human_id>`. Unlike the agent's `clock_in`, it has
  no duration cap, and it still ends at the first shift boundary after it was set. With no shift and
  no `until` it has no end and lasts until the operator clears it or sets presence again — the same
  shape an agent's `clock_out` has, minus the cap on the other direction;
* `DELETE /api/v1/endpoints/{ref}/presence`, clearing any override;
* `PUT /api/v1/endpoints/{ref}/shift` with `{"shift": string|null}`.

These routes MUST answer not-found for an endpoint the caller does not own (as revoke does) and 409
for a revoked endpoint. The CLI MUST wrap them as `switchboard endpoint clock-in REF [--until T]`,
`switchboard endpoint clock-out REF [--until T]`, `switchboard endpoint presence REF`, and
`switchboard endpoint shift REF [EXPR|--clear]`.

The board's endpoint card MUST show effective presence, its source, and when it next changes, with a
control to clock the endpoint in or out and an editor for the shift. An endpoint that is out MUST NOT
be shown with the deaf-consumer warning. The operator's view of held todos MUST say they are held and
until when.

#### Scenario: Operator clocks an agent out

- **WHEN** the owner sends `PUT /api/v1/endpoints/my-agent-k3x9/presence` with
  `{"presence": "out", "until": null}`
- **THEN** the endpoint is out with `source = operator`, and the board card shows it as off shift

#### Scenario: Foreign endpoint

- **WHEN** a human sends `PUT .../presence` for an endpoint they do not own
- **THEN** the response is not-found and nothing changes

### Requirement: Presence Error Handling and Audit

Every presence change MUST record who made it (`agent`, `operator:<human_id>`, or `shift`) and when,
on the endpoint row, and MUST emit a structured log line with the endpoint slug, old and new effective
presence, source, and `until`. A digest that cannot be written to a session's stream MUST be logged
and dropped under SPEC-0011's lossy rules. Its todos keep `last_ringed_at` unchanged, because only a
digest actually written marks them. No presence operation may log a credential.

#### Scenario: Digest write fails

- **WHEN** a digest write to a session's stream fails
- **THEN** the failure is logged with endpoint and reason, and the summarized todos' `last_ringed_at`
  is unchanged

## Security Requirements

* Self verbs MUST be limited to the caller's own endpoint, and MUST NOT accept identifiers that could
  address another endpoint (REQ "Self Verbs Are Unscoped").
* An agent-set `in` override outside a shift MUST be capped at 8 hours, so a prompt injection cannot
  keep an agent awake indefinitely against its human's shift.
* Digest `content` and `meta` MUST contain only counts, queue names and reasons: never todo titles,
  payloads, or anything else derived from a webhook body.
* Queue names are operator-defined (an ingestion route or a webhook's target queue, chosen by the
  human who wrote the rule), never taken from a delivery body, so they are not attacker-controlled.
  They MUST nevertheless be passed through the same neutralization the todo doorbell applies to
  `meta.todo_id`, because a queue name is still operator-supplied text landing inside a
  `notifications/claude/channel` frame: a name containing a newline or a `</channel>` sequence would
  otherwise be able to break the frame it is quoted in.
* The digest MUST NOT be addressed by any caller — it is emitted only by the server for the
  endpoint's own transition — so it MUST NOT be usable to read another endpoint's counts. The
  `pending` and `queues` figures MUST be computed for the caller's endpoint alone, and the operator
  routes MUST apply the same ownership check the other `/api/v1/endpoints/{ref}` routes do, so one
  human cannot read or change another human's presence or backlog counts.
* A prompt injection that makes an agent call `clock_out` MUST NOT be able to do more than quiet that
  endpoint until the next shift boundary or a `clock_in`: presence cannot refuse a pull, hide work
  from `list_todos`, move a todo, or widen scope. A repeated `clock_in` injection is bounded by the
  8-hour cap and by the shift boundary the override always yields to, so an injected agent cannot
  hold itself awake past its human's shift.
* Operator presence routes inherit the operator OAuth guard, ownership check and rate limits of the
  existing `/api/v1/endpoints/{ref}` routes.
