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
  the next shift start. Without one, it has no end.
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

An instance MUST observe a presence change made on another instance within 5 seconds.

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
that endpoint on each instance that hosts an open-stream session for it. The digest MUST be sent only
when the endpoint has at least one push-eligible pending todo.

The transition MUST be decided once across all instances and restarts: an instance MUST claim it with
a conditional update of `presence_seen` from `out` to `in` and act only if the update changed a row.
An `in` to `out` change MUST update `presence_seen` the same way, without a digest, so that the next
return is detected. A periodic evaluation (at least every 30 seconds) MUST detect transitions nobody triggered, such as a
shift start or override expiry. A change of `presence_seen` MUST be broadcast to the other instances
(for example over `LISTEN/NOTIFY`) so each can deliver to its own sessions.

When an endpoint that is `in` gains its first open notification stream on an instance, and it has at
least one push-eligible pending todo, that instance MUST send a digest with reason `reconnect`, at
most once per endpoint per 10 minutes per instance.

After sending a digest, the instance MUST set `last_ringed_at` to the digest time on every todo the
digest counted, without incrementing `ring_attempts`.

#### Scenario: Shift start

- **WHEN** an endpoint with shift `Mon-Fri 09:00-13:00` has 7 pending todos at 09:00 Tuesday and one
  session with an open stream
- **THEN** that session receives exactly one digest with `meta.reason = "shift_start"` and
  `meta.pending = "7"`, and none of the 7 todos is rung individually until its backoff from 09:00
  elapses

#### Scenario: Two instances see the same transition

- **WHEN** two instances evaluate the same shift start concurrently
- **THEN** exactly one claims the transition, and no session receives two digests for it

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

### Requirement: Operator Presence Controls

The operator API MUST expose, under the same OAuth guard as every other `/api/v1` route:

* `GET /api/v1/endpoints/{ref}/presence`, returning the `presence` verb's shape;
* `PUT /api/v1/endpoints/{ref}/presence` with `{"presence": "in"|"out", "until": RFC3339|null}`,
  setting an override with `override_by = operator:<human_id>`. It has no duration cap, and still
  ends at the next shift boundary;
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
* Operator presence routes inherit the operator OAuth guard, ownership check and rate limits of the
  existing `/api/v1/endpoints/{ref}` routes.
