---
status: draft
date: 2026-09-22
implements: [ADR-0030]
requires: [SPEC-0003, SPEC-0006, SPEC-0007, SPEC-0011, SPEC-0014, SPEC-0023]
related: [SPEC-0022]
---

# SPEC-0025: Doorbell Acknowledgement and End-to-End Self-Test

## Overview

A doorbell that Switchboard writes to a session is not necessarily a doorbell that an agent heard.
Claude Code drops channel notifications silently when a session has not loaded the server as a
channel, and returns no error. This spec makes that difference observable and testable. See
[ADR-0030](../../../adrs/ADR-0030-doorbell-acknowledgement-and-self-test.md).

It defines:

* **ring records**, with the states `sent`, `acknowledged` and `unheard`;
* acknowledgement **by claim**, or by the lightweight self verb `ack_doorbell`;
* per-session **unheard** detection;
* the **unheard-rings metrics**, added to the SPEC-0023 registry;
* the **self-test**: the `test_doorbell` and `get_doorbell_test` self verbs, the operator route
  `POST /api/v1/endpoints/{ref}/doorbell-tests`, and `switchboard doctor`;
* **synthetic todos**: marked, auto-cleaned, and excluded from metrics, pulls and hooks.

It amends [SPEC-0011](../channels/spec.md) REQ "Push Notification Shape" (a new `meta.ring_id`)
and the deaf-consumer handling in REQ "Best-Effort Lossy Delivery and Degradation to Pull". It
relies on the **self verb** mechanism that [SPEC-0022](../endpoint-presence/spec.md) REQ "Self
Verbs Are Unscoped" defines. If this spec is implemented first, it introduces that mechanism
exactly as SPEC-0022 describes it.

## Requirements

### REQ-1: Ring Records

Every doorbell that Switchboard **successfully writes** to a session's transport MUST create one
ring record. The doorbell may be a todo doorbell, a SPEC-0022 digest, or a self-test doorbell. The
record MUST hold:

* `ring_id` (`rg_<ULID>`);
* `endpoint_id` and `session_id`;
* `todo_id` (null for a digest);
* `kind` (`todo`, `digest` or `test`);
* `queue` (null for a digest);
* `synthetic` (true only for test doorbells);
* `sent_at`;
* `acked_at`, `ack_via` (`claim` or `ack`) and `acked_session_id`;
* `unheard_at`.

A write that fails MUST NOT create a ring. It MUST continue to count toward the existing
consecutive-write-failure warning.

The ring write MUST NOT block the notification pump. A failure to record a ring MUST be logged and
counted in `switchboard_metrics_collection_errors_total{collector="doorbell_rings"}`, and MUST
NOT drop the doorbell.

Ring records MUST be deleted 7 days after `sent_at`.

#### Scenario: A successful write creates a ring

- **WHEN** a todo doorbell is written to session S without error
- **THEN** one ring exists with `kind = "todo"`, that `todo_id`, `session_id = S`, and `sent_at`
  set, and with `acked_at` and `unheard_at` null

#### Scenario: A failed write creates no ring

- **WHEN** a doorbell write fails because no stream is open
- **THEN** no ring is created, and the session's consecutive-write-failure count increments as
  before

### REQ-2: Ring Identifier on the Doorbell

Every todo doorbell, digest doorbell and test doorbell MUST carry `meta.ring_id`, which is
identifier-safe as SPEC-0011 requires. The doorbell `content` and its instruction to claim MUST
NOT change. `ring_id` MUST be generated before the write, so that the record created after a
successful write carries the same id.

#### Scenario: Doorbell carries the ring id

- **WHEN** a todo doorbell is emitted
- **THEN** its `meta` contains `todo_id`, `queue` and `ring_id`, all with snake_case keys

### REQ-3: Acknowledgement

A ring MUST become `acknowledged` when, before its window elapses:

* **for a todo or test ring**, any session of the ring's endpoint claims the ring's todo, through
  `claim` or `claim_next`; or
* **for a digest ring**, any session of the ring's endpoint claims any todo; or
* **for any ring**, a session of the ring's endpoint calls `ack_doorbell {ring_id}`.

The claim path MUST write the acknowledgement in the same transaction that takes the lease, with
`ack_via = "claim"`. It MUST acknowledge every open ring for that todo on that endpoint. A digest
acknowledgement MUST cover only digest rings sent before the claim.

`ack_doorbell {ring_id}` MUST be a **self verb**: available to every session authenticated to an
active endpoint, without appearing in, or being required in, `scope.verbs`. It MUST set
`ack_via = "ack"` and return `{ring_id, state}`. It MUST be idempotent. A `ring_id` that belongs to
another endpoint, or that does not exist, MUST be answered with `not_found`, identically in both
cases.

An acknowledgement that arrives after `unheard_at` MUST be recorded on the ring (`acked_at`,
`ack_via`), and MUST NOT change the ring's terminal state or any counter.

#### Scenario: Claim acknowledges in the same transaction

- **GIVEN** a todo that was rung to session S
- **WHEN** session T of the same endpoint claims it
- **THEN** the lease and the ring's `acked_at`, with `ack_via = "claim"` and
  `acked_session_id = T`, commit together

#### Scenario: Ack without claiming

- **GIVEN** a busy worker that receives a doorbell and cannot take the work now
- **WHEN** it calls `ack_doorbell {ring_id}`
- **THEN** the ring is `acknowledged` with `ack_via = "ack"`, and the todo stays `pending`

#### Scenario: Another endpoint's ring

- **WHEN** a session of endpoint B calls `ack_doorbell` with a `ring_id` belonging to endpoint A
- **THEN** the call fails with `not_found`, and A's ring is unchanged

### REQ-4: Unheard Rings

A ring MUST become `unheard` when its ack window elapses without an acknowledgement. The window
defaults to 15 minutes. The operator MAY set it from 1 minute to 6 hours instance-wide
(`SWITCHBOARD_DOORBELL_ACK_WINDOW`), and a value outside that range MUST fail startup validation.
Test rings MUST use the test's own window (REQ-7) instead.

The transition MUST be claimed exactly once across all instances and restarts, by a conditional
update that sets `unheard_at` only where `acked_at IS NULL AND unheard_at IS NULL AND sent_at <
now() - window`. Only the rows that update returns MUST increment the unheard counter. The sweep
MUST run at least once a minute.

A ring MUST reach exactly one terminal state: `acknowledged` if the ack came within the window,
otherwise `unheard`.

#### Scenario: Nobody acts on a ring

- **GIVEN** a todo rung at 10:00, with a 15-minute window
- **WHEN** no session of the endpoint claims it or acks the ring by 10:15
- **THEN** the ring is `unheard` at the next sweep, and `switchboard_doorbell_rings_unheard_total`
  increments once

#### Scenario: Two instances sweep at once

- **WHEN** two instances run the unheard sweep over the same expired ring
- **THEN** exactly one sets `unheard_at`, and the counter increments once in total

#### Scenario: A late claim

- **GIVEN** a ring that became `unheard` at 10:15
- **WHEN** the endpoint claims the todo at 10:20
- **THEN** the ring records `acked_at = 10:20`, stays `unheard`, and no counter changes

### REQ-5: Per-Session Unheard Detection

A session whose 3 most recent rings all became `unheard`, with no acknowledgement of any kind from
that session in between, MUST be marked **unheard** in the session registry. The instance MUST log
one warning per marking, with the endpoint slug, the session id and the client's `clientInfo`.
This mark is separate from the existing write-failure warning. Any acknowledgement by the session,
whether a claim or `ack_doorbell`, MUST clear the mark.

The board's endpoint card MUST show an unheard session with a label that says rings reach it but
nothing acts on them, and MUST link the self-test. SPEC-0022 requires that an endpoint whose
effective presence is `out` is not shown with this warning.

#### Scenario: A session without the channel flag

- **GIVEN** a Claude Code session connected without the channel flag
- **WHEN** 3 todos are rung to it and none is claimed within the window
- **THEN** the session is marked unheard, the board shows the warning, and one warning line is
  logged

#### Scenario: A busy worker is not labelled deaf

- **GIVEN** a session with two unheard rings
- **WHEN** it claims any todo
- **THEN** its unheard run resets, and it is not marked

### REQ-6: Unheard-Rings Metrics

Switchboard MUST register these series in the SPEC-0023 registry:

```
switchboard_doorbell_rings_total{kind}                    counter  # kind: todo|digest
switchboard_doorbell_rings_acknowledged_total{kind,via}   counter  # via: claim|ack
switchboard_doorbell_rings_unheard_total{kind}            counter
switchboard_doorbell_unheard_sessions                     gauge
```

`rings_total` MUST increment when a ring is created. `acknowledged_total` MUST increment only for
an acknowledgement that arrives within the window. `unheard_total` MUST increment only for the rows
the REQ-4 update returns. `unheard_sessions` MUST be computed at scrape time from the instance's
session registry. Rings with `synthetic = true` MUST NOT affect any series. Endpoint, session, ring
and todo identifiers MUST NOT be labels (SPEC-0023 REQ-5).

#### Scenario: Self-test leaves the metrics alone

- **WHEN** an endpoint runs `test_doorbell` and the synthetic ring is acknowledged
- **THEN** no `switchboard_doorbell_*` series changes, and no SPEC-0023 todo series changes

### REQ-7: The `test_doorbell` Self Verb

`test_doorbell {wait_seconds?}` MUST be a self verb. It MUST act only on the caller's own endpoint,
and MUST NOT accept an endpoint, queue or todo argument. `wait_seconds` defaults to 60 and MUST be
between 10 and 300. A value outside that range MUST be refused with `invalid_argument`.

On success, it MUST:

1. create a synthetic todo (REQ-9) on the caller's endpoint;
2. return **without waiting for the ring**, with `{test_id, todo_id, report_after}` plus the facts
   observable at that moment: `server_version`, `stream_attached` (whether any session of the
   endpoint holds an open notification stream), and `channel_capability`;
3. write the test doorbell to the endpoint about 2 seconds after returning, through the same
   session selection as a todo doorbell. The ring is not tied to the calling session, because the
   endpoint is one logical agent.

`channel_capability` MUST report that the server advertised `claude/channel`, the `clientInfo`
name and version of each attached session, and the literal note that the client's listener cannot
be observed by the server, so that only a claim proves it.

The test doorbell's content MUST tell the agent that this is a Switchboard self-test, that it
should claim the todo, and that no other action is needed.

Each endpoint MUST have at most one test outstanding and at most 6 tests per rolling hour. A call
over either limit MUST fail with `resource_exhausted`, and MUST name when the next test is allowed.

#### Scenario: Test returns before ringing

- **WHEN** a session calls `test_doorbell {}`
- **THEN** the call returns a `test_id` before any test doorbell is written, and the doorbell is
  written about 2 seconds later

#### Scenario: Out-of-range wait

- **WHEN** a session calls `test_doorbell {"wait_seconds": 900}`
- **THEN** the call fails with `invalid_argument`, and no synthetic todo is created

#### Scenario: Endpoint argument refused

- **WHEN** a session calls `test_doorbell {"endpoint": "someone-else"}`
- **THEN** the call fails with `invalid_argument`, and no todo is created on any endpoint

#### Scenario: Rate limit

- **GIVEN** an endpoint with a test outstanding
- **WHEN** it calls `test_doorbell` again
- **THEN** the call fails with `resource_exhausted`, and the outstanding test is unaffected

### REQ-8: The Test Report

`get_doorbell_test {test_id}` MUST be a self verb, and MUST return the report for a test that
belongs to the caller's endpoint. A `test_id` belonging to another endpoint, or unknown, MUST be
answered with `not_found`. The report MUST contain:

* `server_version`;
* `stream_attached`;
* `channel_capability`;
* `delivered`: `{written: bool, at, session_id}`;
* `claimed_within_seconds`: seconds from write to claim, or null;
* `verdict`;
* `hints`: a list of strings.

`verdict` MUST be exactly one of:

| Verdict | Condition |
|---|---|
| `pending` | The wait has not elapsed and the todo is not yet claimed |
| `pass` | The synthetic todo was claimed within `wait_seconds` of the write |
| `no_session` | No session of the endpoint was attached when the ring was due |
| `no_stream` | Sessions were attached, but none held an open stream, so the write was dropped |
| `write_failed` | The write was attempted and failed |
| `unheard` | The write succeeded, and nothing claimed the todo within `wait_seconds` |

`hints` MUST be chosen by verdict and by the client's `clientInfo.name`, from a table the design
defines. For a Claude Code client with verdict `unheard` or `no_stream`, the hints MUST include
starting it with `--dangerously-load-development-channels server:switchboard` (or
`--channels plugin:<name>@<marketplace>` where the organization allowlists the plugin), passing
`--allowedTools mcp__switchboard`, and the fact that `claude -p` cannot be woken, so a notify hook
(SPEC-0024) should be used instead. For every verdict except `pass` and `pending`, the hints MUST
link the connect guide's troubleshooting section.

Reports MUST be kept for 7 days after the test expires.

#### Scenario: A healthy Claude Code session

- **GIVEN** a Claude Code session started with the development-channels flag and idle
- **WHEN** it calls `test_doorbell`, ends its turn, is woken, claims the todo and calls
  `get_doorbell_test`
- **THEN** the verdict is `pass` and `claimed_within_seconds` is set

#### Scenario: Missing channel flag

- **GIVEN** a Claude Code session connected as a plain MCP server
- **WHEN** it calls `test_doorbell`, and later calls `get_doorbell_test` after `wait_seconds`
- **THEN** `delivered.written` is true, the verdict is `unheard`, and the hints name the
  development-channels flag and `--allowedTools mcp__switchboard`

#### Scenario: Another endpoint's report

- **WHEN** endpoint B calls `get_doorbell_test` with endpoint A's `test_id`
- **THEN** the call fails with `not_found`

### REQ-9: Synthetic Todos

A synthetic todo MUST be stored with `synthetic = true`, `queue` set to a queue the caller's scope
grants (the first in sorted order), `source = "switchboard"` and `kind = "doorbell_test"`. It MUST
have no event, no routing trace and no work order.

A synthetic todo:

* MUST pass the doorbell sender gate for its single test ring only;
* MUST NOT be selected by the doorbell heartbeat sweep or by ring-on-attach;
* MUST NOT fire a notify hook (SPEC-0024);
* MUST NOT be returned by `claim_next`, by `list_todos` without an explicit
  `include_synthetic: true`, or by any board lane or todo list;
* MUST be claimable by id, by any session of its endpoint;
* MUST be completed automatically in the claim's transaction, with the result
  `{"self_test": true, "note": "no work needed"}`, and the claim's response MUST say so;
* MUST NOT increment, decrement or appear in any SPEC-0023 series or any REQ-6 series;
* MUST be deleted when its test expires (`wait_seconds` plus 5 minutes of grace), claimed or not,
  while its test report survives (REQ-8).

The store MUST apply the `synthetic = false` filter by default in every todo read and lifecycle
path, so that an individual caller cannot forget it. Only the self-test paths may opt in.

#### Scenario: A worker's pull skips a synthetic todo

- **GIVEN** an endpoint with an outstanding synthetic todo and one real pending todo
- **WHEN** a session calls `claim_next`
- **THEN** it receives the real todo

#### Scenario: Unclaimed synthetic todo is cleaned up

- **GIVEN** a test with `wait_seconds = 60` whose todo is never claimed
- **WHEN** 6 minutes have passed since the ring
- **THEN** the synthetic todo no longer exists, and `get_doorbell_test` still returns the report
  with verdict `unheard`

### REQ-10: Operator Self-Test Route and `switchboard doctor`

The operator API MUST expose, behind the same OAuth guard and ownership check as the other
`/api/v1/endpoints/{ref}` routes:

* `POST /api/v1/endpoints/{ref}/doorbell-tests` with `{"wait_seconds": 60}`, which runs REQ-7 for
  that endpoint and returns `201` with the REQ-7 start result;
* `GET /api/v1/endpoints/{ref}/doorbell-tests/{test_id}`, which returns the REQ-8 report.

Both MUST answer not-found for an endpoint the caller does not own, and `409` for a revoked
endpoint. The instance operator role MUST NOT widen what either route can reach.

`switchboard doctor [REF] [--wait 60s] [--json]` MUST, in this order:

1. report the CLI's version and whether stored credentials are live;
2. fetch `/healthz` and report the server's version, warning when the server is older than the
   CLI or differs from it in major or minor version;
3. list the caller's endpoints with the number of attached sessions and unheard sessions for each;
4. with `REF`, start a test, poll the report once a second until the verdict is no longer
   `pending`, then print the verdict, the timings and the hints.

`doctor` MUST exit 0 on `pass`, 1 on any other verdict or check failure, and 2 on usage errors.

#### Scenario: Doctor proves the loop

- **GIVEN** an idle agent whose client loaded the channel
- **WHEN** its human runs `switchboard doctor my-agent-k3x9`
- **THEN** the agent is woken, claims the synthetic todo, and doctor prints `pass` and exits 0

#### Scenario: Doctor against someone else's endpoint

- **WHEN** a human runs `switchboard doctor` with the reference of an endpoint they do not own
- **THEN** the route answers not-found, doctor prints that the endpoint was not found, and it
  exits 1

### REQ-11: Error Handling Standards

Every error MUST be wrapped with the endpoint slug and the stage that failed (ring record, ack,
sweep, test start, report). Sentinel errors MUST distinguish not-found, rate-limited and
invalid-argument for the verbs. Ring-recording failures MUST NOT fail or delay a doorbell. Nothing
may be silently swallowed, and every log line MUST use structured key-value fields.

#### Scenario: Ring store unavailable

- **WHEN** a ring cannot be recorded because the database is unavailable
- **THEN** the doorbell is still written, the failure is logged with the slug and todo id, and the
  collection-error counter increments

### REQ-12: Concurrency and Database Standards

Ring creation MUST happen outside the session-registry lock. The unheard sweep and the synthetic
cleanup MUST be single parameterized statements, safe under concurrent instances. The
claim-acknowledgement update MUST run in the claim's transaction, and MUST NOT lock ring rows that
the pump writes. Background loops MUST stop on the server's lifecycle context, and MUST pass
`go test -race`.

#### Scenario: Concurrent claim and unheard sweep

- **WHEN** a claim commits for a todo at the same moment the unheard sweep evaluates its ring
- **THEN** the ring ends in exactly one terminal state, and exactly one counter increments

## Security Requirements

### Authentication

| Surface | Auth | Description |
|---|---|---|
| MCP `ack_doorbell`, `test_doorbell`, `get_doorbell_test` | Required | Endpoint credential or OAuth token; self verbs, caller's endpoint only |
| `POST /api/v1/endpoints/{ref}/doorbell-tests` | Required | Operator OAuth, caller must own `{ref}` |
| `GET /api/v1/endpoints/{ref}/doorbell-tests/{test_id}` | Required | Operator OAuth, caller must own `{ref}` |
| `GET /metrics` series added here | Required | SPEC-0023 scrape credential |

### Rate Limiting

Tests are limited per endpoint (REQ-7). The self verbs sit behind the existing per-endpoint MCP
limiter. The operator routes inherit the `/api/v1` limits.

### Security Headers

The operator routes and the board MUST carry the existing `secureHeaders` set.

### Request Body Size Limits

The operator route body MUST be capped at 4 KiB. MCP bodies stay at 1 MiB (SPEC-0014).

### CSRF Protection

The board's "run self-test" control, if offered, MUST be a POST with the existing CSRF token.

### Redirect Validation

No surface in this spec redirects, except the board control, which MUST redirect only to the
same-origin endpoint card.

### Tenancy and Injection

* A test MUST create a todo only on the caller's own endpoint. No verb or route in this spec can
  name, read or ring another endpoint.
* The test doorbell's content MUST be fixed server text, with no caller-supplied or sender-supplied
  text.
* `clientInfo` values are client-supplied. They MUST be neutralized (length-capped at 128
  characters, with control characters stripped) before they are logged, stored or returned.
