---
status: draft
date: 2026-09-22
implements: [ADR-0035]
requires: [SPEC-0003, SPEC-0006, SPEC-0011, SPEC-0015, SPEC-0023]
related: [SPEC-0022]
---

# SPEC-0030: Per-Queue Admission Control

## Overview

A queue's owner can give it an **admission policy**: a ceiling on how many of its todos may be claimed
at once (`max_in_flight`) and how many claims it may admit per declared window
(`max_claims_per_window`). The policy is enforced **inside the claim transaction**, so concurrent
claimers, replicas, retries and lease takeovers can never admit more than the limit. Work over the limit
is not dropped. It stays `pending`, keeps its attempts, and says why it is waiting and when it becomes
eligible, on the claim response, on the board and in the metrics. See
[ADR-0035](../../../adrs/ADR-0035-per-queue-admission-control.md).

This spec amends [SPEC-0003](../todo-queue/spec.md) REQ "Atomic Claim with FOR UPDATE SKIP LOCKED" (a
budgeted claim also takes the policy lock), [SPEC-0006](../agent-tools/spec.md) REQ "Todo Drain Verbs"
(`claim_next` reports deferred queues, `claim` gains the `deferred` error), and
[SPEC-0011](../channels/spec.md) REQs "Scope-Filtered Fan-Out" and "Doorbell Heartbeat" (no push for a
deferred queue). It adds series to [SPEC-0023](../metrics/spec.md) and a queue header and chip to the
board ([SPEC-0015](../operator-board-v2/spec.md)).

Parallel records, cited by number until they merge: queue identity and team roles are ADR-0038 /
SPEC-0033; notify hooks are SPEC-0024; attempt history is ADR-0039 / SPEC-0034; the queue digest is
ADR-0034 / SPEC-0029.

Terms:

* **Queue identity**: a queue as ADR-0038 identifies it, `(endpoint_id, queue)` for an endpoint queue,
  or `(team_id, queue)` for a team queue.
* **Policy**: the admission settings attached to one queue identity.
* **Window**: a fixed calendar interval (an hour, a day or a week) that starts at a declared local time
  in a declared IANA zone.
* **Charge**: one unit of the current window's budget, spent by a committed claim.
* **In flight**: todos of the queue identity in state `claimed`, whether their lease is live or has
  lapsed but not yet been reaped.
* **Deferred**: a queue identity whose policy currently admits no claim, and each pending todo in it.

## Requirements

### Requirement: REQ-1 Admission Policy Model

A queue identity MAY carry at most one policy. A policy MUST carry:

* its queue identity;
* `max_in_flight`, an integer `0`–`10000`, or null for no in-flight limit;
* `max_claims_per_window`, an integer `0`–`1000000`, or null for no window limit;
* `window`, required whenever `max_claims_per_window` is set: `period` (`hour`, `day` or `week`), `tz`
  (an IANA zone name) and `start` (`HH:MM`, 24-hour, plus `weekday` `Mon`–`Sun` when `period` is
  `week`);
* audit fields: `created_by_human_id`, `updated_by_human_id`, `created_at`, `updated_at`.

At least one of the two limits MUST be non-null. A limit of `0` MUST pause the queue: it admits no
claim. A queue identity with no policy MUST behave exactly as before this spec: its claims MUST NOT
lock, write or wait on any admission row, and the only admission cost they MAY pay is one indexed
lookup that finds no policy. A request that is malformed, has an unknown zone, sets `start` on a
period that does not accept it, or sets neither limit MUST be refused with `invalid_argument`, naming
the field, and MUST leave any stored policy unchanged.

#### Scenario: No policy, no change

- **WHEN** a queue identity has no policy
- **THEN** `claim` and `claim_next` on it behave exactly as SPEC-0003 defines, and take no admission
  lock

#### Scenario: A daily budget

- **WHEN** an owner sets `max_claims_per_window = 100` with window `{period: day, tz:
  America/Los_Angeles, start: 00:00}` on `(endpoint E, investigations)`
- **THEN** at most 100 claims of that queue identity commit between two consecutive Pacific midnights

#### Scenario: Zero pauses the queue

- **WHEN** an owner sets `max_in_flight = 0`
- **THEN** every claim on that queue identity is refused with reason `paused`, and pending todos stay
  pending

#### Scenario: Unknown zone is refused

- **WHEN** an owner sets `window.tz = "Mars/Olympus"`
- **THEN** the request fails with `invalid_argument` naming `window.tz`, and the stored policy is
  unchanged

### Requirement: REQ-2 Policy Scope

A policy MUST govern every todo whose queue identity matches it. For an endpoint queue, that covers
every session and worker authenticated to that endpoint. For a team queue, it covers every endpoint
that drains that team queue. A policy MUST NOT govern todos of any other queue identity, including a
queue with the same name on another endpoint, another owner's endpoint, or a fan-out copy of the same
event delivered to another endpoint (ADR-0022). Each of those is governed only by its own queue
identity's policy.

#### Scenario: A pool shares one budget

- **WHEN** three sessions on endpoint E compete on `lane-m`, which has `max_claims_per_window = 20`
- **THEN** the three together commit at most 20 claims in the window

#### Scenario: Same name, different owners

- **WHEN** human A's endpoint and human B's endpoint each drain a queue named `investigations`, and only
  A's has a policy
- **THEN** A's policy never refuses, charges or delays a claim on B's queue, and B's claims never spend
  A's budget

#### Scenario: A fan-out copy is governed by its own endpoint

- **WHEN** a webhook fans a delivery out to A's endpoint and to a friend's endpoint
- **THEN** A's todo is charged to A's queue identity and the friend's todo to the friend's, if the
  friend has a policy

### Requirement: REQ-3 Charging Rule

Every committed claim on a queue identity with a window limit MUST charge exactly one unit to the
window that contains the commit time. That covers every claim path: `claim`, `claim_next` and the
operator's board claim. It includes a first claim, a claim after a retry's backoff, a claim after
"Retry now", and a claim that takes over a lapsed lease. A charge MUST NOT be refunded by `complete`,
`fail`, `release`, the lease reaper, an operator retry, or any policy change. A claim that admission
refuses MUST NOT be charged. An ingest delivery that dedupes onto an existing todo MUST NOT be charged.
The number of units charged in a window MUST equal the number of claims of that queue identity
committed in it.

#### Scenario: A retry is charged

- **WHEN** a todo is claimed, fails below `max_attempts`, waits out its backoff and is claimed again
  within one window
- **THEN** the window's usage rises by two

#### Scenario: A takeover is charged

- **WHEN** a worker claims a todo whose previous claimant's lease has lapsed
- **THEN** the claim is charged one unit

#### Scenario: Release does not refund

- **WHEN** a worker claims a todo and releases it
- **THEN** the window's usage is unchanged by the release, and the next claim of the todo is charged
  again

#### Scenario: A redelivery is free

- **WHEN** a provider redelivers a webhook with the same delivery id, and ingest dedupes it onto the
  existing todo
- **THEN** no unit is charged

### Requirement: REQ-4 Atomic Enforcement at Claim

On a queue identity with a policy, the admission check, the charge and the claim MUST commit in a
single database transaction, or not at all. The transaction MUST lock the candidate todo first (as
SPEC-0003 defines), then lock the queue identity's policy row with a blocking row lock. It MUST read
the in-flight count and the current window's usage under that lock. Implementations MUST NOT take the
policy lock before a todo lock, and MUST NOT wait on a todo lock while holding the policy lock. A
transaction MUST hold at most one policy lock at a time. After a refusal it MUST release the todo and
policy locks (by ending the transaction, or rolling back to a savepoint taken before the candidate
pick) before it picks a candidate in another queue.

A claim MUST be refused when either of these holds:

* the in-flight count, not counting the candidate itself when the claim takes over the candidate's
  lapsed lease, is at or above `max_in_flight`;
* the window's usage is at or above `max_claims_per_window`.

These limits MUST hold with any number of concurrent claimers on any number of server instances
sharing one database.

`claim_next` MAY skip queues that an unlocked read shows to be exhausted before it picks a candidate.
It MUST re-check under the lock. When the locked check refuses, it MUST go on to its other granted
queues. It MUST NOT return `empty` while a claimable todo remains in an admitted queue.

#### Scenario: Concurrent claimers cannot exceed the limit

- **WHEN** 50 `claim_next` calls race on a queue with `max_claims_per_window = 10` and 50 pending todos,
  spread across two server instances
- **THEN** exactly 10 claims commit, and the other 40 calls return no todo from that queue

#### Scenario: In-flight cap

- **WHEN** a queue with `max_in_flight = 2` has two claimed todos and a third worker calls `claim_next`
- **THEN** the third call is refused for that queue with reason `in_flight_full`; after one of the two
  completes, the next call succeeds

#### Scenario: Takeover at the in-flight cap

- **WHEN** a queue with `max_in_flight = 1` has one claimed todo whose lease has lapsed and not been
  reaped, and a worker claims that same todo
- **THEN** the claim is admitted, because it replaces the lapsed claim rather than adding one

#### Scenario: One deferred queue does not starve the others

- **WHEN** an endpoint granted `triage` and `lane-s` calls `claim_next`, the oldest pending todo is in
  `lane-s`, and `lane-s` is exhausted
- **THEN** the call claims the oldest claimable todo in `triage`

### Requirement: REQ-5 Windows

A window MUST be a fixed interval whose boundaries fall at `start` local wall-clock time in `tz`: every
hour at the `start` minute, every day at `start`, or every week at `weekday` `start`. Membership MUST be
decided on local wall-clock time, so a daylight-saving transition lengthens or shortens a window rather
than skipping or repeating it. A `start` that falls in a daylight-saving gap MUST resolve to the first
valid instant after the gap. The zone database MUST be embedded, as in SPEC-0022 REQ "Shift Schedule",
so validation and evaluation do not depend on the host. Usage MUST be stored per `(policy,
window_start)`, where `window_start` is the absolute instant the window began. When a window ends, the
queue identity MUST start the next window at zero usage, and nothing MUST be written to any todo for
that to happen.

#### Scenario: A Pacific day

- **WHEN** a policy has window `{period: day, tz: America/Los_Angeles, start: 00:00}` and its budget is
  exhausted at 17:00 Pacific
- **THEN** `next_eligible_at` is the next 00:00 Pacific, and claims are admitted again from that instant

#### Scenario: The fall-back day

- **WHEN** the window's day contains the autumn daylight-saving transition in its zone
- **THEN** that window is 25 hours long, and its budget is one budget

#### Scenario: Hourly window

- **WHEN** a policy has window `{period: hour, tz: UTC, start: 00:30}`
- **THEN** windows begin at half past every hour, UTC

### Requirement: REQ-6 Deferred Work Is Retained

A pending todo in a deferred queue identity MUST stay in state `pending`. Its `attempt`, `max_attempts`
and `next_retry_at` MUST NOT change because of admission. It MUST NOT be dead-lettered, pruned by
retention, or lose its dedup slot for being deferred. A failed todo whose retry backoff elapses while
its queue is deferred MUST return to `pending` as SPEC-0003 defines, and then wait for admission.

Each pending todo's admission status MUST be derived when it is read, never stored. The status carries:

* `state`: `admitted` or `deferred`;
* `reason`: `window_exhausted`, `in_flight_full`, `paused` or `admission_unavailable`;
* `next_eligible_at`: for `window_exhausted`, the next window boundary, and `exact: true`. For
  `in_flight_full`, the earliest live lease expiry among the in-flight todos, and `exact: false`,
  because a slot usually frees sooner. For `paused` and `admission_unavailable`, null.

`list_todos`, the todo drawer and the board MUST include it on every pending todo of a queue
identity with a policy.

#### Scenario: An exhausted day keeps its work

- **WHEN** a queue's daily budget is exhausted at 10:00 and 40 todos arrive before midnight
- **THEN** all 40 are `pending` at 23:59 with `attempt` unchanged, each reports `reason:
  window_exhausted` and the next midnight, and none is dead-lettered

#### Scenario: Deferral outlives the retry backoff

- **WHEN** a todo fails at 16:00, its backoff elapses at 16:01, and its queue is exhausted until
  midnight
- **THEN** it returns to `pending` at 16:01, stays deferred until midnight, and its `attempt` is what
  the failed claim left it at

### Requirement: REQ-7 Claim Responses

`claim_next` MUST skip deferred queue identities and continue with the caller's other granted queues.
When it claims nothing, and at least one granted queue identity is deferred with pending todos, it MUST
return `empty: true` together with a `deferred` list. Each entry carries `queue`, `reason`, `pending`
(the count of pending todos) and `next_eligible_at`. `empty: true` without `deferred` MUST keep meaning
that there is no pending work in any granted queue.

`claim` of a specific todo in a deferred queue identity MUST fail with the new error code `deferred`,
carrying `queue`, `reason` and `next_eligible_at`, and MUST NOT charge or change the todo.

A successful claim on a queue identity with a policy MUST return `admission` beside the todo:
`max_in_flight`, `in_flight`, `max_claims_per_window`, `used` (after this claim's charge), `remaining`
and `window_resets_at`.

Every endpoint that is granted `claim` or `claim_next` MUST also be able to call the read-only verb
`admission_status {queue?}`. It returns the policy, usage and status of each queue identity in the
caller's scope, and nothing about any queue outside it.

#### Scenario: A worker is told to wait, not to poll

- **WHEN** a worker's only granted queue is exhausted until 00:00 Pacific and has 12 pending todos
- **THEN** `claim_next` returns `{empty: true, deferred: [{queue, reason: window_exhausted, pending: 12,
  next_eligible_at}]}`

#### Scenario: Headroom on a successful claim

- **WHEN** a worker claims the 73rd todo of a queue with `max_claims_per_window = 100`
- **THEN** the response carries `used: 73`, `remaining: 27` and `window_resets_at`

#### Scenario: A specific claim in a deferred queue

- **WHEN** a worker calls `claim` on a todo whose queue is paused
- **THEN** the call fails with code `deferred` and `reason: paused`, and the todo is unchanged

#### Scenario: Status is scoped to the caller

- **WHEN** a worker calls `admission_status` naming a queue outside its endpoint's scope
- **THEN** the call fails with `forbidden`, as any out-of-scope queue does (SPEC-0006), and reveals
  nothing about that queue

### Requirement: REQ-8 Doorbells and Hooks Respect Admission

Switchboard MUST NOT push for a todo while its queue identity is deferred. That covers a channel
doorbell on creation, a ring on attach, the doorbell heartbeat (SPEC-0011) and a notify hook call
(ADR-0029, SPEC-0024). When a deferred queue identity becomes admitted, it MUST send one doorbell per
endpoint and queue that has push-eligible pending todos, subject to that endpoint's presence
(SPEC-0022). A queue identity becomes admitted:

* at a window boundary;
* when an in-flight slot frees through `complete`, `fail`, `release` or the reaper;
* when its policy is raised or removed.

The reopen sweep MUST run in the background, MUST be exempt from endpoint scoping on the same terms as
the lease reaper (SPEC-0003), and MUST NOT send a doorbell for a queue that is still deferred.

#### Scenario: No wake-up for work that cannot be claimed

- **WHEN** a push-eligible todo is created in an exhausted queue identity that has a notify hook and a
  live session
- **THEN** no doorbell is sent and no hook is called

#### Scenario: One ring at midnight

- **WHEN** the window boundary passes for an exhausted queue identity with 40 pending, push-eligible
  todos
- **THEN** exactly one doorbell is sent for that endpoint and queue, and the notify hook, if any, is
  called once

#### Scenario: Reopening while clocked out

- **WHEN** a queue identity becomes admitted while its endpoint is clocked out
- **THEN** no doorbell is sent until the endpoint clocks in, when the SPEC-0022 digest covers the
  waiting work

### Requirement: REQ-9 Fail Closed

When the policy for a candidate's queue identity cannot be read or evaluated, the claim MUST be refused
with reason `admission_unavailable`, and the todo MUST stay pending. That covers a database error, a
row that fails validation, and a stored zone that the embedded database no longer knows. Such a
failure MUST increment `switchboard_admission_refusals_total{reason="admission_unavailable"}` and MUST
be shown on the queue's board header. It MUST NOT admit the claim. Queue identities without a policy
MUST be unaffected by a failure on another queue's policy.

#### Scenario: A policy that cannot be evaluated

- **WHEN** a stored policy names a zone that the embedded zone database does not know
- **THEN** every claim on that queue identity is refused with `admission_unavailable`, the board shows
  the fault, and claims on the endpoint's other queues proceed

### Requirement: REQ-10 Policy Management

Policies MUST be managed only by a human authorized for the queue identity's owner scope:

* for an endpoint queue, the endpoint's owning human, or its team's configuring role when the endpoint
  is team-owned (ADR-0038);
* for a team queue, the team's configuring role (ADR-0038).

Management MUST be available on:

* the web UI;
* the operator API, as `GET /api/v1/admission`, and `GET`, `PUT` and `DELETE` on
  `/api/v1/admission/{scope}/{queue}`;
* the operator CLI, as `switchboard queue budget show|set|clear`.

A vended endpoint credential MUST NOT create, change or delete a policy, and there MUST be no MCP
write verb for policies. A request naming a queue identity the caller may not manage MUST return
`not_found`, indistinguishable from one that does not exist.

Every create, change and delete MUST append an audit record: who, when, the before value and the after
value. The owner scope MUST be able to read that record. Changes take effect on the next claim:

* lowering a limit below current usage refuses new claims, without revoking claims in flight;
* raising a limit admits immediately;
* changing a window's `period`, `tz` or `start` begins counting in the window the new definition
  places the current instant in, and the change is audited.

#### Scenario: An agent cannot raise its own ceiling

- **WHEN** a vended endpoint attempts to change its queue's policy by any means available to it
- **THEN** no such operation exists or succeeds, and the policy is unchanged

#### Scenario: Another human's queue is opaque

- **WHEN** human B calls `GET /api/v1/admission/{scope}/{queue}` naming a queue identity of human A's
  endpoint
- **THEN** the response is `404 not_found`, the same as for a queue identity that does not exist

#### Scenario: Lowering mid-window

- **WHEN** an owner lowers `max_claims_per_window` from 100 to 50 after 60 claims today
- **THEN** no further claim is admitted until the next window, the 60 claims already committed stand,
  and the audit record shows the change

### Requirement: REQ-11 Board Visibility

The board and the todos view (SPEC-0015) MUST show, for each queue identity with a policy that the
viewer may see:

* in the queue's header: `used` of `max_claims_per_window`, `in_flight` of `max_in_flight`, the status,
  and "resets at", shown in the policy's zone and in the viewer's local time;
* on each deferred pending todo: a chip with the reason and `next_eligible_at`.

An exhausted, paused or unavailable queue MUST be visually distinct from an admitted one. The header
counts MUST update live over the existing SSE fragments. The web UI MUST offer the owner scope a
policy editor and the audit history.

#### Scenario: The owner sees why work waits

- **WHEN** the owner opens the board while `investigations` is exhausted with 12 pending todos
- **THEN** the header reads 100 of 100 used, with the reset time, and each of the 12 cards carries a
  "deferred · budget · eligible 00:00 PT" chip

### Requirement: REQ-12 Metrics

Switchboard MUST add these series to `/metrics` (SPEC-0023):

```
switchboard_admission_policies{queue,status}          gauge    # status: admitted|window_exhausted|in_flight_full|paused|admission_unavailable
switchboard_admission_deferred_todos{queue,reason}    gauge
switchboard_admission_charged_total{queue}            counter
switchboard_admission_refusals_total{queue,reason}    counter
```

The series MUST obey SPEC-0023 REQ-5 and REQ-6: no owner, endpoint, team or todo label; `queue`
capped, with overflow as `__other__`; zero values reported for every status of every policy queue; and
omission, never zero, when the collector cannot compute a value. Per-owner limits and usage MUST NOT
appear in `/metrics`. They belong to the owner scope's board and API.

#### Scenario: A budget shows up in Grafana

- **WHEN** a queue exhausts its daily budget and 30 todos wait
- **THEN** `switchboard_admission_policies{status="window_exhausted"}` is at least 1 for that queue
  label, and `switchboard_admission_deferred_todos{reason="window_exhausted"}` reads 30

#### Scenario: Charges match claims

- **WHEN** a budgeted queue commits 40 claims in an hour
- **THEN** `switchboard_admission_charged_total` for that queue label rises by 40, as
  `switchboard_todos_claimed_total` does

### Requirement: REQ-13 Concurrency and Database Standards

The claim transaction MUST use parameterized queries and one transaction per claim, and MUST return
its connection to the pool on every path. It MUST propagate the request context, so a cancelled claim
rolls back without charging. The reopen sweep MUST start and stop with the server's lifecycle, as the
reaper does. The admission tests MUST run under the race detector in CI, beside the existing queue
packages.

#### Scenario: A cancelled claim charges nothing

- **WHEN** a client disconnects after the policy lock is taken and before the commit
- **THEN** the transaction rolls back, and neither the todo nor the window counter changes

## Security Requirements

* **Authentication.** Every surface in this spec requires authentication.

  | Surface | Auth | Notes |
  |---|---|---|
  | `GET /api/v1/admission` | Required | Operator OAuth grant for the calling human; lists only queue identities that human may manage |
  | `GET`, `PUT` and `DELETE` on `/api/v1/admission/{scope}/{queue}` | Required | Same grant; foreign and unknown identities both `404` |
  | web UI policy editor and board header | Required | Session-authenticated human; owner scope only |
  | MCP `admission_status`, and `admission` on claim responses | Required | Vended endpoint bearer; caller's scope only; read-only |
  | `/metrics` series | Required | Scrape credential (SPEC-0023 REQ-1); aggregate, no tenant labels |

* **Rate limiting.** The admission API routes MUST sit behind a per-human limiter (at most 1 write per
  second, burst 10), since the operator API mount has none today. The MCP verb inherits the vended
  surface's per-endpoint limiter. Admission adds no unauthenticated surface.
* **Security headers.** The web UI and the API responses keep the existing CSP, `X-Frame-Options: DENY`,
  `X-Content-Type-Options: nosniff` and `Referrer-Policy` headers unchanged.
* **Request body size.** `PUT /api/v1/admission/...` bodies MUST be bounded at 16 KiB.
* **CSRF.** The web UI's policy editor posts use the existing CSRF protection for state-changing
  forms. The operator API is bearer-authenticated and not cookie-authenticated.
* **Redirects.** No surface in this spec redirects to a user-supplied URL.
* **Tenancy.** Every policy read and write, every admission status, and every audit record is filtered
  by the caller's owner scope. The deferral detail on a claim response describes only the caller's own
  queue identities. The reopen sweep is exempt from scoping, and it reads no todo out to anyone.
* **No self-escalation.** Agents can read their budget and cannot change it. Nothing a delivery
  carries can change a policy.

## Accessibility Requirements

This spec adds a board header, a chip and a policy editor. They MUST meet WCAG 2.1 AA, as the rest of
the board does (SPEC-0015):

* the live header counts MUST sit in an `aria-live="polite"` region, and a change to exhausted, paused
  or unavailable MUST be announced;
* the reason chip MUST carry a text label, not colour alone;
* the policy editor MUST be keyboard-operable, and MUST follow the board's modal focus-trap and return
  rules;
* icon-only controls, such as "edit budget", MUST carry an `aria-label`.
