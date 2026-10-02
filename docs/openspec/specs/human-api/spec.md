---
status: draft
date: 2026-09-27
implements: [ADR-0023, ADR-0038]
extends: [SPEC-0033]
requires: [SPEC-0006, SPEC-0007, SPEC-0020, SPEC-0034]
related: [ADR-0012, ADR-0022, ADR-0024, ADR-0025]
---

# SPEC-0035: Human API and CLI Parity for Webhooks, Rules, Routes, Events and Todos

## Overview

A human can manage an agent's webhooks, routing rules, routes, delivery history and todos today only
through that agent's MCP endpoint, or partly through the board. The human API at `/api/v1` — the
OAuth-guarded surface the `switchboard` CLI drives — has exactly five routes: vend, list and revoke
endpoints, push a todo, and list agents. So a human at a terminal cannot answer "why did this issue
never reach a lane?" without borrowing an agent's credential, and the CLI cannot show or change the
rules that decide it.

This spec adds the missing human routes and a CLI command for each, and makes parity a tested
property rather than a hope:

* **Webhooks** of an endpoint the human owns: list, create, delete.
* **Rules** on a webhook the human owns: list, replace, add, update, remove, and test without saving.
* **Routes** on a webhook the human owns: list, add, remove.
* **Delivery history** in the human's reach: list, show, and replay one event.
* **Todos** in the human's reach: list, show with attempts, and the board's recovery actions.

Every route runs the same validation and mutation code as its MCP verb. Nothing is reimplemented for
the API. The new routes are **human** surfaces in ADR-0038's sense, not operator surfaces: they are
scoped by the calling human's reach (SPEC-0033 REQ "Reach and Effective Reach"), and the instance
operator gains nothing from them.

This spec realizes [ADR-0023](../../../adrs/ADR-0023-mvp-mcp-api-first-basics.md) (MCP/API-first)
and applies [ADR-0038](../../../adrs/ADR-0038-teams-and-tenancy.md) (reach, and the naming of human
versus operator surfaces). It extends [SPEC-0033](../teams-tenancy/spec.md), whose REQ "Human API for
Teams" already places human management on `/api/v1` and off MCP. It requires
[SPEC-0006](../agent-tools/spec.md) (the verbs being mirrored), [SPEC-0007](../vended-endpoints/spec.md)
(vend-time ceilings), [SPEC-0020](../event-routing/spec.md) (rules, params, work orders) and
[SPEC-0034](../todo-attempts/spec.md) (attempt history). ADR-0012, ADR-0022, ADR-0024 and ADR-0025 set
the semantics mirrored here and are cited in design.md.

## Requirements

### Requirement: Human API Surface

The routes below MUST be mounted under `/api/v1` behind the existing human OAuth guard
(`oauthGuard`), and MUST NOT be exposed as MCP verbs by this spec. Every route MUST accept and return
`application/json`. An endpoint credential (`sbk_…`) MUST be refused with `401`, as it is today.

| Method | Path | Mirrors | Auth |
| --- | --- | --- | --- |
| GET | `/api/v1/endpoints` | `get_default_lease`, across reach (`default_lease_ttl_seconds`, `effective_lease_ttl_seconds`) | Required |
| PATCH | `/api/v1/endpoints/{ref}` | `set_default_lease` ([ADR-0043](../../../adrs/ADR-0043-per-endpoint-default-claim-lease.md)) | Required |
| GET | `/api/v1/webhooks` | `list_webhooks`, across reach | Required |
| GET | `/api/v1/endpoints/{ref}/webhooks` | `list_webhooks` | Required |
| POST | `/api/v1/endpoints/{ref}/webhooks` | `create_webhook` | Required |
| POST | `/api/v1/webhooks/{webhook_id}/rotate` | `rotate_webhook` | Required |
| DELETE | `/api/v1/webhooks/{webhook_id}` | `delete_webhook` | Required |
| GET | `/api/v1/webhooks/{webhook_id}/rules` | `list_webhook_rules` | Required |
| PUT | `/api/v1/webhooks/{webhook_id}/rules` | `set_webhook_rules` | Required |
| POST | `/api/v1/webhooks/{webhook_id}/rules` | `add_webhook_rule` | Required |
| PATCH | `/api/v1/webhooks/{webhook_id}/rules/{rule_id}` | `update_webhook_rule` | Required |
| POST | `/api/v1/webhooks/{webhook_id}/rules/{rule_id}/move` | `move_webhook_rule` | Required |
| DELETE | `/api/v1/webhooks/{webhook_id}/rules/{rule_id}` | `remove_webhook_rule` | Required |
| POST | `/api/v1/webhooks/{webhook_id}/rules/test` | `test_webhook_rules` | Required |
| GET | `/api/v1/webhooks/{webhook_id}/routes` | `list_webhook_routes` | Required |
| PUT | `/api/v1/webhooks/{webhook_id}/routes/{endpoint_id}` | `add_webhook_route` | Required |
| POST | `/api/v1/webhooks/{webhook_id}/routes` | `add_webhook_route` (`{"target_endpoint_id"}` body) | Required |
| DELETE | `/api/v1/webhooks/{webhook_id}/routes/{endpoint_id}` | `remove_webhook_route` | Required |
| GET | `/api/v1/events` | `list_webhook_events`, `recent_webhook_events` | Required |
| GET | `/api/v1/events/{event_id}` | `get_webhook_event` | Required |
| POST | `/api/v1/events/{event_id}/replay` | `replay_webhook_event` | Required |
| GET | `/api/v1/todos` | `list_todos`, across reach | Required |
| GET | `/api/v1/todos/{todo_id}` | `get_todo` | Required |
| POST | `/api/v1/todos/{todo_id}/retry` | board Retry | Required |
| POST | `/api/v1/todos/{todo_id}/release` | board Release | Required |
| POST | `/api/v1/todos/{todo_id}/complete` | board Complete | Required |
| POST | `/api/v1/todos/{todo_id}/fail` | board Fail | Required |

Errors MUST use one JSON shape, `{"error": <human-readable>, "code": <MCP error code>}`, with extra
fields where this spec names them. MCP error codes MUST map to statuses as follows: `invalid_argument`
→ `400`; `forbidden` and `forbidden_source_type` → `403`; `not_found` and
`rule_not_found` → `404`; `conflict` → `409`; `ceiling_exceeded` → `409`; `rate_limited` → `429` with
`Retry-After`; `unavailable` → `503`; `replay_target_required` → `400`; `replay_failed` → `502`;
`internal` → `500`. A routing validation failure MUST answer `400` with routing's own code, exactly
as the MCP rule verbs surface it (`invalid_expression`, `forbidden_function`, `invalid_rule`,
`too_many_rules`, `invalid_params`), except `not_granted`, which MUST answer `403` with code
`forbidden`. A body that does not decode MUST answer `400` with code `invalid_argument`, and one over
the route's size cap `413`. Any other failure is a `500` with code `internal`. The existing mapping of
`store.ErrNotFound` to `400` in `apiHandler.fail` MUST NOT be used by the new routes.

#### Scenario: Endpoint credential on a management route

- **WHEN** a request to `GET /api/v1/webhooks` carries an `sbk_` endpoint credential
- **THEN** the response is `401` with `WWW-Authenticate: Bearer realm="switchboard-api"`, and no
  store query runs

#### Scenario: Error shape

- **WHEN** `PATCH /api/v1/webhooks/{id}/rules/nope` names a rule that does not exist
- **THEN** the response is `404` with body `{"error": "...", "code": "rule_not_found"}`

### Requirement: Reach on Every Route

Every route MUST resolve the calling human's reach on each request, as SPEC-0033 REQ "Reach and
Effective Reach" defines it, and MUST apply it inside the store query rather than by filtering rows
afterwards. Until teams ship, reach is the human's own resources.

A webhook, rule, route, event, todo or endpoint outside the caller's reach MUST produce the same
`404` status, body and timing class as an id that does not exist. `{ref}` MUST resolve by slug or id
among endpoints in reach, as `ownedEndpoint` does today.

A write to a resource owned by a **revoked** endpoint MUST answer `409` with
`{"error", "code": "conflict", "state"}`. Reads of a revoked endpoint's webhooks, events and todos
MUST still succeed, so history remains inspectable after a rotation.

Store methods added for these routes MUST take a reach value, MUST NOT carry the `Unscoped` suffix,
and MUST be covered by the store tenancy test that SPEC-0033 defines.

#### Scenario: Probing another human's webhook

- **GIVEN** human A owns webhook `W`
- **WHEN** human B calls `GET /api/v1/webhooks/W/rules`, and then the same route with a random UUID
- **THEN** both responses are `404` with identical bodies

#### Scenario: Deleting a webhook of a revoked endpoint

- **GIVEN** endpoint E is revoked and still owns webhook `W`
- **WHEN** its human calls `DELETE /api/v1/webhooks/W`
- **THEN** the response is `409` with `state: "revoked"`, and `GET /api/v1/endpoints/E/webhooks`
  still lists `W`

### Requirement: Shared Implementation With MCP

Each route MUST call the same validation, authorization and mutation function its MCP verb calls.
Those functions MUST be extracted from `internal/mcp` into a package both surfaces import, and
parameterized by the caller's principal: an endpoint for MCP, a human reach for the API. Neither
surface MAY keep a private copy of a rule limit, a ceiling check, a grant computation, a route-target
check, the save-time dry run, the conflict check or the SSRF guard.

For the same input, a route and its MCP verb MUST produce the same outcome: the same stored state,
the same MCP error code, and the same response fields, apart from the envelope.

#### Scenario: One rule limit, two surfaces

- **WHEN** a 33-rule set is submitted through `PUT /api/v1/webhooks/{id}/rules` and through
  `set_webhook_rules`
- **THEN** both are refused with code `too_many_rules` and the same message, and the stored rules
  are unchanged

#### Scenario: Dry run applies to the API

- **GIVEN** a candidate rule faults on one of the webhook's 50 latest deliveries
- **WHEN** it is added through `POST /api/v1/webhooks/{id}/rules`
- **THEN** the response is `400` naming the rule and event ids, and the previous config stays in force

### Requirement: Webhook Management

`GET /api/v1/endpoints/{ref}/webhooks` MUST return `list_webhooks`' shape for that endpoint, including
its `ceiling`. `GET /api/v1/webhooks` MUST return `{"webhooks": [...]}` listing every webhook in
reach, a revoked endpoint's included, each with its `endpoint_id`, `endpoint_slug` and
`endpoint_state`, and a routing summary (`rule_count`, `has_default_action`, `has_params`). Neither MAY
ever return a signing secret or ingest token after creation.

`POST /api/v1/endpoints/{ref}/webhooks` with `{"source_type", "target_queue"}` MUST enforce that
endpoint's vend-time ceiling exactly as `create_webhook` does: source type in its allowed types,
target queue in its allowed queues, and count within `webhook_max` under the endpoint row lock.
It MUST reveal the signing secret or the ingest URL token once, in the `201` response.

`POST /api/v1/webhooks/{webhook_id}/rotate` MUST rotate the webhook's ingest URL and signing secret
as `rotate_webhook` does, retiring the old ones, and MUST reveal the new signing secret once for a
signed type.

`DELETE /api/v1/webhooks/{webhook_id}` MUST delete a webhook in reach and answer
`{"webhook_id", "deleted": true}`.

#### Scenario: Ceiling reached

- **GIVEN** endpoint E is vended with `webhook_max` 1 and already owns one webhook
- **WHEN** its human posts a second webhook for E
- **THEN** the response is `409` with code `ceiling_exceeded`, and E still owns one webhook

#### Scenario: Secret shown once

- **WHEN** a `github` webhook is created through the API and then listed
- **THEN** the create response carries `signing_secret`, and no list response carries it

#### Scenario: Rotating a leaked secret

- **WHEN** the human rotates webhook `W`
- **THEN** the response carries the new ingest URL and signing secret, and a delivery signed with the
  old secret is refused

### Requirement: Rule Management

The rule routes MUST mirror the rule verbs' inputs and outputs (SPEC-0020), including `default_action`,
`params`, `position`, and the `grant` block in every read and write response. Writes MUST run the full
save path: validation, grant check, dry run against recent deliveries, and the lock-and-compare
conflict check that answers `409 conflict` when the stored config changed since it was read.
`POST …/rules/{rule_id}/move` with `{"position"}` MUST reorder as `move_webhook_rule` does, dry run
included. `DELETE …/rules/{rule_id}` MUST skip the dry run, as `remove_webhook_rule` does, so an owner can
always remove a faulting rule, and MUST succeed when the rule is already absent.

`PUT /api/v1/webhooks/{id}/rules` replaces the whole config. It MUST require the `rules` key (an
empty list removes every rule) and MUST ignore the read-only fields of a `GET` response, so a `GET`
body is a valid `PUT` body. A body that omits the `params` key MUST keep the stored params unchanged.
A body that clears params MUST say so with `"params": null` or `"params": {}`. This is SPEC-0026
REQ-4's rule for `set_webhook_rules`, applied here from the start: omitting `params` must never clear
them silently (ADR-0025 names that trap).

`POST /api/v1/webhooks/{id}/rules/test` MUST accept exactly one of `event_id` or `payload`, plus the
optional candidate `rules`, `default_action`, `params`, `headers` and `omit_envelope`. It MUST save
nothing, and MUST refuse an `event_id` that belongs to another webhook with `404`.

#### Scenario: Replacing rules without params

- **GIVEN** webhook `W`'s stored config carries `params.repo_prefixes`
- **WHEN** its human sends `PUT /api/v1/webhooks/W/rules` with `rules` and no `params` key
- **THEN** the response is `200`, the new rules are stored, and the stored params are unchanged

#### Scenario: Clearing params

- **WHEN** its human sends `PUT /api/v1/webhooks/W/rules` with `"params": null`
- **THEN** the stored params are removed

#### Scenario: Concurrent edit

- **GIVEN** the human reads `W`'s rules, and an agent then changes them through MCP
- **WHEN** the human's `PATCH` for one rule arrives
- **THEN** the save fails with `409 conflict` rather than overwriting the agent's change

#### Scenario: Test against a stored delivery

- **WHEN** the human posts `{"event_id": 812}` to `…/W/rules/test` with a candidate rule set
- **THEN** the response carries the decision and trace for event 812, and `W`'s stored rules are
  unchanged

### Requirement: Work Orders on the Human API

A rule action MAY carry `work_order: true` through the human API, under the same checks the MCP rule
verbs apply: queue-only actions, the queue grant, and the dry run. The `work_order` contents
(`verified`, `authorized_by`, `subject`, `authority`) MUST remain computed by the router at ingest.
No route MAY accept them as input.

Every successful rule write whose resulting config contains at least one `work_order` action, or
whose previous config did, MUST emit one structured log record carrying the human id, webhook id,
surface (`api`), and the ids of the rules whose `work_order` flag was added, removed or kept. Rule
responses MUST mark each rule carrying `work_order` so a client can show it.

#### Scenario: Supplying work-order contents

- **WHEN** an `add` body sets `action.work_order` to an object with `verified: true`
- **THEN** the response is `400` with code `invalid_argument`, and nothing is saved

#### Scenario: Adding a work-order rule is logged

- **WHEN** the human adds a rule `lane-s` with `{"queue": "lane-s", "work_order": true}`
- **THEN** a log record names the human, the webhook, surface `api`, and `lane-s` as added

### Requirement: Route Management

`GET …/routes` MUST return `list_webhook_routes`' shape. `PUT …/routes/{endpoint_id}` MUST authorize
the target exactly as `add_webhook_route` does: an active endpoint in the caller's reach, or one whose
human has a friend edge authorizing delivery. Every refusal MUST be the same `403` with code
`forbidden`, whatever the reason, so the route cannot be used to discover which endpoints exist.
Adding an existing route MUST succeed without change. `DELETE …/routes/{endpoint_id}` MUST succeed
whether or not the route existed.

`POST …/routes` with the body `{"target_endpoint_id": …}` MUST be the same operation as
`PUT …/routes/{target_endpoint_id}`, for a client that holds the id in a body, as
`add_webhook_route`'s arguments do. Both MUST run the same function as `add_webhook_route`, with the
signed-in human as the principal: a webhook is in reach when any of the human's endpoints owns it,
which is wider than the MCP verb's own-endpoint rule (SPEC-0033 F19) and is why this is a human
surface only.

A route on a webhook whose owning endpoint is **revoked** MUST follow REQ "Reach on Every Route" for
an add, which answers `409` with the `state`. `GET` and `DELETE` MUST still succeed: a revoked
endpoint's routes still deliver (SPEC-0001), so withdrawing one is the only way to stop them short
of deleting the webhook, and withdrawing delivery is always safe.

#### Scenario: Routing to a stranger's endpoint

- **WHEN** the human adds a route to an endpoint of a human with no friend edge, and then to a
  random UUID
- **THEN** both responses are `403` with identical bodies

#### Scenario: A rule names an endpoint only after it is routed

- **GIVEN** the human owns webhook `W` and endpoint `E`, and `E` is not a route of `W`
- **WHEN** they save rules on `W` whose action names `"endpoints": ["E"]`
- **THEN** the save is `403 forbidden` and the rules are unchanged
- **WHEN** they then call `PUT …/routes/E` and save the same rules
- **THEN** `GET …/rules` lists `E` in `grant.endpoints`, and the save succeeds

#### Scenario: Removing a route of a revoked endpoint's webhook

- **GIVEN** endpoint E is revoked, owns webhook `W`, and `W` routes to endpoint F
- **WHEN** its human calls `PUT …/routes/G` and then `DELETE …/routes/F`
- **THEN** the add is `409` with `state: "revoked"`, and the remove succeeds and stops delivery to F

### Requirement: Delivery History

`GET /api/v1/events` MUST accept `list_webhook_events`' filters — `provider`, `event_type`,
`disposition`, `since`, `limit` (1–200, default 50) and `cursor` — plus `webhook_id` and `endpoint`
(slug or id), and MUST return its shape with `next_cursor`. `GET /api/v1/events/{event_id}` MUST
return `get_webhook_event`'s shape, including the sanitized headers and payload.

Both MUST satisfy SPEC-0033 REQ "Owner-Scoped History Reads" against the human's reach directly.
They MUST NOT borrow one of the human's endpoints as a stand-in caller.

#### Scenario: Faulted deliveries for one webhook

- **WHEN** the human calls `GET /api/v1/events?webhook_id=W&disposition=faulted&limit=20`
- **THEN** only `W`'s faulted events are returned, newest first, with `next_cursor` if more exist

### Requirement: Event Replay

`POST /api/v1/events/{event_id}/replay` with an optional `{"target_url"}` MUST replay one event in
reach, following SPEC-0033 REQ "Owned Replay Targets". The replaying endpoint is the event's owning
endpoint. Its first owned replay target is the default. An explicit target MUST be in that
endpoint's owned list or pass the shared SSRF validator at call and dial time. No target and no
default MUST answer `400 replay_target_required`.

A replay MUST draw from the same per-endpoint replay rate bucket MCP replays use, so the API cannot
raise an endpoint's replay budget. An event outside reach MUST answer `404` before its payload is
read.

#### Scenario: Replay without a target

- **GIVEN** the event's endpoint owns no replay targets
- **WHEN** the human replays it with an empty body
- **THEN** the response is `400` with code `replay_target_required`, and no request leaves the server

#### Scenario: Shared budget

- **GIVEN** an agent has spent its endpoint's replay burst through MCP
- **WHEN** its human replays one of that endpoint's events through the API
- **THEN** the response is `429` with `Retry-After`

### Requirement: Todos and Board Actions

`GET /api/v1/todos` MUST list todos in reach with filters `queue`, `state`, `endpoint`, `limit`
(1–200, default 50) and `cursor`, in `list_todos`' compact shape plus `endpoint_id`. It MUST NOT
include payloads. `GET /api/v1/todos/{todo_id}` MUST return `get_todo`'s shape, including `payload`,
`routing`, `work_order`, `result` and `attempts` (SPEC-0034), with `attempts_limit` 1–50, default 20.

`POST …/retry`, `…/release`, `…/complete` and `…/fail` MUST call the board's human-owned actions
(`RetryTodoOperatorOwned` and its siblings) and MUST record attempts exactly as the board does.
`complete` and `fail` MUST accept an optional `{"result"}` bounded like the board's.

#### Scenario: Why did an issue not reach a lane

- **WHEN** the human calls `GET /api/v1/todos?queue=lane-s&state=pending`
- **THEN** every pending `lane-s` todo in reach is listed, each with its endpoint

#### Scenario: Retry a dead letter

- **WHEN** the human retries a dead-lettered todo
- **THEN** it returns to `pending` with its attempt history kept (SPEC-0034 REQ-12)

### Requirement: CLI Parity

The `switchboard` binary MUST provide a command for every `/api/v1` route, grouped by noun:

| Command | Route |
| --- | --- |
| `endpoint vend NAME [--lease-ttl DUR]` | `POST /endpoints` |
| `endpoint list` (with a LEASE column) | `GET /endpoints` |
| `endpoint edit REF --lease-ttl DUR\|default` | `PATCH /endpoints/{ref}` |
| `webhook list [--endpoint REF]` | `GET /webhooks`, or `GET /endpoints/{ref}/webhooks` |
| `webhook create REF --source TYPE --queue Q` | `POST /endpoints/{ref}/webhooks` |
| `webhook rotate WEBHOOK` | `POST /webhooks/{id}/rotate` |
| `webhook delete WEBHOOK` | `DELETE /webhooks/{id}` |
| `webhook rules get WEBHOOK` | `GET …/rules` |
| `webhook rules set WEBHOOK --file F` | `PUT …/rules` |
| `webhook rules add WEBHOOK --expr E --queue Q\|--drop [--id ID] [--name N] [--work-order] [--position N]` | `POST …/rules` |
| `webhook rules update WEBHOOK RULE [--expr E] [--queue Q\|--drop] [--name N] [--work-order\|--no-work-order]` | `PATCH …/rules/{rule}` |
| `webhook rules move WEBHOOK RULE POSITION` | `POST …/rules/{rule}/move` |
| `webhook rules remove WEBHOOK RULE` | `DELETE …/rules/{rule}` |
| `webhook rules test WEBHOOK (--event ID\|--payload F) [--file F] [--header H]` | `POST …/rules/test` |
| `webhook route list WEBHOOK` | `GET …/routes` |
| `webhook route add WEBHOOK ENDPOINT` | `PUT …/routes/{endpoint}` |
| `webhook route remove WEBHOOK ENDPOINT` | `DELETE …/routes/{endpoint}` |
| `event list [--webhook W] [--endpoint REF] [--disposition D] [--provider P] [--type T] [--since S] [--limit N]` | `GET /events` |
| `event show ID` | `GET /events/{id}` |
| `event replay ID [--target URL]` | `POST /events/{id}/replay` |
| `todo list [--queue Q] [--state S] [--endpoint REF] [--limit N]` | `GET /todos` |
| `todo show ID [--attempts N]` | `GET /todos/{id}` |
| `todo retry\|release ID` | `POST /todos/{id}/retry\|release` |
| `todo complete\|fail ID [--result F]` | `POST /todos/{id}/complete\|fail` |

Every command MUST take `--json`, which prints the API response body verbatim. Without it, the
command MUST print a human-readable table or summary. Inputs that are JSON documents (`--file`,
`--payload`, `--result`) MUST accept a path, `-` for stdin, or `@path`, as `todo push --payload` does
today. `webhook rules get`, `set`, `add` and `update` MUST mark rules carrying `work_order` in their
text output. A command that gets a `4xx` or `5xx` MUST print the `error` and `code` and exit
non-zero. `webhook rules get --json` MUST print a document `webhook rules set --file` accepts
unchanged. `webhook rules set` MUST send the file as written, MUST refuse before sending a file with
no `rules` key, and MUST say in its text output when the file had no `params` and the stored params
were therefore kept. `webhook rules test --file` MUST treat the file's `params` the same way.
`webhook route add` and `remove` MUST accept ENDPOINT as an endpoint id, or as the slug of one of the
human's own endpoints, resolved through `GET /api/v1/endpoints` before any route is written; a slug
that resolves to nothing MUST fail without a route call. `webhook route list` MUST print the owner
endpoint first, then the routes in grant order, with slugs where the human has them.

#### Scenario: Inspecting a lane's rules from a terminal

- **WHEN** the human runs `switchboard webhook rules get W`
- **THEN** the rules print in evaluation order with their ids, expressions and actions, and the
  `lane-s` rule is marked as carrying a work order

#### Scenario: Machine-readable output

- **WHEN** the human runs `switchboard event list --webhook W --json`
- **THEN** stdout is exactly the API's JSON body

### Requirement: Parity Is Tested

A test MUST enumerate every tool the MCP server registers (a superset of `mcp.AllVerbs()`) and fail
when a tool has neither a mapped `/api/v1` route nor an entry, with a reason, on an explicit
agent-only allowlist. The allowlist starts with exactly the lease verbs, `claim`, `claim_next` and
`heartbeat`: a human does not hold leases through the API. `complete` and `fail` map to the board
actions in REQ "Todos and Board Actions", and `recent_webhook_events` maps to `GET /api/v1/events`.
Verbs added later by other specs, for example `release` (SPEC-0034) or the presence verbs
(SPEC-0022), MUST either gain a route or be allowlisted by that spec. `get_default_lease` maps to
`GET /api/v1/endpoints` and `set_default_lease` to `PATCH /api/v1/endpoints/{ref}` (ADR-0043).

`PATCH /api/v1/endpoints/{ref}` takes `{"default_lease_ttl_seconds": int|null}`. The key is
required, any other key MUST be refused with `invalid_argument` (scope is not editable, SPEC-0007),
and a value from 60 to 86400 or null is the only valid input. It MUST follow REQ "Reach on Every
Route" (409 for a revoked endpoint) and draw from the per-human write bucket.

A second test MUST enumerate the `/api/v1` route table and fail when a route has no CLI command.

#### Scenario: A new MCP verb without an API route

- **WHEN** a developer registers a new MCP verb `archive_webhook` and adds no route and no allowlist
  entry
- **THEN** the parity test fails naming `archive_webhook`

### Requirement: Error Handling Standards

All error-producing operations MUST follow structured error handling:

- Errors MUST be wrapped with contextual information at each layer boundary.
- The extracted shared package MUST define sentinel errors for the domain failures callers must
  distinguish, and both surfaces MUST map them to their own envelope, never by matching message text.
- Silent error swallowing MUST NOT occur. Every error MUST be returned, logged with context, or handled
  with a documented reason.
- Structured logging MUST be used for error reporting (key-value pairs, not string interpolation).

#### Scenario: Sandbox outage during a save

- **WHEN** the rule sandbox is unavailable while the human saves rules
- **THEN** the response is `503` with code `unavailable`, the previous config stays in force, and one
  structured log record names the webhook and the cause

### Requirement: Database Operation Standards

All database operations MUST follow structured data access patterns:

- Transactions MUST be used for multi-step mutations that require atomicity. Webhook creation keeps
  its endpoint row lock, and rule saves keep their row lock and compare.
- Connection lifecycle MUST be explicitly managed. Connections MUST be returned to the pool after use,
  with timeouts configured, and slow resolution MUST happen before a lock is taken, as `mutateRules`
  does today.
- Queries MUST use parameterized statements. String interpolation in queries MUST NOT occur.

#### Scenario: Ceiling race

- **WHEN** two requests create a webhook for the same endpoint at `webhook_max - 1`
- **THEN** exactly one succeeds, and the other answers `409 ceiling_exceeded`

## Security Requirements

<!-- Governing: ADR-0018 (Security-by-Default), SPEC-0016 REQ "Mandatory Security Section in Web Specs" -->

### Authentication

Every route in this spec requires a human OAuth bearer (`resource=<base>/api`) resolved by
`oauthGuard`. There are no public routes. Endpoint credentials and session cookies MUST NOT
authenticate any `/api/v1` route.

| Endpoint | Auth | Justification |
| --- | --- | --- |
| Every route in REQ "Human API Surface" | Required | — |

### Rate Limiting

`/api/v1` has no limiter today. This spec MUST add a per-human token bucket to every `/api/v1`
route: 20 requests per second, burst 40, for reads, and 5 per second, burst 20, for writes. Over the
limit answers `429` with `Retry-After`. Replays MUST additionally draw from the owning endpoint's
existing replay bucket (REQ "Event Replay"), and rule saves remain bounded by the sandbox's per-tenant
share (SPEC-0033 F5).

### Security Headers

No new header policy is introduced. Every response carries the global `secureHeaders` set
(`Content-Security-Policy`, `X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff`,
`Referrer-Policy: strict-origin-when-cross-origin`).

### Request Body Size Limits

The existing 64 KiB `/api/v1` cap applies. `PUT …/rules`, `POST …/rules` and `POST …/rules/test` MAY
raise it to 256 KiB, because 32 rules of 4 KiB each plus 16 KiB of params exceed 64 KiB. Over the cap
answers `413`.

### CSRF Protection

Every route authenticates only with an `Authorization: Bearer` header that a browser never attaches
on its own, and MUST NOT accept a cookie. Cross-site requests therefore cannot act as the human, and
no CSRF token is required.

### Redirect Validation

No route redirects. Replay MUST NOT follow redirects, MUST NOT use a proxy, and MUST re-validate
every dialed address through the shared SSRF guard (ADR-0029, SPEC-0033 REQ "Owned Replay Targets").
