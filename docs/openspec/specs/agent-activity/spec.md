---
status: draft
date: 2026-09-27
implements: [ADR-0042]
extends: [SPEC-0034, SPEC-0006, SPEC-0013]
requires: [SPEC-0004, SPEC-0023, SPEC-0016]
related: [SPEC-0033, SPEC-0012]
---

# SPEC-0036: Agent Activity and the Todo Audit Trail

## Overview

Switchboard collects what agents do while they work a todo and shows the owning human a complete
history of it. There are three inputs:

* an **OTLP/HTTP trace receiver** that binds spans to a todo and its attempt;
* an optional **`note` on `heartbeat`**;
* a **`todo_activity` log** for transitions that attempts and events do not already record, such as
  human actions, rings and A2A states.

There are two surfaces:

* a full **todo page**, with an outcome card, a merged timeline, a per-attempt trace waterfall and
  the source event;
* an **Activity feed** across the human's todos, with a catch-up marker.

See [ADR-0042](../../../adrs/ADR-0042-agent-activity-and-todo-audit-trail.md).

This spec extends [SPEC-0034](../todo-attempts/spec.md): claim responses gain `traceparent`,
attempts gain `trace_id` and drop counters, and `heartbeat` gains `note`. It also extends
[SPEC-0006](../agent-tools/spec.md) (the drain verbs) and [SPEC-0013](../operator-board/spec.md),
where the todo detail becomes a page and the drawer links to it. It requires
[SPEC-0004](../persistence/spec.md) for migrations and retention,
[SPEC-0023](../metrics/spec.md) for metrics and [SPEC-0016](../mcp-oauth/spec.md) for OAuth
bearer resolution.

Terms:

* **Trail**: every recorded fact about one todo. That covers its source event, its attempts, its
  activity entries and its spans.
* **Owner scope**: as in SPEC-0034. Today it is the todo's `endpoint_id` for agents and the
  owning human for the Board. SPEC-0033 widens both to teams.
* **Bound span**: a span Switchboard has assigned to exactly one todo and, when one can be
  determined, one attempt of it.

## Requirements

### REQ-1: OTLP Receiver Route and Authentication

Switchboard SHALL serve `POST /otlp/{endpoint}/v1/traces`. `{endpoint}` is the endpoint's slug or
id, resolved exactly as the MCP route resolves it. The route:

* SHALL authenticate only with `Authorization: Bearer <credential>`. The credential is either an
  `sbk_` endpoint credential or an OAuth access token, resolved through the same functions MCP
  uses.
* SHALL answer 403 when the credential's endpoint does not match `{endpoint}`.
* SHALL NOT read the session cookie. A request that carries a cookie and no bearer gets 401.
* SHALL require the endpoint's verb scope to include `heartbeat`. Otherwise it answers 403.
* SHALL NOT be exposed as an MCP tool, and SHALL NOT issue redirects.
* SHALL be served only when `SWITCHBOARD_OTLP` is enabled (REQ-21). When it is disabled, the route
  answers 404.

#### Scenario: Stock exporter configuration

- **WHEN** an exporter is configured with `OTEL_EXPORTER_OTLP_ENDPOINT=$SWITCHBOARD_URL/otlp/my-bot`
  and `OTEL_EXPORTER_OTLP_HEADERS=Authorization=Bearer sbk_…` for endpoint `my-bot`
- **THEN** its `POST $SWITCHBOARD_URL/otlp/my-bot/v1/traces` is authenticated as `my-bot`

#### Scenario: Credential for another endpoint

- **WHEN** endpoint A's credential posts to `/otlp/B/v1/traces`
- **THEN** the response is 403 and nothing is stored

### REQ-2: Encodings, Compression and Size Limits

The receiver SHALL accept `Content-Type: application/x-protobuf` and `application/json` (OTLP/JSON,
with hex-encoded trace and span ids). It SHALL answer in the request's encoding. Any other content
type SHALL get 415.

It SHALL accept `Content-Encoding: gzip`. The request-size cap SHALL be enforced on both the
compressed stream and the decompressed stream, before the body is fully buffered. The cap is
`otlp_max_request_bytes`, default 4 MiB. A body over the cap SHALL get 413.

A request with more than `otlp_max_spans_per_request` spans (default 1000) SHALL get 400, and
nothing from it is stored. An undecodable body SHALL get 400 with the message `otlp-decode-failed`.
Protobuf and JSON inputs that encode the same data SHALL produce identical stored rows.

#### Scenario: Gzip bomb

- **WHEN** a 100 KiB gzip body inflates past 4 MiB
- **THEN** decompression stops at the cap, the response is 413, and nothing is stored

#### Scenario: Encoding parity

- **WHEN** the same three spans are exported once as protobuf and once as OTLP/JSON, to two fresh
  todos
- **THEN** both todos hold identical span rows, apart from `todo_id`

### REQ-3: Binding Spans to Todos and Attempts

For each span, Switchboard SHALL determine the owning todo from the first of these rules that
applies:

1. **Minted trace id.** The span's trace id equals the `trace_id` of an attempt (REQ-4) of a todo
   in the endpoint's owner scope. The span binds to that todo and that attempt.
2. **Attribute.** The span, or its resource, carries `switchboard.todo.id` naming a todo in the
   endpoint's owner scope. The span attribute wins over the resource attribute. The attempt is
   chosen as follows:
   * with `switchboard.attempt.seq`, the attempt with that `seq`;
   * otherwise, the attempt whose `[claimed_at, ended_at]` interval contains the span's start;
   * otherwise, the todo's latest attempt;
   * otherwise, none. The span still binds to the todo.
3. **Earlier binding.** An earlier span of the same trace id was bound by rule 2 for this endpoint.
   The span binds to that todo, and its attempt is chosen as in rule 2.

A span that matches none of these SHALL NOT be stored. It SHALL be counted in the response's
`partial_success.rejected_spans`. A `switchboard.todo.id` naming a todo that does not exist and one
naming a todo outside the scope SHALL produce identical responses. A mixed batch SHALL store every
bound span and SHALL still answer 200.

#### Scenario: Supervisor passes the traceparent

- **WHEN** Harness claims todo T, receives `traceparent` 00-`abc…`-…, and its child process exports
  spans with trace id `abc…`
- **THEN** every span binds to T's current attempt, with no attribute needed

#### Scenario: Foreign todo attribute

- **WHEN** endpoint A exports a span with `switchboard.todo.id` set to endpoint B's todo
- **THEN** the span is rejected with the same `partial_success` message as a random id, and B's
  trail is unchanged

### REQ-4: Trace Context on Claim Responses

Every committed claim (SPEC-0034 REQ-2) SHALL mint a 16-byte random trace id and store it as the
attempt's `trace_id`. `claim` and `claim_next` responses SHALL carry `traceparent`, a W3C Trace
Context value (`00-<trace_id>-<span_id>-01`). Its parent span id is random and is not stored.
`get_todo` SHALL return each attempt's `trace_id`. The field is additive, and clients that ignore
it are unaffected.

#### Scenario: Each attempt has its own trace

- **WHEN** a todo is claimed, fails, and is claimed again
- **THEN** the two claim responses carry different trace ids, and each attempt stores its own

### REQ-5: Span Translation and Stored Fields

Each bound span SHALL be stored with these fields:

* `todo_id` and `attempt_seq` (the attempt seq is nullable);
* `trace_id`, `span_id` and `parent_span_id`;
* `name`, at most 256 bytes;
* `kind`;
* `status_code`, and `status_message` of at most 512 bytes;
* `start_at` and `end_at`;
* `service_name`, at most 128 bytes;
* `tool`, from `gen_ai.tool.name`, at most 128 bytes;
* `category`;
* `attributes`, bounded jsonb (REQ-6);
* `received_at`.

`category` SHALL be the first of these that applies:

1. the `switchboard.category` attribute, at most 64 bytes;
2. `gen_ai.operation.name`: `chat`, `text_completion` and `generate_content` give `reason`, and
   `execute_tool` gives `tool`;
3. a present `gen_ai.tool.name` gives `tool`;
4. an `http.request.method` or `rpc.system` attribute gives `net`;
5. a `db.system` attribute gives `read`;
6. the lowercased SpanKind.

A span whose end precedes its start SHALL be rejected (counted in `rejected_spans`). Span events,
links and trace state SHALL be dropped. So SHALL all resource attributes except `service.name` and
the binding attributes.

#### Scenario: A tool span

- **WHEN** a span has `gen_ai.operation.name = execute_tool` and `gen_ai.tool.name = bash`
- **THEN** it is stored with `category = tool` and `tool = bash`

### REQ-6: Attribute Allow-List and Tool Input/Output Opt-In

`attributes` SHALL keep only allow-listed keys:

* `gen_ai.system`, `gen_ai.request.model`, `gen_ai.response.model`, and `gen_ai.usage.*`;
* `gen_ai.operation.name`, `gen_ai.tool.call.id`, `code.function.name` and `code.file.path`;
* `process.exit.code`, `error.type`, and `http.response.status_code`;
* every `switchboard.*` key.

Tool input and output are a separate group: `gen_ai.tool.call.arguments`, `gen_ai.tool.call.result`,
`gen_ai.input.messages`, `gen_ai.output.messages` and `process.command_args`. Keys in that group
SHALL be kept only when `SWITCHBOARD_OTLP_KEEP_TOOL_IO` is enabled.

The following caps SHALL apply:

* A kept value SHALL be truncated to 1024 bytes on a UTF-8 boundary. When tool I/O is enabled, a
  tool I/O value is truncated to 4096 bytes instead.
* A span keeps at most 32 attributes and at most 16 KiB of attributes in total.
* A truncation SHALL be recorded as `switchboard.truncated: true` on the span.

#### Scenario: Default drops tool arguments

- **WHEN** a span carries `gen_ai.tool.call.arguments` and tool I/O is not enabled
- **THEN** the stored span has no `gen_ai.tool.call.arguments` key

### REQ-7: Credential Masking

Before storage, Switchboard SHALL replace every match of its credential pattern set with
`[REDACTED]`. The pass covers kept attribute values, span names, status messages and notes (REQ-9).
The pattern set SHALL cover at least:

* Switchboard's own `sbk_` credentials and `Bearer <token>` values;
* GitHub tokens: `ghp_`, `gho_`, `ghu_`, `ghs_`, `ghr_` and `github_pat_`;
* Slack tokens: `xoxa-`, `xoxb-` and `xoxp-`;
* keys shaped like `sk-` and `sk-ant-`;
* AWS access key ids: `AKIA` / `ASIA` followed by 16 characters.

The masker SHALL be a pure function with table tests. It SHALL NOT allocate unboundedly on
adversarial input: it runs over already-capped values and its regexes are linear-time.

#### Scenario: Token in a kept value

- **WHEN** tool I/O is enabled and an argument contains `ghp_` followed by 36 characters
- **THEN** the stored value contains `[REDACTED]` in its place

### REQ-8: Idempotency and the Export Response

A span SHALL be upserted on `(todo_id, trace_id, span_id)`. An exporter's retry of a batch that was
already stored therefore changes nothing and is not counted as rejected.

A successful response SHALL be an `ExportTraceServiceResponse`. When any span is rejected, it SHALL
carry `partial_success` with the `rejected_spans` count and a fixed, non-identifying
`error_message` that lists the rejection reasons: `unbound`, `cap`, `invalid`. Error responses
(4xx or 5xx) SHALL carry a `google.rpc.Status` in the request's encoding. A 429 SHALL carry
`Retry-After`.

#### Scenario: Retry is harmless

- **WHEN** the same batch of 10 bound spans is posted twice
- **THEN** 10 rows exist, and both responses have `rejected_spans = 0`

### REQ-9: Heartbeat Notes

`heartbeat` SHALL accept an optional `note` string. A non-empty note SHALL be handled as follows:

* It is truncated to 512 bytes on a UTF-8 boundary, with truncation marked, and masked (REQ-7).
* It is stored as a `todo_activity` entry of kind `note` on the open attempt, in the same
  transaction as the heartbeat.
* On a fenced attempt it requires the matching `lease_token`, exactly as the heartbeat does
  (SPEC-0034 REQ-6). A mismatch is `conflict`, and no note is stored.
* When the attempt already holds `notes_max_per_attempt` notes (default 200), the heartbeat
  succeeds, the note is not stored, and the attempt's `notes_dropped` counter is incremented.

#### Scenario: A progress note

- **WHEN** an agent calls `heartbeat(todo_id, note: "tests passing, opening PR")`
- **THEN** the lease is extended, and the todo's trail shows the note under the current attempt with
  the endpoint as actor

### REQ-10: The Todo Activity Log

Switchboard SHALL keep an append-only `todo_activity` table. Each row has:

* `todo_id`, and `attempt_seq` (nullable);
* `seq`, increasing per todo;
* `at`;
* `kind`;
* `actor_kind`: `endpoint`, `human` or `system`;
* `actor_ref`: an endpoint id, a human id, or a system component name;
* `message`, at most 512 bytes;
* `detail`: jsonb, at most 4 KiB.

It SHALL record these kinds:

| Kind | Written by | Transactional |
| --- | --- | --- |
| `note` | `heartbeat` with a note (REQ-9) | Yes |
| `human_action` | Board claim, complete, fail, retry, extend, release; `detail.action` names it | Yes |
| `retried` | A manual retry that resets the attempt counter, with the prior state | Yes |
| `a2a_state` | A2A cancel, reject, input-required, auth-required, resume | Yes |
| `rung` | A doorbell ring, with transport and ring count | No, best effort |

Rows carry no owner-scope column of their own. They are reachable only through their todo.
Attempts (SPEC-0034) and events remain the authoritative record of claims, attempt endings and
intake. `todo_activity` SHALL NOT duplicate them.

#### Scenario: A human retries a dead letter

- **WHEN** the owner clicks Retry on a dead-lettered todo
- **THEN** a `retried` entry with `actor_kind = human` and the human's id is written in the same
  transaction as the state change

### REQ-11: The Todo Page

`GET /todos/{id}` SHALL render a full page when requested without `HX-Request`. With `HX-Request`
it keeps returning the drawer fragment. The page SHALL show the following.

* **A header:** title, state chip, queue, source kind, owner label, attempt `N/max`, and the next
  retry countdown or dead-letter callout.
* **An outcome card:** the most recent closed attempt's outcome, summary and artifact, the died
  marker where it applies (SPEC-0034 REQ-13), and `result` rendered as escaped, pretty-printed JSON.
  A todo with no closed attempt shows "No attempt has finished yet."
* **The timeline** (REQ-12).
* **The trace waterfall** for each attempt with spans (REQ-13).
* **The source event panel** (REQ-14).
* **The existing state-dependent actions**, with their existing CSRF protection.

The drawer SHALL gain a "Full history" link to the page.

#### Scenario: Deep link

- **WHEN** the owner opens `/todos/T` in a new tab
- **THEN** the full page renders with the outcome card first and the timeline below

### REQ-12: The Merged Timeline

The page SHALL render one timeline of the todo's trail, oldest first, merging:

* **Intake:** the event received, verified (trust mode and verify detail) and routed (the matching
  rule's id and name from `routing_trace`, the target queue, and "work order" when `work_order` is
  set).
* **Creation:** the todo created.
* **Activity:** each `todo_activity` entry.
* **Attempts:** each attempt, as a group containing its claim (claimant, claimer kind, time), its
  notes in time order, its span count with a link to its waterfall, and its end (outcome,
  disposition, summary, artifact, died marker with last heartbeat).
* **Drops:** the pruned-attempt notice (`attempts_pruned`) and the dropped-span and dropped-note
  notices, when non-zero.

Each entry SHALL show an absolute UTC timestamp in a `<time datetime>` element and a relative time.
The timeline SHALL render, oldest first, up to `timeline_max_entries` entries (default 500),
followed by a notice that names how many older entries were not shown.

#### Scenario: A relay's story

- **WHEN** a todo was routed by rule `lane-s`, claimed, noted twice, reaped, claimed again and
  completed
- **THEN** the timeline shows, in order: received, routed by `lane-s`, created, attempt 1 (claim,
  two notes, reaped with the died marker), attempt 2 (claim, completed with its summary)

### REQ-13: The Trace Waterfall

For each attempt with bound spans, the page SHALL render a waterfall. Spans with no attempt are
grouped under "Unattributed spans".

* **Rows:** one per span, in tree order, indented by depth. Each shows the name, category, tool,
  duration, and an error marker when `status_code` is error.
* **Bars:** each bar is positioned and sized by CSS custom properties (`--sb-at`, `--sb-len`) that
  the server computes as percentages of the attempt's span window. They are set in an inline
  `style` attribute, which the existing CSP (`style-src 'self' 'unsafe-inline'`) allows. There is
  no inline script.
* **Details:** an expandable `<details>` element per span lists its kept attributes as escaped
  text.
* **Fallback:** with more than `waterfall_max_spans` spans (default 500), the waterfall renders the
  first 500 in start order and a notice.

The waterfall SHALL be fully readable with JavaScript disabled.

#### Scenario: Markup in a span name

- **WHEN** a span is named `<img src=x onerror=alert(1)>`
- **THEN** the waterfall shows the literal text, and nothing executes

### REQ-14: The Source Event Panel

The page SHALL show the todo's source event, when it has one:

* source, family and type;
* trust mode, `verified` and `verify_detail`;
* received time and disposition;
* the sanitized headers already stored;
* the payload, pretty-printed when it is JSON, and otherwise escaped text capped at 64 KiB with a
  notice;
* `routing_trace`, as a compact list of the rules evaluated and the one that matched;
* `work_order`.

It SHALL NOT show `source_ip`. The event read SHALL be scoped to the owning human through the todo
(REQ-18). A todo created by an operator or over A2A, with no event, shows "Created by …" instead.

#### Scenario: Routing explained

- **WHEN** the owner opens a todo that rule `handoff-lane-m` routed
- **THEN** the panel names `handoff-lane-m` as the matching rule and shows the work order

### REQ-15: The Activity Feed and Catch-Up

`GET /activity` SHALL render a reverse-chronological feed across the signed-in human's todos. It
SHALL contain:

* attempt endings, with outcome, summary and the died marker;
* notes;
* `human_action`, `retried` and `a2a_state` entries;
* dead letters.

Every entry SHALL link to its todo page. The feed SHALL page by keyset (`before=<cursor>`), 50
entries per page, and filter by queue, endpoint and outcome.

A per-human `activity_seen_at` SHALL be advanced when the first page is viewed unfiltered. Entries
newer than the previous value SHALL be marked "new", and the rail's Activity item SHALL show the
count of new entries, capped at "99+". The rail SHALL gain an "Activity" item between Board and
Todos.

#### Scenario: Catching up

- **WHEN** a human returns after three attempts ended overnight
- **THEN** the rail shows "Activity 3", and the feed marks those three as new with their summaries

### REQ-16: Live Updates

While a todo page is open, new timeline entries and new spans SHALL arrive over the existing SSE
stream, scoped to the owning human. That covers attempt claimed or ended, notes, activity, and a
span batch. They SHALL be appended through out-of-band swaps that follow the OOB wrapper contract
(`internal/web/oob_contract_test.go`, #286), into an `aria-live="polite"` region. Span batches SHALL
be coalesced to at most one frame per todo per second.

#### Scenario: Watching a live attempt

- **WHEN** the owner has `/todos/T` open and the agent heartbeats with a note
- **THEN** the note appears under the open attempt without a reload

### REQ-17: A2UI Todo Detail

When A2UI is enabled, `switchboard://todo/{id}/a2ui` SHALL include the outcome card and a compact
timeline: at most 50 entries, spans summarised as a count per attempt. It SHALL NOT include the
waterfall or the event payload.

### REQ-18: Tenant Isolation

Every read of a trail SHALL be filtered by the todo's owner scope. The web uses the owning-human
predicate. The agent path (`get_todo`) uses `endpoint_id`. `todo_spans` and `todo_activity` SHALL
be reachable only through their todo.

A foreign todo id and an unknown one SHALL produce byte-identical responses. That holds on
`/todos/{id}`, on its fragments, on SSE subscriptions and on the OTLP receiver (REQ-3). The
Activity feed SHALL contain only the human's own todos. The instance operator SHALL see ingest
aggregates only (REQ-20), never another user's spans, notes or summaries.

#### Scenario: Second human

- **WHEN** human H2 requests `/todos/T`, where T is H1's
- **THEN** H2 gets the same 404 body as for a random id, and no trail data appears

### REQ-19: Bounds and Retention

These caps SHALL be `settings` keys:

| Key | Default |
| --- | --- |
| `otlp_max_request_bytes` | 4 MiB |
| `otlp_max_spans_per_request` | 1000 |
| `spans_max_per_attempt` | 2000 |
| `spans_max_per_todo` | 10000 |
| `notes_max_per_attempt` | 200 |
| `activity_max_per_todo` | 1000 |
| `timeline_max_entries` | 500 |
| `waterfall_max_spans` | 500 |

A span beyond a cap SHALL be rejected (`cap`) and counted: `spans_dropped` on the attempt, or on
the todo for unattributed spans. An activity row beyond its cap SHALL delete the oldest `note`
rows first. Transactional kinds are never pruned below the cap. `todo_spans` and `todo_activity`
SHALL be deleted with their todo (`ON DELETE CASCADE`). A live todo's trail SHALL NOT be pruned by
retention.

#### Scenario: Span cap

- **WHEN** an attempt already has 2000 spans and 10 more arrive
- **THEN** the 10 are rejected with reason `cap`, and `spans_dropped` becomes 10

### REQ-20: Metrics

Switchboard SHALL export:

* `switchboard_otlp_spans_total{result="accepted|rejected_unbound|rejected_cap|rejected_invalid"}`
* `switchboard_otlp_requests_total{code}`
* `switchboard_todo_activity_total{kind}`

None of these SHALL carry a todo, endpoint or human label.

### REQ-21: Configuration

These settings SHALL exist:

* `SWITCHBOARD_OTLP`: `1` enables the receiver. It is off by default until the feature ships as
  default-on in a release note.
* `SWITCHBOARD_OTLP_KEEP_TOOL_IO`: `true` enables the tool I/O group (REQ-6). It is off by
  default.

Both SHALL be documented in the configuration reference. The Activity feed, the todo page and
heartbeat notes are not flag-gated.

### REQ-22: Agent Setup Documentation

The docs SHALL include a guide that covers:

* configuring a stock OTLP exporter (endpoint, header, protocol);
* passing the claim's `traceparent` to a child process (`TRACEPARENT`) or setting
  `switchboard.todo.id`;
* heartbeat notes;
* what is and is not stored by default;
* the plain statement that notes, summaries and kept attributes are shown to everyone who can read
  the todo, and that producers must redact.

### REQ-23: Database Operation Standards

Every schema change SHALL be a numbered migration under `internal/db/migrations`. Transactional
activity writes SHALL be CTE arms or statements in the same transaction as the transition they
record. Span upserts for one request SHALL run in one transaction per todo, so a failure leaves no
partial batch for that todo. Store tests SHALL run against Postgres (`SWITCHBOARD_TEST_DATABASE_URL`).

#### Scenario: Rollback leaves no orphan

- **WHEN** the transaction of a Board retry fails after its activity insert
- **THEN** neither the state change nor the `retried` entry is visible

### REQ-24: Error Handling Standards

The receiver SHALL map errors to these statuses:

| Status | Cause |
| --- | --- |
| 400 | Decode or validation failure |
| 401 | No or invalid bearer |
| 403 | Slug mismatch or missing grant |
| 404 | Disabled |
| 413 | Too large |
| 415 | Content type |
| 429 | Rate limited |
| 500 | Store failure; the message is generic and the request id is logged |

Logs SHALL carry the request id, the span count and the encoding. They SHALL never carry a
credential, an attribute value or a note.

## Security Requirements

This spec adds one unauthenticated-reachable HTTP route (the receiver, which authenticates every
request) and two human pages.

### Authentication

| Surface | Auth | Description |
| --- | --- | --- |
| `POST /otlp/{endpoint}/v1/traces` | Required | Endpoint bearer (`sbk_` or OAuth). Slug must match. Scope must include `heartbeat`. Cookies are ignored. |
| MCP `heartbeat` (`note`) | Required | Unchanged scope |
| MCP `claim`, `claim_next` (`traceparent`) | Required | Unchanged scopes |
| `GET /todos/{id}` page | Required | Human session; owning-human predicate |
| `GET /activity` | Required | Human session; the human's own todos only |

### Rate Limiting

The receiver SHALL have its own per-endpoint rate limiter (`newRateLimiter`), defaulting to 20
requests per second with a burst of 40. Over the limit it answers 429 with `Retry-After`. A
pre-auth limiter per source IP SHALL run before bearer resolution, as MCP does.

### Security Headers

The new pages SHALL keep the existing CSP (`script-src 'self'`) and security headers
(SPEC-0012). The receiver's responses SHALL carry `X-Content-Type-Options: nosniff` and
`Cache-Control: no-store`.

### Request Body Size Limits

The receiver's limit is REQ-2's `otlp_max_request_bytes`, enforced before and after decompression.
The human pages are GET-only and keep the existing 1 MiB human-group cap.

### Input Handling

* The protobuf and JSON decode path SHALL have a Go fuzz test, run for a bounded time in CI.
* Every stored string is length-capped (REQ-5, REQ-6, REQ-9, REQ-10).
* No URL found in a span, note or event SHALL be fetched.

### CSRF Protection

The todo page and the feed are read-only GETs. The existing Board actions on the page keep their
CSRF tokens. Advancing `activity_seen_at` on a GET is an idempotent, self-scoped write, and needs
no token.

### Redirect Validation

No redirects are introduced. An artifact is a link only when it is an `https` URL
(`rel="noopener noreferrer"`), as in SPEC-0034 REQ-13.

## Accessibility Requirements

These apply to the todo page, the timeline, the waterfall and the Activity feed, per WCAG 2.1 AA.

- **WCAG 2.1 AA Compliance.** All new surfaces SHALL meet WCAG 2.1 Level AA in both themes.
- **ARIA Landmarks.**
  - The todo page SHALL have one `<main>` landmark.
  - The outcome card, timeline, traces and source event SHALL each be a `<section>` with a visible
    `<h2>`.
  - The timeline SHALL be an ordered list.
- **Colour.** Categories, outcomes and the died and error markers SHALL each carry a text label or
  an `aria-label`, and SHALL NOT rely on colour alone.
- **The waterfall as data.** The waterfall SHALL be a `<table>` with a caption and column headers,
  so screen readers get name, category, start offset and duration. The bars are decorative
  (`aria-hidden`).
- **Dynamic Content Regions.** Live-appended timeline entries SHALL land in an
  `aria-live="polite"` region.
- **Keyboard Navigation.** Span `<details>`, feed filters, paging links and artifact links SHALL be
  in tab order with visible focus. The feed SHALL respect the global keyboard map (SPEC-0013).
- **Time.** Relative times SHALL be accompanied by an absolute `<time datetime>` value.
