---
status: approved
date: 2026-09-22
implements: [ADR-0033]
requires: [SPEC-0001, SPEC-0006, SPEC-0011, SPEC-0020]
related: [SPEC-0029, SPEC-0032, SPEC-0033]
---

# SPEC-0028: Reply to Source

## Overview

A todo that came from a conversation carries a **reply address**: the Slack thread, forge issue or pull
request, or Cairn artifact the delivery was about. Switchboard derives it from the verified delivery at
ingest. An agent answers with the `reply` verb, naming only the todo and the text; Switchboard posts
with a **connection**, an outbound credential owned by the todo's human or team, and records the reply
on the todo. See [ADR-0033](../../../adrs/ADR-0033-reply-to-source.md).

This spec amends three others, which it names rather than edits:

* [SPEC-0011](../channels/spec.md) REQ "Push Notification Shape": `meta` gains `reply_to`
  (REQ-2 below).
* [SPEC-0006](../agent-tools/spec.md) REQ "Todo Drain Verbs": `todoOut` gains `reply_to` and
  `replies`; a new `reply` verb joins the grantable surface (REQ-6).
* [SPEC-0020](../event-routing/spec.md) REQ "Routing Trace": a new trace cause, `self_reply`
  (REQ-11).

Owner scope, team roles and the rule that credential selection follows the todo's owner scope come from
ADR-0038 and [SPEC-0033](../teams-tenancy/spec.md) (Teams and tenancy). Until SPEC-0033 is
implemented, "owner scope" means the owning human, and every requirement below that names a team
applies once teams exist.

Terms:

* **Reply address**: a small, typed, immutable record on a todo naming where an answer goes.
* **Connection**: an owner-scoped outbound credential of one kind, bound to one account key.
* **Account key**: what a connection is for: a forge host, a Slack team id, or a Cairn base URL.
* **Owner scope**: exactly one human or one team.

## Requirements

### REQ-1: Reply Address Derivation

Switchboard MUST derive a reply address at ingest, in Go, from the verified request body, after
verification and before the todos are written. It MUST NOT be derived from a routing rule, from agent
input, or from any unsigned header. It MUST be stored on every todo the delivery mints, identically on
each fan-out target, and MUST NOT change after the todo is written.

A reply address MUST be derived only when the delivery was verified by its source's signing scheme
(`trust_mode = signed` and `verified = true`). A `generic` (token-trust) delivery MUST NOT receive a
reply address, including when it carries forge event headers or a forge-shaped body.

Addresses by source:

| Source | Events | Kind | Fields |
|---|---|---|---|
| `github` | `issues`, `issue_comment`, `pull_request`, `pull_request_review`, `pull_request_review_comment` | `forge_comment` | `provider`, `host`, `repo`, `number` |
| `gitea` | as `github` | `forge_comment` | `provider`, `host`, `repo`, `number` |
| `slack` | `event_callback` whose inner event has a `channel` and a `ts` | `slack_thread` | `team_id`, `channel`, `thread_ts` |
| `cairn` | artifact events | `cairn_comment` | `instance`, `artifact_id` |
| `stripe`, `generic` | all | none | none |

`host` MUST be the host of the repository's `html_url` in the verified body. `thread_ts` MUST be the
inner event's `thread_ts` when present, else its `ts`, so a top-level message is answered in a new
thread under it. `instance` MUST be the scheme and host of the artifact URL in the verified Cairn body.
Every field MUST match a strict charset (`repo`: `owner/name` of `[A-Za-z0-9._-]`; `number`: a positive
integer; Slack ids: `[A-Z0-9]`; `thread_ts`: digits, one dot, digits; `artifact_id`: the Cairn id
charset); a value that fails MUST yield no address rather than a sanitized one.

An event not in the table MUST yield no address. Adding the `linear` and `plain` kinds is left to
SPEC-0032's kinds (ADR-0037).

#### Scenario: Slack message in a channel

- **GIVEN** a Slack webhook whose signature verifies
- **WHEN** an `event_callback` arrives whose inner event is a message with `channel = C0123` and
  `ts = 1726990000.000100` and no `thread_ts`
- **THEN** every todo it mints carries `reply_to = {kind: slack_thread, team_id, channel: C0123,
  thread_ts: 1726990000.000100}`

#### Scenario: Reply inside an existing thread

- **WHEN** the inner event carries `thread_ts = 1726980000.000200`
- **THEN** the address's `thread_ts` is `1726980000.000200`, not the message's own `ts`

#### Scenario: Pull request comment on Gitea

- **WHEN** a verified Gitea `issue_comment` arrives for PR 482 in `stump.wtf/switchboard` whose
  `repository.html_url` host is `gitea.example.net`
- **THEN** the address is `forge_comment` with `host = gitea.example.net`,
  `repo = stump.wtf/switchboard`, `number = 482`

#### Scenario: Generic webhook carrying a forge payload

- **WHEN** a `generic` webhook receives a body shaped like a GitHub `issues` event with
  `X-GitHub-Event: issues`
- **THEN** the todo has no reply address

#### Scenario: Unsafe field

- **WHEN** a verified forge body carries a repository full name containing `/../`
- **THEN** no address is derived and the todo is otherwise created normally

#### Scenario: Fan-out

- **WHEN** a verified delivery fans out to the owner's endpoint and a friend's endpoint
- **THEN** both todos carry the same address, and each is answered, if at all, with its own owner
  scope's connection (REQ-5)

### REQ-2: Reply Address Exposure

`todoOut` (SPEC-0006) MUST include `reply_to` when the todo has an address: its `kind`, the
non-secret fields, and a one-line `display` string (for example `slack #C0123 thread 1726990000.000100`
or `gitea stump.wtf/switchboard#482`). It MUST NOT include any connection identifier, credential,
fingerprint or URL containing a credential.

The doorbell's `meta` (SPEC-0011 REQ "Push Notification Shape") MUST include `reply_to` set to the
address kind when the todo has one, and MUST omit it otherwise. The value MUST pass the same
neutralization as the other `meta` values. The session instructions MUST tell the agent that a
doorbell carrying `reply_to` can be answered with `reply`, passing its `todo_id`, and that the text is
posted publicly to the origin.

The address is a statement about where the work came from, not a claim that a reply will succeed:
exposure MUST NOT depend on whether a matching connection exists.

#### Scenario: Doorbell for a Slack-born todo

- **WHEN** a todo with a `slack_thread` address rings a session
- **THEN** `meta` contains `todo_id`, `queue`, `kind`, `source` and `reply_to = slack_thread`

#### Scenario: Doorbell without an address

- **WHEN** a Stripe-born todo rings
- **THEN** `meta` has no `reply_to` key

### REQ-3: Connections

A connection MUST carry: an id; an owner scope (exactly one of owning human or owning team); a `kind`
(`github`, `gitea`, `slack`, `cairn`); a `name` unique within the owner scope; an account key; a
secret; an optional targets allowlist; an optional endpoints allowlist; `created_by`; and timestamps for
creation, last rotation and last use.

Account keys by kind: `github` a host (default `github.com`); `gitea` an HTTPS base URL; `slack` a team
id; `cairn` an HTTPS base URL. Within an owner scope, `(kind, account key)` MUST be unique.

The secret MUST be stored through the `internal/cred` envelope (AES-256-GCM, `enc:v1:`). If no secret
encryption key is configured, creating or rotating a connection MUST be refused with
`encryption_required`; a plaintext connection secret MUST never be written. The secret MUST be
write-only: no API response, page, MCP result, log line, metric label or error message may contain it.
Surfaces MAY show a fingerprint: the first 12 hex characters of the SHA-256 of the secret.

A targets allowlist entry MUST be, per kind: a repository glob (`owner/*` or `owner/name`) for forges,
a channel id for Slack, an artifact tag or `*` for Cairn. An empty allowlist admits every target the
credential itself can reach.

#### Scenario: Create without an encryption key

- **GIVEN** `SWITCHBOARD_SECRET_ENCRYPTION_KEY` is unset
- **WHEN** a human creates a Slack connection
- **THEN** the request fails with `encryption_required` and nothing is stored

#### Scenario: Secret is never read back

- **WHEN** the owner lists connections, opens one, or rotates one
- **THEN** each response contains the fingerprint and no secret

#### Scenario: Duplicate account key

- **WHEN** a human who already has a `github` connection for `github.com` creates another for
  `github.com`
- **THEN** the request fails with `conflict`

### REQ-4: Connection Management

Connections MUST be manageable by humans, through the web UI and the operator API, and only within
their own scope: a human manages connections they own; for a team, only a role ADR-0038 permits to
configure the team (team owners and admins) may create, rotate or delete, and members MAY see names,
kinds, account keys and fingerprints but nothing more.

Connections MAY also be managed over MCP through `ConnectionVerbs()`: `list_connections`,
`create_connection`, `rotate_connection` and `delete_connection`. The family MUST be off by default:
it MUST NOT be in the operator API's basics vend, the vend wizard, quick vend and consent screen MUST
list it unchecked, and the consent screen MUST say that the holder can create, replace and delete the
owner's outbound credentials. A granted endpoint MUST act within its owner scope with the permissions
REQ-4 gives that owner, so a team endpoint may manage team connections only while ADR-0038 lets its
grant configure the team. Secrets passed to `create_connection` and `rotate_connection` are
write-only input under REQ-3; they MUST be replaced with `«redacted»` in any log or trace of tool
arguments. `list_connections` MUST return names, kinds, account keys, allowlists and fingerprints
only. Every write MUST record the acting endpoint and its accountable human alongside `created_by`.

The management surface MUST offer: create, list, rotate secret (replace), edit allowlists, delete, and
**test**. Test MUST make one harmless authenticated read against the provider (for example Slack
`auth.test`, a forge "current user" call, a Cairn token introspection) and report success or the
provider's error class, without posting anything.

Deleting a connection MUST NOT delete reply rows; they keep the connection's name and fingerprint as of
the reply.

#### Scenario: Another human's connection

- **WHEN** human B requests human A's connection by id through the operator API
- **THEN** the response is `404`, indistinguishable from an unknown id

#### Scenario: Team member cannot rotate

- **GIVEN** a team connection and a human who is a member but not an admin of that team
- **WHEN** they try to rotate it
- **THEN** the request is refused with `forbidden` and the secret is unchanged

#### Scenario: Connection verbs are off by default

- **WHEN** an endpoint minted by the basics vend, or vended in the wizard without touching the
  connection chips, calls `tools/list`
- **THEN** no connection-management tool is listed

#### Scenario: Granted endpoint stores a connection

- **GIVEN** an endpoint whose owner granted `create_connection` and `list_connections`
- **WHEN** it creates a Slack connection with a bot token
- **THEN** the connection is sealed under REQ-3 and owned by the endpoint's owner scope,
  `list_connections` shows its fingerprint and no secret, and no log line carries the token

#### Scenario: Granted endpoint stays in its scope

- **GIVEN** endpoint E, granted `rotate_connection`, owned by human A
- **WHEN** E names human B's connection id
- **THEN** the call fails with `not_found`, indistinguishable from an unknown id

### REQ-5: Credential Resolution

For a `reply` on todo T, Switchboard MUST select the connection as follows:

1. the owner scope is **T's** owner scope (the owner of T's endpoint, or T's team when T is a team
   todo under SPEC-0033), never the calling endpoint's owner if different and never the owner of the
   webhook that minted T;
2. among that scope's connections, the one whose `kind` matches the address kind's provider and whose
   account key equals the address's `host` (forges), `team_id` (Slack) or `instance` (Cairn), compared
   case-insensitively for hosts and exactly otherwise;
3. that connection's targets allowlist, if non-empty, MUST admit the address, and its endpoints
   allowlist, if non-empty, MUST contain the calling endpoint.

If no connection satisfies all three, the reply MUST be refused with `no_connection` (or
`target_not_allowed` when a connection matched but its allowlist refused) and nothing is dialed. A
connection from any other owner scope MUST never be considered.

#### Scenario: Cross-owner fan-out

- **GIVEN** human A's GitHub webhook routes to human B's endpoint by an approved route, and only A has a
  GitHub connection
- **WHEN** B's agent calls `reply` on its todo
- **THEN** the result is `no_connection` and A's connection is not used

#### Scenario: Host mismatch

- **GIVEN** the owner's only forge connection is `gitea` for `https://gitea.example.net`
- **WHEN** a todo's address has `host = git.attacker.example`
- **THEN** the result is `no_connection` and no request is made to either host

#### Scenario: Team credential stays with team todos

- **GIVEN** a team with a Slack connection, and a team member whose personal endpoint has a todo from
  their own personal Slack webhook
- **WHEN** that personal todo is answered with `reply`
- **THEN** the team's connection is not considered; only the member's own connections are

#### Scenario: Personal endpoint working a team todo

- **GIVEN** a member's personal endpoint holding the claim on a team-queue todo (SPEC-0033)
- **WHEN** it calls `reply`
- **THEN** the team's connection is used, because the todo's owner scope is the team

#### Scenario: Allowlist refuses

- **GIVEN** a GitHub connection with targets `acme/*`
- **WHEN** the address is `other/repo#7`
- **THEN** the result is `target_not_allowed`

### REQ-6: The `reply` Verb

`reply` MUST take `todo_id` (required), `text` (required, non-empty after trimming) and
`idempotency_key` (optional, at most 128 bytes of printable ASCII). It MUST NOT accept an address,
connection, provider or URL.

`reply` MUST be a grantable verb, enforced by the scope guard like every other verb. The vend wizard,
the quick vend and the consent screen MUST list it, unchecked by default. It MUST NOT be granted by the
operator API's basics vend, which today grants `AllVerbs()` verbatim (`internal/server/api.go`): that
path MUST grant `BasicVerbs()`, which is `AllVerbs()` without the opt-in families: the reply and
connection families (REQ-4), and the families other specs mark opt-in the same way (SPEC-0029's
notification verbs, SPEC-0030's admission-policy verbs). Posting to a third party and handling the
owner's outbound credentials are always deliberate grants.

A call MUST be refused, before any dial, when:

* the todo does not exist or does not belong to the calling endpoint: `not_found`;
* the todo has no reply address: `no_reply_address`;
* the todo is `pending`, or claimed by someone else: `not_claimed`;
* the todo is `done` or `failed` and either the caller was not the endpoint that completed or failed
  it, or more than the grace window (15 minutes, operator-configurable up to 24 hours) has passed:
  `reply_window_closed`;
* REQ-5 finds no connection: `no_connection` or `target_not_allowed`;
* a limit in REQ-10 is exhausted: `rate_limited`, with a retry-after in seconds;
* REQ-12 refuses the text: `invalid_argument` or `secret_detected`.

The result MUST carry `reply_id`, `state` (`sent`, `failed`, `unknown`), `duplicate` (true when the
idempotency key matched an earlier call), `display` (the address display string) and, when the provider
returned one, `permalink`. A `failed` or `unknown` result MUST carry a provider error class
(`auth`, `not_found`, `forbidden`, `rate_limited`, `invalid`, `server`, `network`) and no provider
response body.

#### Scenario: Reply while holding the claim

- **GIVEN** endpoint E holds the claim on a todo with a `forge_comment` address and a matching
  connection
- **WHEN** E calls `reply {todo_id, text: "Picked this up."}`
- **THEN** one comment is created on the issue and the result is `sent` with its permalink

#### Scenario: Outcome after complete

- **WHEN** E completes the todo and calls `reply` 3 minutes later
- **THEN** the reply is sent

#### Scenario: Window closed

- **WHEN** E calls `reply` 20 minutes after completing, with the default grace window
- **THEN** the result is `reply_window_closed` and nothing is posted

#### Scenario: Another endpoint's todo

- **WHEN** endpoint F calls `reply` with a todo id owned by E
- **THEN** the result is `not_found`

#### Scenario: Verb not granted

- **WHEN** an endpoint without `reply` in its scope calls it
- **THEN** the call fails with `forbidden` and the tool is absent from its `tools/list`

### REQ-7: Provider Delivery

Each kind MUST post through exactly one provider call, to the connection's configured base URL only:

| Kind | Call |
|---|---|
| `forge_comment` on `github` | `POST {api}/repos/{repo}/issues/{number}/comments`, body `{"body": text}`; `api` is `https://api.github.com` for `github.com`, else `https://{host}/api/v3` |
| `forge_comment` on `gitea` | `POST {base}/api/v1/repos/{repo}/issues/{number}/comments`, body `{"body": text}` |
| `slack_thread` | `POST https://slack.com/api/chat.postMessage`, body `{"channel", "thread_ts", "text"}`; a `200` with `"ok": false` is a failure |
| `cairn_comment` | `POST {base}/v1/artifacts/{artifact_id}/comments`, body `{"body": text}` |

Path components MUST be the validated address fields (REQ-1), URL-escaped. The credential MUST be sent
only in the provider's authorization header, and only to the connection's base URL. Every dial MUST go
through `internal/push.Validator` at dial time, with the operator's private-host allowlist applied;
HTTPS MUST be required (the existing `PushAllowHTTP` opt-in excepted); redirects MUST NOT be followed
and a `3xx` MUST be a failure. Each attempt MUST time out within 10 seconds.

#### Scenario: Slack returns ok false

- **WHEN** `chat.postMessage` answers `200` with `{"ok": false, "error": "not_in_channel"}`
- **THEN** the reply is `failed` with class `forbidden`, and the recorded row keeps the provider's
  error code `not_in_channel`

#### Scenario: Private Gitea not allowlisted

- **GIVEN** a Gitea connection whose host resolves to a private address and an operator allowlist that
  does not include it
- **WHEN** a reply would dial it
- **THEN** no connection is opened and the result is `failed` with class `network`

#### Scenario: Redirect

- **WHEN** the provider answers `302`
- **THEN** the redirect is not followed and the reply is `failed`

### REQ-8: Idempotency

Before dialing, Switchboard MUST insert the reply row with an idempotency key: the caller's key, or,
when absent, `sha256(todo_id + "\x00" + text)`. Keys MUST be unique per todo. A call whose key already
has a row MUST NOT dial and MUST return that row's outcome with `duplicate = true`. Two concurrent calls
with one key MUST produce exactly one dial.

Retries within one call: a dial that failed before the request was written (DNS, connect, TLS) or a
`429` MAY be retried up to twice, honouring `Retry-After` up to 10 seconds. Any other failure after the
request may have been written (timeout while awaiting a response, connection reset, `5xx`) MUST be
recorded `unknown` and MUST NOT be retried automatically, because none of the providers deduplicate.

#### Scenario: Retried tool call

- **WHEN** an agent calls `reply` with `idempotency_key = done-1`, the post succeeds, and the MCP
  response is lost, and the agent calls again with the same key
- **THEN** exactly one comment exists and the second result is `sent` with `duplicate = true`

#### Scenario: Timeout after write

- **WHEN** the provider accepts the connection and the request, then does not answer within 10 seconds
- **THEN** the row is `unknown`, the result is `unknown`, and no second request is made

### REQ-9: Recording

Every `reply` call that passes REQ-6's pre-dial checks MUST produce exactly one reply row carrying:
reply id, todo id, calling endpoint id, connection id with its name and fingerprint at the time, owner
scope, address kind and display, idempotency key, `text_sha256`, the text itself, state, provider error
class and code, provider object id and permalink, and timestamps. Refused calls MUST NOT produce rows.

The todo's replies MUST be visible to the todo's owner scope on the board's todo drawer and through
`todoOut.replies` (count, last state, last permalink). Reply rows MUST be deleted with their todo under
the retention policy (ADR-0002), and never before it.

#### Scenario: Drawer shows replies

- **WHEN** the owner opens a todo that has two replies
- **THEN** the drawer lists both with state, time, display and permalink, and no credential material

### REQ-10: Rate Limits

Switchboard MUST enforce, and refuse with `rate_limited` plus a retry-after:

* per calling endpoint: a token bucket, default 30 per minute with a burst of 10;
* per todo: a lifetime cap, default 20, which the owner scope MAY raise to at most 100;
* per connection: after a provider `429`, no further dial on that connection until its `Retry-After`
  (or 30 seconds when absent) has passed.

Limits MUST be evaluated before the reply row is inserted, so a refused call consumes no idempotency
key. The per-todo cap MUST be counted from reply rows, so it holds across instances and restarts.

#### Scenario: Looping agent

- **WHEN** an agent calls `reply` for the 21st time on one todo
- **THEN** the call is refused with `rate_limited` and nothing is posted

#### Scenario: Provider throttles

- **WHEN** Slack answers `429` with `Retry-After: 20`
- **THEN** replies through that connection are refused with `rate_limited` for 20 seconds, while other
  connections are unaffected

### REQ-11: Echo Suppression

An inbound delivery MUST be recorded with routing cause `self_reply` and MUST mint no todo when either:

* the delivery's provider object id (the forge comment id, the Slack message `ts` within its channel,
  the Cairn comment id) equals the provider object id of a reply row sent within the last 24 hours in
  the same owner scope; or
* it is a Slack event whose `bot_id` equals the bot id recorded for a connection in that owner scope by
  its last successful test or reply.

The event row and its trace MUST persist as for any drop (SPEC-0020 REQ "Drop Action Semantics"). An
owner MUST be able to see these on the events view with the `self_reply` cause.

#### Scenario: Our own comment comes back

- **GIVEN** a reply created GitHub comment 99001 on an issue the owner's webhook watches
- **WHEN** GitHub delivers `issue_comment.created` for comment 99001
- **THEN** the event is recorded with cause `self_reply` and no todo is created

#### Scenario: A human answers in the thread

- **WHEN** a human posts in the same Slack thread after the agent's reply
- **THEN** the delivery is routed normally and a todo is created

### REQ-12: Content Limits and Secret Scanning

`text` MUST be refused with `invalid_argument` when empty after trimming, larger than 16 KiB, or larger
than the target provider's documented limit. `text` MUST be posted as given: Switchboard MUST NOT add
mentions, rewrite links, or render it. An owner scope MAY configure a signature line that is appended
to every reply through a connection (for example the endpoint slug), set on the connection by a human.

Before dialing, Switchboard SHOULD scan `text` with a high-confidence secret rule set (the gitleaks rule
set Cairn adopts for its server-side redaction) and refuse a match with `secret_detected`, naming the
rule id and never the matched value.

#### Scenario: A token in the reply

- **WHEN** the text contains a string matching the GitHub personal access token rule
- **THEN** the call is refused with `secret_detected` naming the rule, nothing is posted, and no reply
  row is written

### REQ-13: Error Handling Standards

Every error MUST be wrapped with context at each boundary (store, connection resolution, provider
adapter). Sentinel errors MUST exist for each stable code in REQ-6 so the tool layer maps them without
string matching. No error may be swallowed: every provider failure MUST be recorded on its row and
logged with structured fields (reply id, todo id, endpoint slug, connection id, provider class and
code), never with the text, the credential, or a provider response body.

#### Scenario: Provider adapter failure is structured

- **WHEN** the Gitea adapter receives a `401`
- **THEN** the row is `failed` with class `auth`, the log line carries the connection id and class,
  and the tool result carries class `auth` and no response body

## Security Requirements

### Authentication

| Surface | Auth | Justification |
|---|---|---|
| MCP `reply` | Required | Vended endpoint credential; `reply` must be in the endpoint's scope. |
| MCP connection verbs | Required | Vended endpoint credential; the verb must be in scope (off by default, REQ-4). |
| Web UI connection pages | Required | Human session with CSRF; owner or team role per REQ-4. |
| `GET /api/v1/connections` | Required | Operator OAuth bearer; results limited to the caller's scopes. |
| `POST /api/v1/connections` | Required | As above; owner or team admin. |
| `PUT /api/v1/connections/{id}/secret` | Required | As above. |
| `PATCH /api/v1/connections/{id}` | Required | As above. |
| `DELETE /api/v1/connections/{id}` | Required | As above. |
| `POST /api/v1/connections/{id}/test` | Required | As above. |

There are no public endpoints in this capability.

### Rate Limiting

`reply` is limited per endpoint, per todo and per connection (REQ-10). Connection management routes
MUST share the operator API's existing per-human limiter. `test` MUST be limited to 6 calls per minute
per connection, because it makes an authenticated call to a third party.

### Security Headers

Connection pages and API responses MUST carry the headers every other operator route carries:
`Content-Security-Policy`, `X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff`,
`Referrer-Policy: strict-origin-when-cross-origin`. Outbound provider calls are client requests and are
not subject to them.

### Request Body Size Limits

Connection create and update bodies MUST be bounded at 16 KiB. The `reply` arguments are bounded by
the MCP transport limit and by REQ-12's 16 KiB text cap.

### CSRF Protection

Web UI connection forms MUST use the web UI's existing CSRF protection. The operator API is
bearer-authenticated and not cookie-based.

### Redirect Validation

Provider calls MUST NOT follow redirects (REQ-7). The web UI's post-save redirects MUST go only to
fixed internal paths.
