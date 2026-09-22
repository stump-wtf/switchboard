---
status: draft
date: 2026-09-22
implements: [ADR-0029]
requires: [SPEC-0003, SPEC-0006, SPEC-0007, SPEC-0011]
related: [SPEC-0019, SPEC-0022, SPEC-0023]
---

# SPEC-0024: Outbound Todo Notify Hooks

## Overview

A **notify hook** is an outbound HTTPS URL that an endpoint registers for itself. When a
push-eligible todo owned by that endpoint becomes ready, Switchboard POSTs a signed notification
to the URL. The notification carries identifiers and a one-line summary, never the todo's payload.
It is the doorbell of [ADR-0013](../../../adrs/ADR-0013-channels-push-delivery.md) on a second
transport. It exists for consumers that have no live MCP session to ring: one-shot runs, scheduled
sweeps, CI jobs, and supervisors that start a worker on demand. See
[ADR-0029](../../../adrs/ADR-0029-outbound-todo-webhooks.md), which this spec implements.

The queue stays the ledger. A hook that is never configured, fails, times out, or is disabled loses
nothing: the todo is `pending` and `claim_next` returns it. The consumer gets the work by
**claiming** it, never from the notification body.

This spec turns ADR-0029's decisions into requirements. It also settles the ADR's two open
questions:

* **Presence.** Hooks respect endpoint presence by default. A per-hook `ignore_presence` flag
  opts an on-demand dispatcher out. When an endpoint returns from `out`, a hook that was held gets
  one backlog notification. See REQ-9.
* **Digest.** A burst is **not** coalesced in v1. Each ready todo produces its own notification,
  deduplicable by `webhook-id`. The delivery counters in REQ-11 are how a burst problem would be
  measured before coalescing is designed. This question stays open, with a measurement plan, in
  [design.md](./design.md).

Terms:

* **Hook**: one registered outbound URL, owned by exactly one endpoint.
* **Notification**: one logical message about one event (for example, one todo becoming ready).
* **Attempt**: one HTTP request carrying a notification. A notification has at most three.
* **Delivery**: the outcome of all attempts for one notification: `delivered` or `failed`.

## Requirements

### REQ-1: Hook Ownership and Scope

Every hook MUST belong to exactly one endpoint (`endpoint_id`), and MUST inherit that endpoint's
owner scope. There MUST NOT be an instance-wide hook, an environment-configured hook URL, or any
hook that is not owned by an endpoint.

A hook MUST fire only for todos whose `endpoint_id` equals the hook's `endpoint_id` **and** whose
queue is in the hook's effective queue set. The effective queue set MUST be the hook's `queues`
list intersected with the queues the endpoint's scope grants **at fire time**. An empty `queues`
list MUST mean "every queue the endpoint's scope grants". A queue that was removed from the scope
after the hook was created MUST stop matching without any change to the hook row.

A todo that no endpoint owns (a system todo) MUST NOT fire any hook.

Each endpoint MUST be limited to a ceiling on its hook count. The default is 5, and the operator
MAY lower or raise it instance-wide. Creating a hook past the ceiling MUST fail with
`resource_exhausted` and persist nothing.

#### Scenario: A hook fires only for its own endpoint's todos

- **GIVEN** endpoints A and B belong to different humans, both drain a queue named `inbox`, and A
  has a hook
- **WHEN** a push-eligible todo owned by B lands on `inbox`
- **THEN** A's hook is not called, and nothing about B's todo appears in any request to A's URL

#### Scenario: A hook filtered to one queue

- **GIVEN** endpoint A drains `inbox` and `reviews`, and has a hook with `queues = ["reviews"]`
- **WHEN** a push-eligible todo lands on A's `inbox`
- **THEN** the hook is not called

#### Scenario: Scope shrink stops a hook without editing it

- **GIVEN** a hook with `queues = []` on an endpoint whose scope granted `inbox` and `reviews`
- **WHEN** `reviews` is removed from the endpoint's scope, and a push-eligible todo later lands on
  `reviews` through a route that still targets the endpoint
- **THEN** the hook is not called for that todo

#### Scenario: Ceiling reached

- **GIVEN** an endpoint that already has 5 hooks and the default ceiling
- **WHEN** it calls `create_notify_hook`
- **THEN** the call fails with `resource_exhausted`, and no hook row, secret or URL is stored

### REQ-2: Management Verbs

Switchboard MUST expose four MCP verbs in the webhook family:

* `create_notify_hook {url, queues?, ignore_presence?}` validates the URL (REQ-3), mints a secret
  (REQ-4) and stores the hook. It returns `{hook_id, url, queues, ignore_presence, enabled,
  signing_secret}`. **This is the only time the secret is revealed.**
* `list_notify_hooks {}` returns the endpoint's hooks with their health (REQ-8) and the ceiling
  (`max`, `used`). It MUST NOT return any secret.
* `rotate_notify_hook {hook_id}` mints a new secret, returns it once, and re-enables a hook that
  REQ-8 disabled. It starts the dual-signing grace period REQ-4 defines.
* `delete_notify_hook {hook_id}` removes the hook and its secrets. An in-flight attempt MAY
  complete. No attempt may start after the delete commits.

These four verbs MUST be grantable per endpoint like any other verb. They MUST be listed by the
vend wizard, the quick-vend grant list and the MCP OAuth consent screen, grouped and labeled as
granting **outbound HTTP calls**. They MUST NOT be part of any default or "all verbs" grant. An
endpoint vended before this spec does not hold them until its human grants them. An endpoint that
lacks a verb MUST receive the same `permission_denied` as for any other unscoped verb.

Every verb MUST act only on the caller's own endpoint. A `hook_id` that belongs to another
endpoint, including another endpoint of the same human, MUST be answered exactly like an unknown
id: `not_found`.

#### Scenario: Create reveals the secret once

- **WHEN** an endpoint holding `create_notify_hook` creates a hook for `https://dispatch.example.com/sb`
- **THEN** the result carries `signing_secret`, and a following `list_notify_hooks` returns the
  hook with no secret field

#### Scenario: Endpoint without the verb

- **GIVEN** an endpoint whose scope does not include `create_notify_hook`
- **WHEN** it calls `create_notify_hook`
- **THEN** the call fails with `permission_denied`, and no outbound request is ever made

#### Scenario: Another endpoint's hook id

- **GIVEN** hook `nh_1` belongs to endpoint A
- **WHEN** endpoint B, owned by the same human, calls `delete_notify_hook {"hook_id": "nh_1"}`
- **THEN** the call fails with `not_found`, and `nh_1` is unchanged

#### Scenario: Not in the default grant

- **WHEN** a human quick-vends an endpoint with the default grant
- **THEN** its scope contains none of the four notify-hook verbs

### REQ-3: Target Validation (SSRF Guard)

Switchboard MUST validate a hook URL with the shared `internal/push.Validator`:

* once at `create_notify_hook`, failing fast with `invalid_argument` and persisting nothing;
* again immediately before **every** attempt.

The URL MUST be `https`. `http` MUST be accepted only when the operator has set the existing
`SWITCHBOARD_PUSH_ALLOW_HTTP` opt-in. The URL MUST NOT carry userinfo (`user:pass@`). It MUST be at
most 2048 bytes. It MUST NOT resolve to a loopback, private, link-local, unique-local,
unspecified, multicast or carrier-grade NAT address, or to one of Switchboard's own listen
addresses.

The operator MAY permit specific private ranges instance-wide with an explicit CIDR allowlist
(`SWITCHBOARD_NOTIFY_HOOK_ALLOW_CIDRS`). This is an operator bound for single-tenant or homelab
installs, and the self-hosting guide MUST say that it exposes those ranges to every tenant. The
default MUST be empty.

The connection MUST be made to an IP address from the **same** resolution that passed validation.
A second, unvalidated lookup between validation and dial MUST NOT be possible. TLS MUST verify the
certificate against the URL's hostname.

Switchboard MUST NOT follow redirects. A `3xx` response MUST be recorded as a failed attempt with
the status code, and its `Location` MUST NOT be dialled.

#### Scenario: Private target refused at create

- **WHEN** an endpoint calls `create_notify_hook` with a URL whose host resolves to `10.0.0.5`
- **THEN** the call fails with `invalid_argument` naming the rejected address class, and nothing is
  stored

#### Scenario: DNS rebinding between create and delivery

- **GIVEN** a hook whose host resolved to a public address at create time
- **WHEN** the host resolves to `127.0.0.1` at delivery time (simulated by the injected resolver)
- **THEN** no connection is opened, the attempt is recorded as `rejected_ssrf`, and it counts
  toward REQ-8's consecutive failures

#### Scenario: Redirect is a failure

- **WHEN** a hook's receiver answers `302` with `Location: http://169.254.169.254/`
- **THEN** the attempt is recorded as failed with status 302, and no request is made to the
  `Location`

#### Scenario: Plain HTTP without the opt-in

- **GIVEN** `SWITCHBOARD_PUSH_ALLOW_HTTP` is unset
- **WHEN** an endpoint calls `create_notify_hook` with `http://dispatch.example.com/sb`
- **THEN** the call fails with `invalid_argument`

### REQ-4: Signing (Standard Webhooks)

Switchboard MUST mint each hook's secret itself from a CSPRNG: 32 random bytes, presented as
`whsec_` followed by their base64. It MUST store the secret only through the `internal/cred`
envelope, like every other held secret. A caller MUST NOT be able to supply its own secret.

Every attempt MUST carry the [Standard Webhooks](https://www.standardwebhooks.com/) headers:

* `webhook-id`: the notification id (`msg_<26-char ULID>`), **identical across all attempts of one
  notification**;
* `webhook-timestamp`: the attempt time, in integer Unix seconds;
* `webhook-signature`: `v1,<base64 HMAC-SHA256>`, computed over
  `<webhook-id>.<webhook-timestamp>.<raw body>` with the decoded secret.

Each attempt MUST also carry `content-type: application/json` and
`user-agent: switchboard/<version>`, where `<version>` is the stamped server version.

After `rotate_notify_hook`, Switchboard MUST sign every attempt with both the new and the previous
secret for a grace period (default 24 hours), as two space-separated `v1,` entries in one
`webhook-signature` header. When the grace period ends, the previous secret MUST be destroyed.

#### Scenario: A receiver verifies a notification

- **WHEN** a receiver computes HMAC-SHA256 over `webhook-id + "." + webhook-timestamp + "." + body`
  with the secret `create_notify_hook` returned
- **THEN** the result equals the base64 value after `v1,` in `webhook-signature`

#### Scenario: Retries keep the id

- **WHEN** a notification's first attempt times out and a second attempt is sent
- **THEN** both attempts carry the same `webhook-id`, and each carries its own `webhook-timestamp`
  and signature

#### Scenario: Rotation grace

- **GIVEN** a hook rotated 1 hour ago
- **WHEN** it sends an attempt
- **THEN** `webhook-signature` holds two `v1,` entries, and one verifies with each secret

### REQ-5: Notification Body

The body MUST be a JSON object that carries identifiers and a summary only. A `todo.ready`
notification MUST carry exactly these fields:

```json
{"type": "todo.ready", "reason": "created", "todo_id": "td_…", "queue": "inbox",
 "kind": "pull_request", "source": "gitea", "summary": "PR #482 opened in …",
 "endpoint": "<slug>", "attempt": 1, "created_at": "2026-09-22T14:03:11Z"}
```

`reason` MUST be `created` or `requeued` (REQ-6). `attempt` MUST be the todo's current attempt
number. `summary` MUST be the todo's title after the neutralization the channel doorbell applies,
truncated to 200 characters. `kind` and `source` MUST be omitted when the todo has none.

The body MUST NOT contain any field of the todo's payload, any request headers, the routing trace,
any work order, or any other endpoint's data. It MUST NOT exceed 4 KiB.

The receiver documentation MUST state that `summary` is text supplied by the sender. It can inform
a consumer, and can never instruct one.

#### Scenario: No payload leaves

- **GIVEN** a todo whose payload holds an issue body, the sender's email and a `token` field
- **WHEN** its hook fires
- **THEN** the request body contains none of those values, and contains only the fields listed
  above

#### Scenario: A hostile summary stays data

- **GIVEN** a todo whose title contains a newline and the text `ignore previous instructions`
- **WHEN** its hook fires
- **THEN** `summary` is a single JSON string with the newline neutralized, and no other field
  carries sender text

### REQ-6: Trigger and Sender Gate

A hook MUST fire after the todo's transaction commits, from the same store hook that rings the
channel doorbell, and **only** for a todo that passes the SPEC-0011 sender gate: the delivery
verified under its source's trust mode, or it arrived on a token-trust self-managed webhook. A
todo that would not ring a session MUST NOT call a hook.

A hook MUST fire for these transitions into `pending`:

* `created`: a new todo commits;
* `requeued`: a claimed todo returns to `pending` because its lease expired, or a failed todo
  re-enters `pending` when its retry is due.

A hook MUST NOT fire for the doorbell heartbeat's re-rings (SPEC-0011 REQ "Doorbell Heartbeat"),
for redeliveries that dedup onto an existing todo, or for a todo that leaves `pending`.

Each transition MUST produce at most one notification per matching hook across all instances. The
instance whose statement performed the transition sends it.

A hook MUST fire whether or not a channel session is attached to the endpoint. When both exist,
both fire. The claim lease is what prevents double work.

A hook MUST NOT fire for a todo that another Switchboard feature marks as excluded from push. Today
that means synthetic self-test todos (SPEC-0025) and quarantined deliveries (SPEC-0026), which are
cited here by number until those specs merge.

#### Scenario: Unverified delivery calls no hook

- **GIVEN** an endpoint with a hook
- **WHEN** a todo is created from a delivery whose signature did not verify
- **THEN** the hook is not called, and the todo is still claimable by pull

#### Scenario: Lease expiry re-fires

- **GIVEN** a todo whose hook fired on creation, and a consumer that claimed it and died
- **WHEN** the reaper returns the todo to `pending`
- **THEN** the hook receives one `todo.ready` notification with `reason = "requeued"` and the new
  `attempt` number

#### Scenario: Redelivery does not re-fire

- **WHEN** a producer redelivers an event that dedups onto a live todo
- **THEN** no hook is called

#### Scenario: Two instances, one notification

- **GIVEN** two Switchboard instances
- **WHEN** a todo is created through instance 1
- **THEN** each matching hook receives exactly one notification for that transition, sent by
  instance 1

### REQ-7: Best-Effort Delivery Off the Ingest Path

Hook delivery MUST NOT block or slow ingest, todo creation, or any MCP verb. Notifications MUST be
handed to a bounded in-process queue on each instance. When that queue is full, the notification
MUST be dropped, logged and counted (REQ-11), and the todo MUST be unaffected.

Each attempt MUST time out after 5 seconds, covering the connection, TLS and response headers.
Switchboard MUST read at most 64 KiB of the response body and then discard it. A `2xx` status MUST
count as delivered.

A notification MUST have at most 3 attempts, with backoff of about 1 second and then about 5
seconds, jittered. A network error, a timeout, `408`, `429` or a `5xx` MUST be retried. Any other
status, including every `3xx` and every `4xx` except `408` and `429`, MUST end the notification
without retry.

Pending attempts MUST NOT be persisted. A restart loses in-flight notifications and nothing else.
The ADR-0013 contract holds: the todo stays `pending` and claimable.

#### Scenario: Unreachable receiver

- **GIVEN** an endpoint whose hook URL accepts no connections
- **WHEN** a verified delivery creates a todo
- **THEN** the ingest response time is unchanged, the todo is `pending` and claimable, and the
  hook records a failed delivery after 3 attempts

#### Scenario: Permanent client error is not retried

- **WHEN** a receiver answers the first attempt with `401`
- **THEN** no second attempt is sent, and the delivery is recorded as failed with status 401

#### Scenario: Queue overflow

- **GIVEN** the delivery queue on an instance is full
- **WHEN** another notification is produced
- **THEN** it is dropped, one warning is logged with the hook id and not the URL, the dropped
  counter increments, and no todo changes state

### REQ-8: Hook Health and Auto-Disable

Each hook MUST record `last_attempt_at`, `last_status` (the HTTP status, or null for a network
error), `last_error` (a short classified reason such as `timeout`, `tls`, `rejected_ssrf` or
`redirect`, never response body text), and `consecutive_failures` (failed deliveries in a row).

A delivered notification MUST reset `consecutive_failures` to 0. When `consecutive_failures`
reaches 10, the hook MUST be disabled with `disabled_reason = "consecutive_failures"` and
`disabled_at` set, and a warning MUST be logged. A disabled hook MUST NOT be called.

`list_notify_hooks` MUST show each hook's health and disabled state. A hook MUST be re-enabled only
by `rotate_notify_hook`, or by its owning human from the endpoint card (REQ-10).

#### Scenario: Dead receiver is disabled

- **GIVEN** a hook whose receiver has been down for 10 consecutive notifications
- **WHEN** an 11th todo becomes ready
- **THEN** the hook is not called, and `list_notify_hooks` shows `enabled = false` and
  `disabled_reason = "consecutive_failures"`

#### Scenario: Recovery resets the count

- **GIVEN** a hook with `consecutive_failures = 7`
- **WHEN** its next notification is delivered with `204`
- **THEN** `consecutive_failures` is 0

### REQ-9: Presence

This requirement applies once SPEC-0022 (endpoint presence) is implemented. Until then, every
endpoint's effective presence is `in`, and hooks fire under REQ-6 alone.

A hook with `ignore_presence = false` (the default) MUST NOT fire while its endpoint's effective
presence is `out`. Todos created while the endpoint is out stay `pending`, as SPEC-0022 requires.

When the endpoint's presence changes from `out` to `in` and it has at least one push-eligible
pending todo in the hook's effective queue set, each such hook MUST receive **one**
`todos.backlog` notification. It MUST be decided by the same conditional `presence_seen` update
that decides the SPEC-0022 digest, so a transition produces one notification per hook across all
instances and restarts. Its body MUST carry only counts, queue names, and the oldest pending age:

```json
{"type": "todos.backlog", "reason": "clock_in", "endpoint": "<slug>", "pending": 7,
 "queues": ["lane-m", "reviews"], "oldest_pending_seconds": 5400, "created_at": "…"}
```

`reason` MUST be one of SPEC-0022's digest reasons, excluding `reconnect`, which is a session
event and does not apply to hooks. The body MUST NOT contain any todo id, title or payload.

A hook with `ignore_presence = true` MUST fire under REQ-6 regardless of presence, and MUST NOT
receive `todos.backlog`.

#### Scenario: Clocked-out endpoint holds its hook

- **GIVEN** an endpoint that is `out`, with a hook that does not ignore presence
- **WHEN** 3 push-eligible todos are created
- **THEN** the hook is not called, and at clock-in it receives one `todos.backlog` with
  `pending = 3`

#### Scenario: An on-demand dispatcher ignores presence

- **GIVEN** an endpoint that is `out`, with a hook where `ignore_presence = true`
- **WHEN** a push-eligible todo is created
- **THEN** the hook receives `todo.ready` immediately, and no `todos.backlog` at clock-in

### REQ-10: Operator Web UI

The endpoint card MUST list the endpoint's hooks to its owning human. For each hook it MUST show
the URL's scheme, host and path, with the query string redacted, plus the queues, enabled state,
last status, last attempt time, consecutive failures and disabled reason. The owning human MUST be
able to disable, re-enable and delete a hook from the card. Only the endpoint's owner scope may see
or change its hooks. The instance operator role grants no view of another tenant's hooks.

#### Scenario: Human disables a noisy hook

- **WHEN** the owning human disables a hook from the endpoint card
- **THEN** the hook stops firing at once, and `list_notify_hooks` shows `enabled = false` with
  `disabled_reason = "operator"`

#### Scenario: Foreign human

- **WHEN** a signed-in human requests the hook controls for an endpoint another human owns
- **THEN** the response is not-found, and nothing changes

### REQ-11: Observability

Switchboard MUST add these series to the SPEC-0023 registry, with bounded labels only:

```
switchboard_notify_hook_notifications_total{type,outcome}  counter
   # type: todo.ready|todos.backlog; outcome: delivered|failed|dropped
switchboard_notify_hook_attempts_total{result}             counter
   # result: 2xx|3xx|4xx|5xx|timeout|network|tls|rejected_ssrf
switchboard_notify_hooks_disabled_total{reason}            counter
   # reason: consecutive_failures|operator
```

Hook ids, endpoint ids, URLs and hosts MUST NOT be labels.

Every attempt MUST produce a structured log line with the hook id, the notification id, the
attempt number, the result, and the duration. No log line, error, trace or API response may
contain a hook secret, a signature, or the URL's query string.

#### Scenario: Secret never logged

- **WHEN** a hook's attempts fail and succeed across a rotation
- **THEN** no log line contains either secret, any `webhook-signature` value, or the URL's query
  string

### REQ-12: Error Handling Standards

Every error from validation, signing, dialling or storage MUST be wrapped with the hook id and the
stage that failed. A delivery failure MUST NOT be returned to, or surface on, any ingest or MCP
call path. Sentinel errors MUST distinguish SSRF rejection, redirect, timeout, and permanent
client error, so that REQ-8 and REQ-11 can classify them. Nothing may be silently swallowed: each
error MUST be returned, recorded on the hook row, or logged with a documented reason.

#### Scenario: Store outage during health update

- **WHEN** recording a hook's health fails because the database is unavailable
- **THEN** the failure is logged with the hook id and notification id, and the delivery worker
  continues with the next notification

### REQ-13: Concurrency and Database Standards

The delivery queue and its workers MUST start and stop with the server's lifecycle context, drain
or abandon in-flight attempts on shutdown within a bounded time, and MUST be race-free under
`go test -race`. Hook creation MUST enforce the ceiling in the same transaction that inserts the
row. Health updates MUST be single parameterized statements, and MUST NOT take row locks that
ingest or claim paths wait on.

#### Scenario: Concurrent creates at the ceiling

- **GIVEN** an endpoint with 4 hooks and a ceiling of 5
- **WHEN** two `create_notify_hook` calls race
- **THEN** exactly one succeeds, and the other fails with `resource_exhausted`

## Security Requirements

### Authentication

The four verbs MUST require the endpoint's bearer credential or OAuth access token, as every MCP
verb does (SPEC-0007), plus the per-verb grant in REQ-2. The web UI controls MUST require a signed-in
human who owns the endpoint (`auth.RequireHuman` plus the ownership check that revoke uses).

| Surface | Auth | Description |
|---|---|---|
| MCP `create_notify_hook`, `list_notify_hooks`, `rotate_notify_hook`, `delete_notify_hook` | Required | Endpoint credential and per-verb grant |
| Web UI hook controls on the endpoint card | Required | Owning human session with CSRF token |
| Outbound POST to the hook URL | n/a (outbound) | Signed with the hook secret (REQ-4) |

### Rate Limiting

The verbs MUST sit behind the existing per-endpoint MCP rate limiter. Each hook MUST additionally
be limited to 120 notifications per minute. Notifications over the limit MUST be dropped and
counted as `dropped`, so that a hostile or runaway producer cannot turn Switchboard into a request
amplifier against the receiver.

### Security Headers

Web UI responses MUST carry the existing `secureHeaders` middleware set. Outbound requests MUST
NOT forward any inbound header, cookie or credential.

### Request Body Size Limits

MCP request bodies stay capped at 1 MiB (SPEC-0014). The outbound body MUST NOT exceed 4 KiB
(REQ-5). At most 64 KiB of a receiver's response MUST be read (REQ-7).

### CSRF Protection

The web UI's disable, re-enable and delete actions MUST be POST requests that carry the existing
CSRF token, and MUST be rejected without it.

### Redirect Validation

Outbound attempts MUST NOT follow redirects (REQ-3). The web UI's actions MUST redirect only to the
same-origin endpoint card.
