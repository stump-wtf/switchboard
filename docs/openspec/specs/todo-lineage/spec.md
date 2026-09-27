---
status: draft
date: 2026-09-27
implements: [ADR-0043]
extends: [SPEC-0036, SPEC-0034, SPEC-0020, SPEC-0006]
requires: [SPEC-0004, SPEC-0013]
related: [SPEC-0033, SPEC-0012]
---

# SPEC-0037: Todo Lineage, Subject Threads and Tags

## Overview

This spec adds three things to todos:

* **Subject threads.** Every todo gets a canonical `subject_key` naming what it is about. For
  example, `gitea:o/r#57` for a pull request and every event on it.
* **Lineage links.** These record, with provenance, which todo's work *produced* a subject or
  *spawned* another todo.
* **Tags.** Free metadata, using Cairn's grammar.

It also adds two surfaces: a `/graph` view, and a lineage section on the SPEC-0036 todo page. See
[ADR-0043](../../../adrs/ADR-0043-todo-lineage-subject-threads-and-tags.md). The design responds to
stump-wtf/switchboard#28 and stump-wtf/cairn#3.

Terms:

* **Thread key**: the canonical key REQ-1 derives. **Thread**: the todos in one owner scope that
  share a thread key.
* **Link**: a `todo_links` row. Its **kind** is `produced` or `spawned`, and its **provenance** is
  `declared`, `api`, `tagged` or `trace`.
* **Owner scope**: as in SPEC-0034 and SPEC-0036.

## Requirements

### REQ-1: Thread Key Projection

`internal/routing` SHALL provide `ThreadKeyOf(source string, headers map[string]string, body []byte)
string`. It is a pure function over the verified delivery. It SHALL return `""` when it cannot
determine a key. It SHALL derive keys as follows:

| Provider event | Key |
| --- | --- |
| Gitea/GitHub `issues`, `issue_comment`, `pull_request`, `pull_request_review`, `pull_request_review_comment` (Gitea also: `pull_request_comment`, `pull_request_rejected`, `pull_request_approved`, `pull_request_sync`) | `<provider>:<owner>/<repo>#<number>` |
| `push`, `create`, `delete` with a branch or tag ref; `workflow_run`, `workflow_job`, `check_run`, `check_suite` (the head branch) | `<provider>:<owner>/<repo>@<ref>` |
| `release` | `<provider>:<owner>/<repo>@<tag_name>` |
| Cairn `artifact.created` (and the other artifact events) | `cairn:<artifact id>` |

These rules SHALL apply to every key:

* Provider, owner and repo are lowercased. The ref keeps its case, with `refs/heads/` or
  `refs/tags/` stripped.
* A key SHALL be at most 256 bytes.
* A key SHALL NOT contain any other payload text.

`SubjectOf` and work orders (SPEC-0020) SHALL be unchanged.

#### Scenario: A PR and its comments share a thread

- **WHEN** Gitea delivers `pull_request` opened for `Abrahms/Sublayer#57`, then a
  `pull_request_comment` on it
- **THEN** both todos have `subject_key = gitea:abrahms/sublayer#57`

#### Scenario: Workflow runs group by branch

- **WHEN** Gitea delivers `workflow_run` for `abrahms/sublayer` on `master`
- **THEN** the todo's key is `gitea:abrahms/sublayer@master`

### REQ-2: Thread Key on Todos

Every event-minted todo SHALL store `subject_key`, derived by REQ-1 when it is minted, in the same
insert. Todos from other paths SHALL store `null` unless the creator supplies a normalized ref as
`subject` (REQ-9). Other paths means the operator API, A2A and dev. `todos.subject_key` SHALL be
indexed with `(subject_key, created_at)`. No backfill is required. Existing todos have a null key
until the operator runs the optional backfill command (REQ-16).

### REQ-3: Reference Normalization

`internal/routing` SHALL provide `NormalizeRef(ref string, cfg RefConfig) (key string, ok bool)`. It
SHALL map these inputs to REQ-1 keys:

* **Already-canonical keys**, re-lowercased per REQ-1.
* **Short forms** `<owner>/<repo>#<n>` and `<owner>/<repo>@<ref>`. These are accepted only with a
  provider prefix, or when `cfg.DefaultProvider` is set.
* **Forge web URLs** on hosts listed in `cfg.ForgeHosts`, which maps a host to a provider:
  * `/<o>/<r>/issues/<n>`, `/<o>/<r>/pulls/<n>` and `/<o>/<r>/pull/<n>` map to `#<n>`;
  * `/<o>/<r>/src/branch/<ref>` and `/<o>/<r>/tree/<ref>` map to `@<ref>`.
* **`mcp://cairn/<id>`**, and `https://<cairn host>/a/<id>` on the configured Cairn origin, map to
  `cairn:<id>`.

It SHALL return `ok = false` for anything else. That includes unknown hosts, fragments, and inputs
over 512 bytes. It SHALL never perform I/O. `cfg` is built from `SWITCHBOARD_BASE_URL`-style
settings: `SWITCHBOARD_FORGE_HOSTS` (e.g. `gitea.stump.rocks=gitea,github.com=github`) and
`SWITCHBOARD_CAIRN_ORIGIN`. `github.com=github` is always included.

#### Scenario: Handle shapes agree

- **WHEN** `NormalizeRef` is given `https://cairn.stump.wtf/a/abc123` (Cairn origin configured) and
  `mcp://cairn/abc123`
- **THEN** both return `cairn:abc123`

### REQ-4: Links

Switchboard SHALL persist links. Each link has:

* `id`;
* `from_todo_id` and `from_attempt_seq` (nullable);
* `kind`: `produced` or `spawned`;
* `to_subject_key`, set exactly when the kind is `produced`;
* `to_todo_id`, set exactly when the kind is `spawned`;
* `provenance`: `declared`, `api`, `tagged` or `trace`;
* `actor_ref`: the endpoint id, human id or source label that made the statement;
* `created_at`.

A link SHALL be deleted when its `from_todo_id` todo is deleted. A `spawned` link SHALL also be
deleted when its `to_todo_id` todo is deleted. Duplicate statements SHALL be idempotent, keyed on
`(from_todo_id, kind, target, provenance)`. There SHALL be no `inferred` provenance, and no code path
SHALL create a link by comparing freeform text.

### REQ-5: Declaring What an Attempt Produced

`heartbeat`, `complete`, `fail` and `release` SHALL accept an optional `produced` array of strings.
It holds at most 16 entries per call, each at most 512 bytes. Each entry SHALL be normalized
(REQ-3):

* **An entry that normalizes** records a `produced` link from the todo and its current attempt, with
  provenance `declared`, and `actor_ref` set to the endpoint.
* **An entry that does not normalize** is ignored. The response SHALL carry `produced_ignored`, the
  number of entries that did not normalize, so agents can see a typo.

In the same transaction as the call:

* When `artifact` normalizes (SPEC-0034 REQ-5), it SHALL also record a `declared` `produced` link.
* On a fenced attempt, the lease token SHALL be required, exactly as for the call itself.

At most 64 `produced` links SHALL be kept per attempt. Entries beyond the cap are dropped and
counted in `produced_ignored`.

#### Scenario: Agent declares the PR it opened

- **WHEN** an agent completes todo A with `produced: ["https://gitea.stump.rocks/o/r/pulls/57"]`
  (host configured as gitea)
- **THEN** A has a `produced` link to `gitea:o/r#57` with provenance `declared`

### REQ-6: Produced From Spans

When SPEC-0036's receiver stores a bound span that carries `switchboard.produced` (a string, or an
array of strings), each value SHALL be normalized and recorded as a `declared` `produced` link from
the span's todo and attempt. The REQ-5 caps apply, and the receiver SHALL NOT reject the span over a
link cap.

### REQ-7: Resolving `produced` Links

A `produced` link from todo A (attempt claimed at `t`) to key K SHALL resolve, for a reader, to
every todo that meets all of these conditions:

* it is in the reader's owner scope;
* its `subject_key = K`;
* it was created at or after `t` minus 60 seconds, to absorb clock and ordering slack;
* it is not A itself.

Resolution SHALL happen at read time, so the order of the webhook and the declaration does not
matter. A link whose key matches nothing in scope SHALL render as the key, with "(nothing in view)"
and no link.

#### Scenario: Declaration after the webhook

- **WHEN** the webhook for `o/r#57` mints todo B at 10:00:05, and agent A (claimed at 09:58)
  completes with `produced: ["gitea:o/r#57"]` at 10:02
- **THEN** B's lineage shows A as its producer

#### Scenario: Foreign thread stays invisible

- **WHEN** human H1's agent declares `gitea:o/r#57`, and H2 owns the only todo with that key
- **THEN** neither H1's nor H2's views show a link, and H1 sees "(nothing in view)"

### REQ-8: Spawned Links From the Operator API and the Board

`POST /api/v1/endpoints/{ref}/todos` (ADR-0026) and any Board form that creates a todo SHALL accept
an optional `caused_by` todo id. When the named todo is in the caller's owner scope, a `spawned` link
from it to the new todo SHALL be recorded, with provenance `api` and `actor_ref` set to the human.
Otherwise nothing is recorded, and the response SHALL be identical for foreign and unknown ids.
The todo is still created.

### REQ-9: Subject on Created Todos

Todos created via the operator API or a Board form MAY carry `subject`: a string normalized by REQ-3
and stored as `subject_key`. When the string does not normalize, the key is null. This lets a human
put a hand-made todo into a thread.

### REQ-10: Links From Verified Producer Deliveries

When a verified delivery mints todo T and its subject is a Cairn artifact:

* **`todo:<id>` tags.** For each tag of the form `todo:<id>` (at most 8), when `<id>` names a todo
  in T's owner scope, Switchboard SHALL record a `spawned` link from `<id>` to T, with provenance
  `tagged`.
* **Trace ids.** When the delivery carries a trace id, Switchboard SHALL record a `spawned` link
  with provenance `trace`, from the todo whose attempt has that minted `trace_id` in T's owner
  scope, to T. The trace id comes from either:
  * a `data.trace_id` field (32 lowercase hex characters) in Cairn's companion change; or
  * a `trace:<32 hex>` tag.

In both cases, a todo that is unknown and one that is outside T's owner scope SHALL behave
identically: nothing is recorded. These links SHALL be written in the same transaction as T's
insert.

When the webhook has trusted actors configured (SPEC-0026), only a delivery whose actor is trusted
SHALL create links. Otherwise any verified delivery may.

#### Scenario: Handoff artifact links to its origin

- **WHEN** an agent working todo A creates a Cairn handoff tagged `todo:A`, and its delivery mints
  todo B on the same human's `lane-s`
- **THEN** B's lineage shows "spawned by A (tagged)"

### REQ-11: Tags on Todos

Tags SHALL match `^[a-z0-9._:/#-]{1,64}$`. A todo SHALL carry at most 32 tags. Each stored tag
records:

* `origin`: `rule`, `agent`, `human` or `producer`;
* `added_by`: the rule id, endpoint id, human id or source;
* `added_at`.

Adding a tag the todo already has changes nothing, and the original origin is kept. The sources
are:

* **Rule.** A routing rule action (`routing.Action`) SHALL accept `tags: [...]`, at most 8 tags.
  Every todo the rule mints gets them, with origin `rule`. `set_webhook_rules` and
  `test_webhook_rules` SHALL validate the tags against the grammar.
* **Agent.** `claim`, `claim_next`, `heartbeat`, `complete`, `fail` and `release` SHALL accept
  `tags: [...]`, at most 16 per call. An entry prefixed `-` removes that tag, but only when the tag
  has origin `agent`. An invalid entry, or one over the cap, is dropped and counted in
  `tags_ignored`.
* **Human.** The todo page SHALL let the owner add and remove tags of any origin, through
  CSRF-protected POSTs.
* **Producer.** A Cairn artifact delivery's tags that match the grammar SHALL be copied onto the
  minted todo, at most 32, with origin `producer`. Forge labels SHALL NOT be copied.

Tags are data. They SHALL be rendered escaped, and SHALL grant nothing.

#### Scenario: Rule tags

- **WHEN** a rule with action `{queue: "lane-s", tags: ["fleet", "lane:s"]}` mints a todo
- **THEN** the todo carries `fleet` and `lane:s`, both with origin `rule`

### REQ-12: Reading Tags, Threads and Links Over MCP

These fields SHALL be additive:

* `claim`, `claim_next` and `get_todo` SHALL return `subject_key` and `tags`, as a list of strings.
* `get_todo` SHALL also return `lineage`, with these fields:
  * `produced_by`: todos in the endpoint's scope that hold a `produced` link to this todo's key,
    each with `{todo_id, attempt_seq, provenance}`;
  * `spawned_by`: the same shape;
  * `produced`: the keys this todo's attempts declared, as keys only;
  * `spawned`: todo ids in scope.
  At most 20 entries are returned per list.
* `list_todos` SHALL accept `tag` and `subject_key` filters.

The MCP scope is the endpoint, per SPEC-0034 REQ-10.

### REQ-13: Lineage on the Todo Page

SPEC-0036's todo page SHALL gain a "Lineage" section with a visible heading, showing:

* **Upstream:** what produced or spawned this todo, walked up to 10 hops, with cycles cut and
  marked;
* **Downstream:** what this todo produced or spawned, one hop, with each produced key resolved per
  REQ-7;
* **Thread:** the other todos in its thread, newest first, up to 20, with a link to
  `/graph?thread=<key>`.

Every edge SHALL show its provenance as text: "declared by <endpoint>", "added by <human>",
"tagged on <artifact>" or "matched trace". Tags SHALL render as chips, with human add and remove
controls.

### REQ-14: The Graph View

`GET /graph` SHALL render the signed-in human's todos, grouped by thread.

* **Group headers.** Each group has a header showing the key in a human form, for example
  `abrahms/sublayer#57`, and the provider. Todos with a null key go in a trailing "Unthreaded"
  group.
* **Cards.** Todos appear inside their group in `created_at` order. Each card shows the id, title
  (escaped, truncated), state chip (text and colour), claimant chip (the endpoint name, with a
  per-endpoint colour derived from a fixed palette and always labelled), and tags. Each card links
  to its todo page.
* **Links.** Each card SHALL list its outgoing links as text: target, kind and provenance. Every
  link is marked up with `data-sb-edge-from`, `data-sb-edge-to`, `data-sb-edge-kind` and
  `data-sb-edge-provenance`.
* **Script.** `static/js/sb-graph.js`, served from `'self'`, SHALL draw an SVG overlay of arrows
  between the linked cards. Arrows are solid for `declared`, `api` and `trace`, and dashed for
  `tagged`. The overlay SHALL redraw on resize and after SSE swaps. It SHALL be `aria-hidden`,
  because the textual list is the accessible form.
* **Filters and limits.** Filters are `since` (default 24 h, max 30 days), `queue`, `endpoint`,
  `tag`, `state` and `thread`. The view holds at most 300 todos and 80 groups, ordered by the most
  recent activity in each group. When it truncates, a notice says so.
* **Live updates.** A new todo in view SHALL be appended to its group over SSE.

The rail SHALL gain "Graph" after "Activity".

#### Scenario: The mockup

- **WHEN** agent X, working `sublayer#57`'s opened todo, completes with
  `produced: ["gitea:abrahms/deaddrop#17"]`, and `deaddrop#17`'s todos exist in view
- **THEN** `/graph` shows groups `abrahms/sublayer#57` and `abrahms/deaddrop#17`, with a solid arrow
  from the `#57` todo to the first `#17` todo created after the claim, and the card's link list
  reads "produced abrahms/deaddrop#17 (declared by X)"

### REQ-15: Filters by Tag and Thread

`/todos`, `/activity` (SPEC-0036 REQ-15) and `/graph` SHALL accept `tag` and `thread` query
parameters. Clicking a tag chip or a thread header SHALL apply the filter.

### REQ-16: Optional Backfill

A `switchboard backfill-threads` subcommand (it connects with `SWITCHBOARD_DATABASE_URL`, like `serve`) SHALL compute `subject_key` for existing
event-minted todos from their stored events. It works in batches of 1000, is idempotent, and is
safe to run while the server is up. It is not run by migration.

### REQ-17: Tenant Isolation

Every read of threads, links and tags SHALL be filtered by the reader's owner scope. Every write
that names another todo (`caused_by`, `todo:` tags, trace matches) SHALL verify that todo is in the
writer's or the minted todo's owner scope, and SHALL behave identically for a foreign id and an
unknown one. A thread SHALL never include a todo outside the reader's scope. Link targets outside
the scope SHALL NOT be counted, named or hinted at.

#### Scenario: Probing with caused_by

- **WHEN** human H2 creates a todo with `caused_by` set to H1's todo id, and again with a random id
- **THEN** both responses are byte-identical apart from the new todo's id, and no link exists

### REQ-18: Bounds, Retention and Database Standards

These caps apply:

| Item | Cap |
| --- | --- |
| Tags per todo | 32 |
| Tags per rule | 8 |
| Tags per agent call | 16 |
| `produced` links per attempt | 64 |
| `spawned` links per todo, as target | 64 |
| `todo:` tags processed per delivery | 8 |
| Lineage walk depth | 10 |

The schema SHALL be a numbered migration. Links and tags SHALL cascade with their todos. Transactional
writes (REQ-5, REQ-8, REQ-10, REQ-11) SHALL commit with the call that makes them, or not at all. Store
tests SHALL run against Postgres.

### REQ-19: Metrics

Switchboard SHALL export:

* `switchboard_todo_links_total{kind, provenance}`;
* `switchboard_todo_link_statements_ignored_total{reason="unnormalized|out_of_scope|cap"}`;
* `switchboard_todo_tags_total{origin}`.

None of these SHALL carry a todo, endpoint or human label.

## Security Requirements

### Authentication

| Surface | Auth | Description |
| --- | --- | --- |
| MCP `produced`, `tags` arguments | Required | Endpoint credential; the verb's existing scope |
| `POST /api/v1/endpoints/{ref}/todos` (`caused_by`, `subject`) | Required | Operator API OAuth; owner scope |
| `GET /graph`, todo page lineage | Required | Human session; owner scope |
| Todo tag add/remove | Required | Human session; CSRF; owner scope |

### Rate Limiting

The new arguments ride existing verbs and routes and are covered by their existing limiters. `/graph`
is a GET on the human group, and uses its existing limits.

### Security Headers

The pages keep the SPEC-0012 CSP (`script-src 'self'`). `sb-graph.js` is vendored and served from
`/static`, and uses no `eval` and no inline handlers.

### Request Body Size Limits

These are the existing caps. Every new string field is length-capped (REQ-3, REQ-5, REQ-11).

### CSRF Protection

Tag add and remove are POSTs on the human group, behind `RequireCSRF`.

### Redirect Validation

No redirects are introduced. `NormalizeRef` never fetches, and link targets render as internal todo
links or as plain text keys, never as external links.

## Accessibility Requirements

- **WCAG 2.1 AA Compliance.** `/graph` and the lineage section SHALL meet WCAG 2.1 AA in both themes.
- **Non-visual graph.** The SVG overlay is decorative (`aria-hidden="true"`). Each card's textual link
  list is the accessible representation, and SHALL be complete without the script.
- **ARIA Landmarks.** `/graph` has one `<main>`. Each thread group is a `<section>` with its key as
  its heading.
- **Colour.** Claimant chips, state chips and provenance line styles SHALL each carry a text label.
  Colour SHALL NOT be the only signal.
- **Keyboard Navigation.** Cards, tag chips, filters and tag controls SHALL be reachable in a logical
  tab order with visible focus.
- **Dynamic Content Regions.** Cards appended over SSE land in an `aria-live="polite"` region per
  group.
