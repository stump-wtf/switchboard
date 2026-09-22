---
status: draft
date: 2026-09-22
implements: [ADR-0039]
extends: [SPEC-0003, SPEC-0006]
requires: [SPEC-0004, SPEC-0013, SPEC-0023]
---

# SPEC-0034: Attempt History on Todos

## Overview

Every committed claim of a todo opens an **attempt**, and the transition that ends the lease closes
it with an **outcome**. Attempts are kept with the todo, bounded, scoped exactly as the todo is, and
returned on claim responses and from a new `get_todo` verb. The next claimer therefore learns what
earlier attempts tried, who made them, and whether each one failed or **died** (its lease lapsed
with no report). An optional **lease token** fences an attempt, so only its holder can heartbeat,
complete, fail or release it. See [ADR-0039](../../../adrs/ADR-0039-attempt-history-on-todos.md).

This spec extends [SPEC-0003](../todo-queue/spec.md) (the lifecycle, lease, reaper and retries, which
are unchanged except that each transition now also writes an attempt) and
[SPEC-0006](../agent-tools/spec.md) (the drain verbs, which gain optional arguments, response fields,
and the `get_todo` and `release` verbs). It requires [SPEC-0004](../persistence/spec.md) (migrations
and retention), [SPEC-0013](../operator-board/spec.md) (the todo detail drawer) and
[SPEC-0023](../metrics/spec.md) (metrics).

Parallel records, cited by number until they merge: the first consumer is Harness SPEC-0019 (relay
attempts); owner scopes widen under SPEC-0033 (teams and tenancy); dead-letter notifications are
SPEC-0029's; re-queue wake-ups on notify hooks are SPEC-0024's; queue admission (SPEC-0030) charges
one unit per committed claim, which is the same event REQ-2 records.

Terms:

* **Attempt**: the span from one committed claim to the transition that ends that lease.
* **Open attempt**: an attempt with no `ended_at`. A todo has at most one.
* **Died**: an attempt whose outcome is `lease_expired` or `reaped`.
* **Owner scope**: the scope a todo's reads are filtered by, today its `endpoint_id` (ADR-0022), and
  after SPEC-0033 whatever owner scope that spec assigns the todo's queue.

## Requirements

### REQ-1: Attempt Record

Switchboard SHALL persist one attempt record per committed claim, with these fields:

| Field | Type | Meaning |
| --- | --- | --- |
| `todo_id` | text | The todo; deleting the todo deletes its attempts |
| `seq` | int | 1-based, increasing per todo, never reused or reset |
| `attempt` | int | The todo's `attempt` counter after this claim (a manual retry resets the counter, never `seq`) |
| `claimer_kind` | enum | `endpoint` (an agent credential) or `owner` (a signed-in human claiming from the Board) |
| `claimer_endpoint_id` | uuid, nullable | The endpoint whose credential claimed; null for `owner` |
| `claimer_session` | text, nullable | The MCP session id the claim arrived on, when it arrived over MCP |
| `owner` | text | The `owner` string written to the todo by the claim |
| `claimant` | text, nullable | Caller-supplied label, at most 128 bytes (REQ-5) |
| `claimed_at` | timestamptz | When the claim committed |
| `last_heartbeat_at` | timestamptz, nullable | The last accepted heartbeat |
| `lease_expires_at` | timestamptz | The lease expiry most recently set by the claim or a heartbeat |
| `ended_at` | timestamptz, nullable | When the attempt closed; null while open |
| `outcome` | enum, nullable | REQ-3; null while open |
| `disposition` | enum, nullable | REQ-3; null while open |
| `summary` | text, nullable | At most 2048 bytes (REQ-5) |
| `summary_truncated` | bool | True when `summary` was cut |
| `artifact` | text, nullable | At most 512 bytes (REQ-5) |
| `lease_token_hash` | bytea, nullable | SHA-256 of the lease token, when the claim requested a fence (REQ-6) |

The claimer fields SHALL be provenance only: no authorization decision SHALL read them. An attempt
SHALL carry no owner-scope column of its own. It SHALL be reachable only through its todo, and SHALL
inherit that todo's owner scope.

#### Scenario: Fields after a claim

- **WHEN** endpoint E claims todo T over MCP session S with `claimant = "harness/box/fixer/run-7"`
- **THEN** T has one open attempt with `seq = 1`, `claimer_kind = endpoint`,
  `claimer_endpoint_id = E`, `claimer_session = S`, the claimant label, and a null `outcome`

### REQ-2: Opening an Attempt on Every Committed Claim

Every transition that sets a todo to `claimed` by claiming it SHALL insert exactly one attempt
record in the same database transaction as the `todos` update: `claim`, `claim_next`, a claim that
takes over a lapsed lease, and the Board's claim. A claim that does not commit SHALL insert nothing.
A todo SHALL NOT have more than one open attempt, and a database constraint checked at commit SHALL
enforce this. The A2A interrupt and resume transitions (SPEC-0018) SHALL NOT open or close an
attempt; the attempt stays open across them.

#### Scenario: A lost claim race writes nothing

- **WHEN** two endpoints `claim` the same pending todo concurrently
- **THEN** exactly one claim commits, and the todo has exactly one attempt

#### Scenario: Interrupt keeps the attempt open

- **WHEN** a claimed todo moves to `input-required` and later back to `claimed` through resume
- **THEN** its single open attempt stays open, and no new attempt is opened

### REQ-3: Closing an Attempt

The transition that ends a lease SHALL close the todo's open attempt in the same transaction,
setting `ended_at`, `outcome` and `disposition`:

| Transition | `outcome` | `disposition` |
| --- | --- | --- |
| `complete` | `completed` | `done` |
| `fail` with `attempt < max_attempts` | `failed` | `retry_scheduled` |
| `fail` with `attempt >= max_attempts` | `failed` | `dead_lettered` |
| `release` (REQ-9, and the Board's Release) | `released` | `requeued` |
| A claim that takes over a lapsed lease | `lease_expired` | `requeued` |
| The reaper, `attempt < max_attempts` | `reaped` | `requeued` |
| The reaper, `attempt >= max_attempts` | `reaped` | `dead_lettered` |
| A2A cancel of a claimed or interrupted todo | `canceled` | `canceled` |
| Revocation of the owning endpoint | `revoked` | `dead_lettered` |

For a lease takeover, the old attempt SHALL be closed before the new one is opened, in one
transaction. A transition that ends no lease (for example, a manual retry of a dead letter, or the
retry scheduler re-queuing a `failed` todo) SHALL NOT close or open an attempt.

#### Scenario: Fail below the cap

- **WHEN** the holder fails todo T at attempt 2 of 5
- **THEN** T's attempt 2 closes with `outcome = failed` and `disposition = retry_scheduled`

#### Scenario: Takeover

- **GIVEN** attempt 1 of todo T is open and its lease expired a minute ago
- **WHEN** `claim_next` takes T over before the reaper runs
- **THEN** attempt 1 closes with `outcome = lease_expired`, and attempt 2 opens, in one transaction

### REQ-4: Died Versus Failed

Every read that returns an attempt SHALL include a derived boolean `died`, true exactly when
`outcome` is `lease_expired` or `reaped`. A died attempt's `summary` and `artifact` SHALL be null,
because the holder never reported. Each heartbeat that extends a lease (SPEC-0003 REQ "Visibility
Window, Lease, and Heartbeat") SHALL set the open attempt's `last_heartbeat_at` and
`lease_expires_at` in the same statement as the todo update.

#### Scenario: The reaper records a death

- **GIVEN** a worker claims todo T, heartbeats once at 14:01, and then stops
- **WHEN** the lease expires and the reaper runs
- **THEN** the attempt closes with `outcome = reaped`, `died = true`, `last_heartbeat_at = 14:01`,
  and a null summary, and the next claimer sees it that way

### REQ-5: Summary, Artifact and Claimant Inputs

`complete`, `fail` and `release` SHALL accept two optional string arguments, `summary` and
`artifact`. `claim` and `claim_next` SHALL accept an optional string argument, `claimant`.

* `summary` SHALL be stored in the closing attempt, cut to 2048 bytes on a UTF-8 boundary, with
  `summary_truncated` set when cut. When `summary` is absent and `result` is present, the stored
  summary SHALL be the compact JSON serialization of `result`, cut the same way.
* `artifact` SHALL be at most 512 bytes and SHALL match `^mcp://cairn/[A-Za-z0-9_-]{1,64}$` or be an
  absolute `https` URL. Anything else SHALL fail the call with error code `invalid`, naming the
  argument, and no transition SHALL apply. Switchboard SHALL NOT fetch, resolve or dereference an
  artifact.
* `claimant` SHALL be cut to 128 bytes, and control characters SHALL be removed.
* `result` SHALL keep its current meaning, stored on the todo.

All inputs SHALL be stored and returned as data, and SHALL NOT be interpreted.

#### Scenario: Summary from result

- **WHEN** a worker calls `fail` with `result = {"error": "tests red"}` and no `summary`
- **THEN** the closing attempt's summary is `{"error":"tests red"}`

#### Scenario: A file URL as artifact

- **WHEN** a worker calls `complete` with `artifact = "file:///etc/passwd"`
- **THEN** the call fails with `invalid` naming `artifact`, and the todo stays `claimed`

### REQ-6: Lease Token Fence

`claim` and `claim_next` SHALL accept an optional boolean `require_fence`. When it is true and the
claim commits, Switchboard SHALL generate a lease token of at least 128 random bits, SHALL return it
once as `lease_token` in that response, SHALL store only its SHA-256 in the attempt, and SHALL NOT
log it or return it from any other call.

`heartbeat`, `complete`, `fail` and `release` SHALL accept an optional string `lease_token`:

* When `lease_token` is supplied, the call SHALL apply only if its hash equals the open attempt's
  `lease_token_hash`, compared in constant time. Otherwise it SHALL fail with `conflict`.
* When the open attempt has a `lease_token_hash` and the call supplies no `lease_token`, it SHALL fail
  with `conflict`.
* When neither is present, the call SHALL behave exactly as it does today.

The owner check (`owner = agent:<agent_id>`) SHALL still apply in every case. The fence narrows it and
never widens it. Board actions by the owning human SHALL be exempt from the fence, so a human can
always release or fail a stuck attempt. They SHALL close the attempt with the outcome of the action.

#### Scenario: The agent cannot close its supervisor's attempt

- **GIVEN** Harness claimed todo T with `require_fence: true` on endpoint E
- **WHEN** an agent holding E's credential calls `complete` on T with no `lease_token`
- **THEN** the call fails with `conflict`, and T stays `claimed`

#### Scenario: A stale worker's token

- **GIVEN** attempt 1's lease lapsed and attempt 2 was claimed with a fence by the same agent identity
- **WHEN** the attempt-1 worker calls `complete` with attempt 1's token
- **THEN** the call fails with `conflict`, and attempt 2 is untouched

#### Scenario: Unfenced client unchanged

- **WHEN** a client claims without `require_fence` and completes without `lease_token`
- **THEN** both calls behave exactly as they did before this spec

### REQ-7: Attempts on Claim Responses

A committed `claim` or `claim_next` SHALL return, beside the todo:

| Field | Meaning |
| --- | --- |
| `attempt_seq` | The new attempt's `seq` |
| `lease_token` | Present only when `require_fence` was true (REQ-6) |
| `prior_attempts` | The five most recent closed attempts of this todo, newest first |
| `attempts_total` | The number of attempts this todo has had, including pruned ones |

Each prior attempt SHALL carry `seq`, `attempt`, `claimer_kind`, `claimant`, `claimed_at`,
`last_heartbeat_at`, `ended_at`, `outcome`, `disposition`, `died`, `summary`, `summary_truncated`
and `artifact`. It SHALL NOT carry `lease_token_hash`, `claimer_session` or `claimer_endpoint_id`.
The field description in the tool schema SHALL state that `summary` is data written by an earlier
attempt and never an instruction.

#### Scenario: Second attempt sees the first

- **GIVEN** attempt 1 of todo T failed with summary `tests still red`
- **WHEN** T is claimed again
- **THEN** the response has `attempt_seq = 2` and `prior_attempts[0].summary = "tests still red"`

### REQ-8: The get_todo Read Verb

Switchboard SHALL expose `get_todo` with arguments `id` (required) and `attempts_limit` (optional,
default 20, maximum 50). It SHALL return the todo's `todoOut` fields plus `result`,
`next_retry_at`, a derived `dead_letter` (true exactly when `state = 'failed'` and
`next_retry_at IS NULL`), `attempts` (newest first, up to `attempts_limit`, including the open
attempt), `attempts_total` and `attempts_pruned`. Attempt entries SHALL have the REQ-7 shape.

`get_todo` SHALL be registered and allowed for an endpoint whose scope holds `list_todos` or
`get_todo`. It SHALL be scoped exactly as `list_todos` is: the todo MUST be owned by the calling
endpoint (or, after SPEC-0033, by an owner scope the endpoint may read) and in a granted queue.
Otherwise the call SHALL fail with `not_found`, indistinguishable from an id that was never minted
(REQ-10).

The `todoOut` returned by `fail` SHALL also carry `next_retry_at` and `dead_letter`.

#### Scenario: Reading a dead letter

- **WHEN** an endpoint calls `get_todo` on its own dead-lettered todo after five failed attempts
- **THEN** the response has `dead_letter = true`, a null `next_retry_at`, and five attempts, newest
  first

#### Scenario: An existing endpoint gets the verb

- **WHEN** an endpoint vended before this spec with `list_todos` in its scope lists its tools
- **THEN** `get_todo` is advertised, and a call to it succeeds on the endpoint's own todo

### REQ-9: The release Verb

Switchboard SHALL expose `release` with arguments `id`, `summary`, `artifact` and `lease_token`. It
SHALL apply `ReleaseTodo`'s existing semantics: only the current lease owner, within the endpoint's
scope, may release; the todo returns to `pending` with `owner` and `lease_expires_at` cleared; and
`attempt` is neither consumed nor reset. It SHALL close the open attempt with `outcome = released`
(REQ-3). `release` SHALL be a grantable verb in the drain family, listed by the vend wizard and the
consent screen, and not implied by any other verb.

#### Scenario: Release on shutdown

- **WHEN** the holder of todo T calls `release` with `summary = "daemon stopping"`
- **THEN** T is `pending`, its attempt counter is unchanged, and the attempt closes `released` with
  that summary

#### Scenario: Release without the grant

- **WHEN** an endpoint whose scope lacks `release` calls it
- **THEN** the call fails with `forbidden`, and nothing changes

### REQ-10: Tenant Isolation

Every read of an attempt SHALL be filtered by the owning todo's owner scope, in the same statement
that reads the todo. Agent-facing reads SHALL constrain `todos.endpoint_id` to the caller's endpoint
(ADR-0022). Board reads SHALL use the owning-human predicate the Board's todo reads already use.
SPEC-0033 MAY widen both predicates to team membership, and attempts SHALL follow without a change
of their own. An attempt of another owner scope, and an id that was never minted, SHALL both produce
`not_found`, with the same error body. No response SHALL reveal whether a foreign todo has attempts.

The instance operator SHALL see attempt data only as aggregates (SPEC-0023 metrics), never another
user's summaries, artifacts or claimant labels.

The reaper and the endpoint-revocation cascade SHALL close attempts system-wide without a caller
scope. That is the same background-sweep exemption SPEC-0003 REQ "Endpoint Ownership (Tenant
Isolation)" grants `ReapExpired`, and these sweeps SHALL read nothing out.

#### Scenario: Foreign todo

- **WHEN** endpoint B calls `get_todo` with the id of endpoint A's todo
- **THEN** B receives `not_found` with a body byte-identical to the response for a random id

#### Scenario: Second human on the Board

- **WHEN** human H2 opens the todo drawer URL of a todo owned by human H1's endpoint
- **THEN** H2 gets the not-found response, and no attempt data is rendered

### REQ-11: Retention and Bounds

* Deleting a todo SHALL delete its attempts in the same statement (`ON DELETE CASCADE`), so the
  retention of terminal todos (SPEC-0004 REQ "Hybrid Retention and Bounded Growth") bounds attempt
  history.
* Retention SHALL NOT delete attempts of a live todo (`pending`, `claimed`, an interrupt state, or a
  `failed` todo with an open retry window).
* When opening an attempt would give a todo more than `attempt_history_max_per_todo` attempts (a
  setting, default 50, minimum 5), the oldest closed attempts SHALL be deleted in the same
  transaction, and the todo's `attempts_pruned` counter SHALL be incremented by the number deleted.
  The open attempt SHALL never be pruned.
* Text limits: `summary` 2048 bytes, `artifact` 512 bytes, `claimant` 128 bytes (REQ-5).

#### Scenario: A hot todo is capped

- **WHEN** a todo is claimed 60 times through manual retries
- **THEN** it has 50 attempts, `attempts_pruned = 10`, `attempts_total = 60`, and the newest 50 seqs
  remain

#### Scenario: Retention removes history with the todo

- **WHEN** retention deletes a done todo older than the age bound
- **THEN** none of its attempts remain

### REQ-12: Manual Retry Keeps History

A manual retry of a dead letter (`RetryTodo`, and its Board variant) SHALL keep every attempt
record, and SHALL reset only the todo's counters, exactly as today. The next claim SHALL open an
attempt with `attempt = 1` and the next `seq`. `get_todo` and `prior_attempts` SHALL show attempts
from before and after the retry.

#### Scenario: Retry after dead letter

- **GIVEN** todo T dead-lettered after attempts with seq 1–5
- **WHEN** its owner retries it and a worker claims it
- **THEN** the new attempt has `seq = 6` and `attempt = 1`, and `prior_attempts` begins with seq 5

### REQ-13: Operator Surfaces

The Board's todo detail drawer (SPEC-0013 REQ "Todo Detail Drawer") SHALL list the todo's attempts,
newest first: attempt number, claimant, claimed and ended times, outcome, a visible marker for died
attempts, the summary rendered as escaped text, and the artifact as a link only when it is an
`https` URL. The A2UI todo detail resource SHALL include the same list when A2UI is enabled. Neither
SHALL show `claimer_session` or `lease_token_hash`.

#### Scenario: Drawer shows a death

- **WHEN** the owner opens the drawer of a todo whose attempt 2 was reaped
- **THEN** attempt 2 shows outcome `reaped`, the died marker, and its last heartbeat time

#### Scenario: Summary markup is inert

- **WHEN** an attempt summary contains `<script>alert(1)</script>`
- **THEN** the drawer renders it as literal text, and no script runs

### REQ-14: Metrics

Switchboard SHALL export `switchboard_todo_attempts_closed_total{queue,outcome}`, incremented after
commit for each closed attempt, with `queue` bounded as SPEC-0023 REQ-5 bounds it. The existing
`switchboard_lease_expired_total` SHALL keep its meaning. Every attempt closed `lease_expired` or
`reaped` SHALL correspond to exactly one increment of it.

#### Scenario: A reap counts once in each family

- **WHEN** the reaper closes one attempt as `reaped`
- **THEN** `switchboard_lease_expired_total` and
  `switchboard_todo_attempts_closed_total{outcome="reaped"}` each increase by exactly 1

### REQ-15: Dead-Letter Context for Notifications

When a transition dead-letters a todo, the committed-transition hook payload SHALL carry the closing
attempt's `seq`, `outcome`, `died`, `summary` and `artifact`, and the todo's `attempts_total`. That
is the input SPEC-0029's notification sinks render. This spec SHALL NOT itself send notifications.

#### Scenario: Dead letter carries its last attempt

- **WHEN** attempt 5 of 5 fails with summary `still red` and artifact `mcp://cairn/Zz9`
- **THEN** the transition hook's payload includes `attempts_total = 5`, `summary = "still red"` and
  that artifact

### REQ-16: Re-Queue Wake-Up Interface

A todo re-queued by the retry scheduler after a failed attempt SHALL remain push-eligible under the
same sender gate as a fresh todo (SPEC-0011), so a relay consumer is woken for its next attempt. The
channel path rings through the `todo_ready` wake-up today, but the in-process doorbell gate
(`doorbellGateTTL`, one minute) suppresses a second ring of the same todo id inside its window, so
an attempt that fails within about 30 seconds of the first ring is re-queued unrung and waits for
the doorbell heartbeat. A re-queue SHALL therefore clear that todo's gate entry, so the re-queue
rings. For notify hooks, this is an interface requirement on SPEC-0024, which SHALL fire
`todo.ready` for re-queued retries.

#### Scenario: Retry rings again

- **WHEN** a failed todo's backoff elapses and the retry scheduler re-queues it
- **THEN** an in-scope live session receives a doorbell for it

#### Scenario: A quick failure is not swallowed by the gate

- **GIVEN** a todo rung at creation, claimed, and failed 5 seconds later
- **WHEN** the retry scheduler re-queues it 30 seconds after that, inside the gate's window
- **THEN** an in-scope live session receives a doorbell for the re-queue, without waiting for the
  doorbell heartbeat

### REQ-17: Migration and Compatibility

The schema change SHALL be one additive, transactional migration (SPEC-0004 REQ "Embedded, Ordered,
Transactional Migrations"). It SHALL create the attempts table and indexes, add
`attempts_total` and `attempts_pruned` to `todos` (default 0), and open one attempt for every todo
that is `claimed` or in an interrupt state when it runs, with `seq = 1`,
`claimer_kind = endpoint`, `claimed_at` from the todo, and `claimant = 'migrated'`. It SHALL NOT
backfill history for terminal todos. Every new argument SHALL be optional, and every new response
field SHALL be additive, so a client written before this spec keeps working unchanged.

#### Scenario: A lease in flight across the upgrade

- **GIVEN** todo T is claimed when the migration runs
- **WHEN** its holder completes it after the upgrade
- **THEN** T's migrated attempt closes `completed`

### REQ-18: Database Operation Standards

Opening and closing an attempt SHALL happen in the same transaction as the `todos` update it
records, through data-modifying CTEs or an explicit transaction. All queries SHALL be parameterized.
Claim throughput under contention SHALL be measured against the pre-change baseline with the
existing concurrency tests, and SHALL NOT regress by more than 10%.

#### Scenario: Rollback leaves no orphan

- **WHEN** the `todos` update of a claim fails after the attempt insert was prepared
- **THEN** neither row is committed

### REQ-19: Error Handling Standards

A missed transition SHALL be classified as today (`classifyMiss` / `classifyMissForHuman`): absent
or foreign is `not_found`, and present but in the wrong state is `conflict`. A fence mismatch
(REQ-6) SHALL be `conflict`, and SHALL never be `forbidden`, which would confirm that the todo is
fenced. Invalid inputs (REQ-5) SHALL be `invalid`, naming the argument. Errors SHALL be wrapped with
the operation and todo id, and structured logs SHALL NOT include a lease token, a summary or an
artifact.

#### Scenario: Fence mismatch is conflict

- **WHEN** a heartbeat presents a wrong `lease_token` for the caller's own claimed todo
- **THEN** the call fails with `conflict`

### REQ-20: Concurrency Safety

The attempt writes SHALL preserve SPEC-0003 REQ "Concurrency Safety of Queue Workers": a reaper and
a taker racing on one lapsed lease SHALL close the open attempt exactly once, and the REQ-2
constraint SHALL make a second open attempt impossible to commit. The new store paths SHALL be
covered by tests that run under the race detector in CI.

#### Scenario: Reaper and taker race

- **WHEN** the reaper and a `claim_next` race on the same lapsed lease
- **THEN** exactly one of them closes the open attempt, and the todo ends with one open attempt at
  most

## Security Requirements

This spec adds verbs and arguments to the authenticated MCP surface. It adds no route.

### Authentication

`get_todo` and `release` SHALL be served only on the vended-endpoint MCP surface, behind the
existing bearer or OAuth authentication (SPEC-0006 REQ "Bearer-Scoped Endpoint Authentication",
SPEC-0016). There is no unauthenticated path to attempt data. Board surfaces SHALL require the
existing human session.

| Surface | Auth | Description |
| --- | --- | --- |
| MCP `get_todo` | Required | Endpoint credential; scope `list_todos` or `get_todo` |
| MCP `release` | Required | Endpoint credential; scope `release` |
| MCP `claim`, `claim_next`, `heartbeat`, `complete`, `fail` (new arguments) | Required | Unchanged scopes |
| Board todo drawer | Required | Human session; owning-human predicate |

### Rate Limiting

The new verbs SHALL be subject to the existing per-endpoint MCP rate limiter (`internal/mcp`
`ratelimit.go`). No new limiter is introduced.

### Security Headers

No new HTTP responses are introduced. Board fragments SHALL keep the existing CSP and security
headers (SPEC-0012).

### Request Body Size Limits

MCP request bodies SHALL remain under the existing body cap. `summary` above 2048 bytes is
truncated, not stored in full (REQ-5).

### CSRF Protection

The drawer is read-only. Existing Board actions (Release, Retry) keep their current CSRF protection.

### Redirect Validation

No redirects are introduced. Artifacts are rendered as links only when they are `https` URLs, with
`rel="noopener noreferrer"`, and are never followed server-side.

## Accessibility Requirements

This spec changes one UI surface, the Board's todo detail drawer. The following apply to the attempt
list it adds, per WCAG 2.1 AA:

- **WCAG 2.1 AA Compliance.** The attempt list SHALL meet WCAG 2.1 Level AA.
- **ARIA Landmarks.** The list SHALL sit inside the drawer's existing dialog landmark, as a list or
  table with a visible heading.
- **Icon-Only Controls.** The died marker SHALL carry an `aria-label` ("died: no report") and SHALL
  NOT rely on colour alone.
- **Dynamic Content Regions.** When a live update appends an attempt to an open drawer, the list SHALL
  be inside an `aria-live="polite"` region.
- **Keyboard Navigation.** Artifact links SHALL be reachable in tab order.
- **Focus Management.** Opening and closing the drawer keeps the existing focus management; the list
  adds no focus traps.
