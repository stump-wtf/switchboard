---
status: draft
date: 2026-09-22
implements: [ADR-0034]
requires: [SPEC-0003, SPEC-0020, SPEC-0022, SPEC-0023]
---

# SPEC-0029: Notification Sinks and Queue Digests

## Overview

An owner (one human, or one team) configures **sinks**, Gotify servers and Apprise API servers, and
subscribes them to events about what they own: dead letters, quarantined deliveries, relay exhaustion,
routing rules that ask to notify, and a scheduled **queue digest**. Switchboard sends a short message
and is done. It keeps no human-facing work state. See
[ADR-0034](../../../adrs/ADR-0034-notification-sinks-and-queue-digests.md).

Sink credentials are rows in the connection vault that SPEC-0028 (reply to source, in flight) defines;
this spec adds the `gotify` and `apprise` kinds to it. The owner shape and team roles come from
SPEC-0033 (Teams and tenancy, in flight). Event sources defined elsewhere and in flight: quarantine
(SPEC-0026), unheard doorbells (SPEC-0025), attempt history (SPEC-0034), admission budgets
(SPEC-0030), and Harness relay attempts (Harness SPEC-0019). Until those land, the requirements that
depend on them apply once they do, and their digest sections read "unavailable".

It amends [SPEC-0020](../event-routing/spec.md) REQ "Rule Validation at Save Time" and REQ "Routing
Trace" with the `notify` action (REQ-8).

Priority: sinks and events (REQ-1 to REQ-11) are P1; the digest (REQ-12 to REQ-14) is P2.

## Requirements

### REQ-1: Notifications, Not Tickets

Switchboard MUST NOT add, for any purpose in this spec: a human-assignee field or lifecycle on todos, a
queue kind for human work, an acknowledge, snooze or resolve state for a notification, or an MCP verb
that files work for a human. A notification MUST be fire-and-forget: once delivered, or recorded as
undeliverable, Switchboard holds no further state about it beyond its delivery record.

#### Scenario: A dead letter is announced, not assigned

- **WHEN** a todo dead-letters and a subscribed sink is notified
- **THEN** the todo's state, owner and assignee are exactly what they would be with no sink configured

### REQ-2: Sinks

A sink MUST carry: an id; an owner scope (exactly one human or one team); a `name` unique in that
scope; a `kind` (`gotify` or `apprise`); a reference to a connection in the same owner scope and of the
same kind; `enabled`; and per-kind options. An owner scope MUST NOT have more than 10 sinks (an
operator-configurable ceiling).

Per-kind connection and options:

* `gotify`: connection account key is the server's HTTPS base URL; the secret is the application
  token. Options: a priority map from severity (`low`, `normal`, `high`) to Gotify priority, default
  `2`, `5`, `8`.
* `apprise`: connection account key is the Apprise API base URL. Exactly one of: a configuration key
  (option `config_key`, not secret) or a list of Apprise URLs (the connection's secret). Options: an
  optional `tag`, and a type map from severity to Apprise type, default `info`, `warning`, `failure`.
  An optional authorization header value MAY be stored in the secret for an Apprise API behind an
  authenticating proxy.

A sink MUST NOT reference a connection from another owner scope, checked on write and on every use.

#### Scenario: Stateful Apprise sink

- **WHEN** a human creates an Apprise sink with base URL `https://apprise.example.net` and
  `config_key = ops`
- **THEN** the sink is stored with no Apprise URLs, and deliveries go to `POST /notify/ops`

#### Scenario: Cross-scope connection

- **WHEN** a team admin creates a team sink referencing the admin's personal Gotify connection
- **THEN** the request fails with `forbidden`

### REQ-3: Gotify Delivery

A Gotify notification MUST be `POST {base}/message` with header `X-Gotify-Key` set to the token and a
JSON body carrying `title`, `message` and `priority`. The token MUST NOT be sent as a query parameter.
Any `2xx` is success.

#### Scenario: Token stays out of the URL

- **WHEN** a Gotify notification is delivered
- **THEN** the request URL contains no token and the `X-Gotify-Key` header carries it

### REQ-4: Apprise Delivery

An Apprise notification MUST be `POST {base}/notify/{config_key}` with `body`, `title`, `type` (and
`tag` when set), or `POST {base}/notify/` with `urls`, `body`, `title` and `type` for a stateless sink.
`format` MUST be `text`. `200` is success; `424` (at least one service failed) MUST be recorded as a
partial failure and retried at most once; `204` (no configuration) MUST be recorded as a configuration
error and not retried.

When a stateless sink is saved, every Apprise URL's scheme MUST be in the instance's Apprise scheme
allowlist. The default allowlist MUST exclude the generic HTTP schemes (`json`, `jsons`, `xml`, `xmls`,
`form`, `forms`) and local-system schemes (`windows`, `dbus`, `gnome`, `macosx`, `syslog`). A URL whose
scheme is not allowed MUST be refused with `forbidden_scheme` naming the scheme and never echoing the
URL.

#### Scenario: Generic HTTP scheme refused

- **WHEN** a human saves a stateless Apprise sink with a `json://` URL under the default allowlist
- **THEN** the save fails with `forbidden_scheme` naming `json`, and nothing is stored

#### Scenario: Partial failure

- **WHEN** the Apprise API answers `424`
- **THEN** the delivery is retried once and, if it answers `424` again, recorded as partially failed

### REQ-5: Sink Management

Sinks MUST be managed only by humans, through the web UI and the operator API: a human manages their
own; for a team, only roles SPEC-0033 allows to configure the team. There MUST be no MCP verb that
creates, edits, deletes, lists or reads a sink.

The surface MUST offer create, list, edit, enable or disable, delete, and **send test**. Send test MUST
deliver one message titled as a test through the sink, bypassing subscriptions and budgets but not the
SSRF guard, and report success or the error class. Deleting a sink MUST delete its subscriptions and
MUST NOT delete delivery records.

#### Scenario: Another owner's sink

- **WHEN** human B requests human A's sink through the operator API
- **THEN** the response is `404`

#### Scenario: Test message

- **WHEN** the owner presses send test on a Gotify sink
- **THEN** one message arrives on the Gotify server and the page reports success

### REQ-6: Events and Subscriptions

A subscription MUST link one sink to one or more event types, with optional filters (queues, endpoints,
webhooks, all within the sink's owner scope) and a minimum severity. Event types:

| Event | Fires | Default severity |
|---|---|---|
| `todo.dead_lettered` | a todo reaches a terminal failed state with no retry scheduled, from `fail` at the cap, lease expiry at the cap, endpoint revocation, or interrupt | high |
| `relay.exhausted` | as `todo.dead_lettered`, for a todo whose attempt history (SPEC-0034) records relay attempts; replaces, not adds to, `todo.dead_lettered` for that todo | high |
| `delivery.quarantined` | a delivery is quarantined (SPEC-0026) | normal |
| `rule.notify` | a routing rule with `notify` matches (REQ-8) | the rule's, default normal |
| `digest` | a digest is due (REQ-12) | low |

A manual re-queue of a dead letter followed by another dead letter MUST fire again. An event that no
subscription matches MUST produce no outbox row.

#### Scenario: Filtered subscription

- **GIVEN** a subscription to `todo.dead_lettered` filtered to queue `forge`
- **WHEN** todos dead-letter on `forge` and on `inbox`
- **THEN** only the `forge` dead letter is notified

#### Scenario: Relay exhaustion is one message

- **WHEN** a relay todo exhausts its attempts
- **THEN** a sink subscribed to both `todo.dead_lettered` and `relay.exhausted` receives one
  `relay.exhausted` message

### REQ-7: Event Ownership and Tenancy

Every event MUST be attributed to exactly one owner scope: a todo event to the todo's owner scope; a
quarantine or rule event to the owner scope of the webhook that received the delivery; a digest to the
scope it was scheduled for. An event MUST be delivered only to sinks in that scope. There MUST NOT be an
instance-wide or operator sink that receives tenant events.

#### Scenario: Friendship fan-out

- **GIVEN** human A's webhook routes to human B's endpoint and both have sinks subscribed to
  `todo.dead_lettered`
- **WHEN** B's todo dead-letters
- **THEN** B's sink is notified and A's is not

#### Scenario: Team todo

- **WHEN** a team-queue todo (SPEC-0033) dead-letters while claimed by a member's personal endpoint
- **THEN** the team's sinks are notified and the member's personal sinks are not

### REQ-8: The `notify` Routing Action

A routing action (SPEC-0020) MAY carry `notify` (a sink name) and `notify_severity` (`low`, `normal`,
`high`). `notify` MUST compose with every other action field, including `drop`. At save time
(`set_webhook_rules`, `add_webhook_rule`, `update_webhook_rule`) a `notify` naming a sink that does not
exist in the webhook's owner scope MUST be refused with `invalid_argument` naming the rule; a match at
evaluation time against a sink that has since been deleted or disabled MUST record a trace fault and
MUST NOT affect the delivery's routing. `test_webhook_rules` MUST report which rules would notify and
MUST NOT send anything. The routing trace MUST record the sink name when a notification was enqueued.

At most one `rule.notify` per delivery per sink MUST be enqueued, whichever rule matched.

#### Scenario: Drop and notify

- **GIVEN** a rule `{expr: ".sender.login == \"dependabot\"", action: {drop: true, notify: "phone"}}`
- **WHEN** a matching delivery arrives
- **THEN** no todo is created, the event is recorded as dropped, and one `rule.notify` is enqueued for
  sink `phone`

#### Scenario: Another owner's sink name

- **WHEN** an agent saves a rule whose `notify` names a sink that exists only in another human's scope
- **THEN** the save fails with `invalid_argument`

#### Scenario: Dry run sends nothing

- **WHEN** `test_webhook_rules` scores a candidate with a `notify` action against stored deliveries
- **THEN** the report lists the would-notify matches and no message is sent

### REQ-9: Message Content

A notification MUST carry: a title naming the event and queue; a one-line summary; the cause for
dead letters; a link to the todo or event on the board (built from `SWITCHBOARD_BASE_URL`); the origin's
display string or permalink when the todo has a reply address (SPEC-0028); and, for
`todo.dead_lettered` and `relay.exhausted`, the attempt count and the closing attempt's summary and
artifact handle when attempt history records them (SPEC-0034 REQ-15 "Dead-Letter Context for
Notifications"). The attempt summary is agent-written text and MUST be treated like a todo title below.

A notification MUST NOT carry any payload field, header, secret, credential fingerprint, or routing rule
expression. Sender-controlled and agent-written text (todo titles, subject titles, attempt summaries) MUST be
truncated to 120 characters,
stripped of control characters, and presented as the item's title, not as prose.

#### Scenario: No payload

- **WHEN** a dead letter's todo payload contains an issue body
- **THEN** the notification contains the todo title (truncated) and a board link, and no text from the
  issue body

### REQ-10: Durable Delivery

The outbox row for an event MUST be written in the same database transaction as the state change that
caused it: the dead-letter update, the quarantine insert, or the routed event insert. A rolled-back
state change MUST leave no outbox row.

A worker MUST claim outbox rows with `FOR UPDATE SKIP LOCKED`, so multiple instances never deliver one
row twice. A failed attempt MUST be retried with exponential backoff (starting at 30 seconds) for at
most 5 attempts; then the row MUST be marked `undeliverable` with the last error class. Each attempt
MUST time out within 10 seconds, MUST pass `internal/push.Validator` at dial time with the operator's
private-host allowlist, MUST require HTTPS (the `PushAllowHTTP` opt-in excepted), and MUST NOT follow
redirects. Delivery records MUST be retained for 30 days.

#### Scenario: Sink down

- **WHEN** a Gotify server is unreachable for an hour
- **THEN** the notification is attempted 5 times, marked `undeliverable`, and appears in the next
  digest's "could not deliver" section

#### Scenario: Two instances

- **GIVEN** two Switchboard instances draining one database
- **WHEN** one outbox row becomes due
- **THEN** exactly one instance delivers it

#### Scenario: Rolled-back dead letter

- **WHEN** the transaction that would dead-letter a todo rolls back
- **THEN** no notification is enqueued

### REQ-11: Deduplication, Budgets and Coalescing

Each event MUST carry a dedup key:

* `todo.dead_lettered`, `relay.exhausted`: the todo id plus its dead-letter count;
* `delivery.quarantined`: the webhook id plus a 10-minute window;
* `rule.notify`: the rule id plus the delivery's subject key (SPEC-0020), or the event id when there
  is no subject.

A second event with the same key and sink inside its window MUST increment the pending row's count
instead of adding a row, and the message MUST say "and N more".

Each sink MUST have a budget, default 30 messages per hour with a burst of 10, owner-configurable within
operator bounds. When exhausted, due rows MUST be held; when budget returns, all held rows for that sink
MUST be delivered as one coalesced message listing counts by event type and a board link. Test messages
MUST NOT consume budget. Budgets MUST be computed from delivery records, so they hold across instances.

#### Scenario: Quarantine storm

- **WHEN** 200 deliveries from one untrusted sender are quarantined in 5 minutes on one webhook
- **THEN** the sink receives one quarantine message saying "and 199 more"

#### Scenario: Budget exhausted

- **WHEN** 50 distinct todos dead-letter in one minute on one sink with the default budget
- **THEN** the sink receives at most 10 individual messages and then one coalesced message when budget
  returns

### REQ-12: Queue Digest Schedule

An owner scope MAY define digests. A digest MUST carry: a name; a schedule; the queues it covers
(default: all the scope owns); its sink targets and/or a Cairn target; and `skip_when_quiet` (default
true).

The schedule MUST use SPEC-0022's zone prefix and day-spec grammar followed by one or more `HH:MM` send
times, for example `TZ=America/Los_Angeles Mon-Fri 09:00,17:00`. The zone database MUST be embedded. An
invalid schedule MUST be refused with `invalid_argument` naming `schedule`. A digest MUST be sent at
most once per scheduled time across all instances (claimed by an atomic update of `last_sent_at`), and
a scheduled time missed during downtime MUST be sent once on recovery if less than one hour late, and
otherwise skipped. An owner scope MUST NOT have more than 5 digests.

#### Scenario: One send across instances

- **GIVEN** two instances and a digest due at 09:00
- **WHEN** 09:00 passes
- **THEN** exactly one digest is built and sent

#### Scenario: Invalid schedule

- **WHEN** a human saves `Mon-Fri 25:00`
- **THEN** the save fails with `invalid_argument` naming `schedule`

### REQ-13: Digest Content and Honest Absence

A digest MUST report, per covered queue and since the previous digest (or the last 24 hours for the
first): pending count and oldest pending age; claimed count; stalled leases (claimed todos whose lease
has expired but that the reaper has not yet processed, and claimed todos with no heartbeat for more than
twice their lease TTL); dead letters (count and up to 5 todo titles with board links); quarantined
deliveries (count by webhook, from SPEC-0026); unheard doorbells per endpoint (from SPEC-0025);
endpoints clocked out (SPEC-0022); work deferred by a queue's admission budget (SPEC-0030); and
notifications recorded `undeliverable`.

A section whose data could not be computed (its source not built yet, or a query failure or timeout)
MUST be shown as "unavailable" with the reason, and MUST NOT be shown as zero. The digest is **quiet**
when every section is available and dead letters, quarantined deliveries, stalled leases, unheard
doorbells and undeliverable notifications are all zero and no queue's oldest pending age exceeds the
digest's threshold (default 1 hour). A quiet digest with `skip_when_quiet` MUST NOT be sent; a digest
with any unavailable section MUST NOT be considered quiet.

The digest MUST contain only counts, ages, queue and endpoint names, todo titles (truncated per REQ-9)
and board links, and only for the digest's owner scope.

#### Scenario: Unmeasured is not zero

- **GIVEN** the unheard-doorbell source (SPEC-0025) is not deployed
- **WHEN** a digest is built
- **THEN** its unheard-doorbell section reads "unavailable: not measured on this instance" and the
  digest is not treated as quiet

#### Scenario: Quiet day

- **WHEN** every section is available and every trouble count is zero, with `skip_when_quiet` true
- **THEN** no digest is sent

#### Scenario: Another owner's queues

- **WHEN** human A's digest is built on an instance where human B owns a queue of the same name
- **THEN** A's digest counts only A's todos

### REQ-14: Digest to Cairn

A digest MAY target a Cairn connection (SPEC-0028 kind `cairn`) in the same owner scope. When it does,
Switchboard MUST publish the digest as a Markdown artifact through that connection with a TTL (default
7 days, at most Cairn's maximum) and the tag `switchboard-digest`, and any sink targets MUST receive a
short message linking the artifact instead of the full digest. If publishing fails, the sinks MUST
receive the full digest text and the failure MUST be recorded.

#### Scenario: Cairn-linked digest

- **WHEN** a digest targets a Cairn connection and a Gotify sink
- **THEN** a Cairn artifact is created and the Gotify message contains its link and the headline counts

### REQ-15: Error Handling Standards

Errors MUST be wrapped with context at each boundary (store, outbox worker, sink adapter, digest
builder). Sentinel errors MUST exist for the stable codes in this spec (`forbidden_scheme`,
`encryption_required`, `invalid_argument`, `forbidden`, `not_found`). Every delivery failure MUST be
recorded on its outbox row with an error class and logged with structured fields (sink id, event type,
owner scope, class), never with a token, Apprise URL, or message body.

#### Scenario: Structured failure

- **WHEN** a Gotify server answers `401`
- **THEN** the row records class `auth`, the log line carries the sink id and class, and no token
  appears anywhere

## Security Requirements

### Authentication

| Surface | Auth | Justification |
|---|---|---|
| Web UI sink, subscription and digest pages | Required | Human session with CSRF; owner or team role per REQ-5. |
| `GET /api/v1/sinks` | Required | Operator OAuth bearer; results limited to the caller's scopes. |
| `POST /api/v1/sinks` | Required | As above. |
| `PATCH /api/v1/sinks/{id}` | Required | As above. |
| `DELETE /api/v1/sinks/{id}` | Required | As above. |
| `POST /api/v1/sinks/{id}/test` | Required | As above. |
| `GET/POST/DELETE /api/v1/sinks/{id}/subscriptions` | Required | As above. |
| `GET/POST/PATCH/DELETE /api/v1/digests` | Required | As above. |

There are no public endpoints in this capability.

### Rate Limiting

Deliveries are bounded per sink by REQ-11. Management routes MUST share the operator API's per-human
limiter; send test MUST be limited to 6 per minute per sink.

### Security Headers

Pages and API responses MUST carry the operator routes' standard headers (`Content-Security-Policy`,
`X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff`, `Referrer-Policy:
strict-origin-when-cross-origin`). Outbound deliveries are client requests and are not subject to them.

### Request Body Size Limits

Management bodies MUST be bounded at 16 KiB.

### CSRF Protection

Web UI forms MUST use the existing CSRF protection. The operator API is bearer-authenticated.

### Redirect Validation

Sink deliveries MUST NOT follow redirects (REQ-10). Post-save web redirects MUST go only to fixed
internal paths.
