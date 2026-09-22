---
status: draft
date: 2026-09-22
implements: [ADR-0031]
requires: [SPEC-0001, SPEC-0003, SPEC-0006, SPEC-0011, SPEC-0020]
extends: [SPEC-0020]
---

# SPEC-0026: Fail-Closed Trusted-Actor Intake and Quarantine

## Overview

This spec makes Switchboard's intake gate fail closed. It has four parts:

* **Fail-closed routing.** A faulting rule stops evaluation, and the delivery is quarantined
  instead of falling through (#212).
* **Params survive omission.** Omitting `params` never clears them (#213).
* **First-class trusted actors.** A per-webhook `trusted_actors` field is evaluated in Go from
  verified fields.
* **Quarantine.** A reserved queue that no tool-bearing agent is ever pushed or handed, drained by
  a human or by a tool-less classifier endpoint.

It also specifies the routing recipe for the public GitHub mirrors. See
[ADR-0031](../../../adrs/ADR-0031-fail-closed-trusted-actor-intake-and-quarantine.md).

It extends [SPEC-0020](../event-routing/spec.md) (event routing): REQ "Deterministic Rule
Evaluation" (fault semantics), REQ "Rule Validation at Save Time", REQ "Rule Parameters", REQ
"Routing Envelope" (`.actor`, `.release`), REQ "Drop Action Semantics" (the new `quarantine`
action), and REQ "Work Orders" (`author_trusted`). It amends the SPEC-0011 sender gate so that
quarantined todos are never doorbell-eligible for ordinary endpoints.

Terms:

* **Disposition**: the outcome of intake for one verified delivery. It is `routed`, `dropped`,
  `quarantined` or `faulted`. A faulted delivery is also quarantined once REQ-6 is implemented.
* **Owner endpoint**: the endpoint that owns the receiving webhook (`endpoint_webhooks.endpoint_id`).
* **Classifier endpoint**: an endpoint vended with the classifier role (REQ-8).

## Requirements

### REQ-1: Faults Stop Evaluation

When evaluating a rule produces a fault (timeout, error, compile failure, or an exhausted per-event
budget), evaluation MUST stop at that rule. No later rule MUST be evaluated, and the default action
MUST NOT apply. This MUST hold for every rule, whatever its action. The delivery's disposition MUST
be `faulted`, and the fault MUST be recorded on the routing trace with the rule id, the rule index,
the cause and the detail.

Until REQ-6 is implemented, a faulted delivery MUST be persisted as an event with its trace and no
todo, spending its dedup slot exactly as a drop does. After REQ-6, it MUST be quarantined with
`quarantine_reason = "rule_fault"`.

Each faulted delivery MUST increment `switchboard_routing_faults_total{cause}`, and MUST log one
warning with the webhook id, rule id, rule index and cause. The owner MUST be able to see faulted
deliveries through `list_webhook_events` (filterable by `disposition`) and as a warning on the
webhook's board card.

#### Scenario: A mistyped trust rule no longer admits everyone

- **GIVEN** rules `[r1: any($params.trusted[]; . == .issue.author) | not → drop, r2: → queue "lane-m"]`
  and `params = {"trusted": "alice"}` (a string, not a list)
- **WHEN** an issue from `mallory` is delivered
- **THEN** r1 faults, r2 is not evaluated, no todo is created on `lane-m`, and the disposition is
  `faulted`

#### Scenario: A faulting drop rule does not route its delivery

- **GIVEN** a drop rule that times out on a large payload, followed by a default action of queue
  `inbox`
- **WHEN** such a payload is delivered
- **THEN** no todo is created on `inbox`, and the fault is recorded and counted

### REQ-2: Unavailable Sandbox Refuses the Delivery

When the routing sandbox cannot evaluate at all for a webhook that has rules (it failed to start,
or it failed wholesale for this delivery), the receiver MUST answer `503` with
`{"error": "routing unavailable"}`, MUST NOT persist an event or a todo, and MUST log an error, so
that the producer retries. A webhook with no rules MUST be unaffected.

#### Scenario: Sandbox down

- **GIVEN** a webhook with rules, on an instance whose sandbox failed to start
- **WHEN** a verified delivery arrives
- **THEN** the response is `503`, nothing is persisted, and the producer's retry succeeds once the
  sandbox is healthy

### REQ-3: Save-Time Fault Refusal and Param Typing

`set_webhook_rules`, `add_webhook_rule`, `update_webhook_rule` and `move_webhook_rule` MUST
dry-run the resulting configuration against up to the 50 most recent stored events of that webhook
before saving. If any rule faults on any of them, the save MUST fail with `invalid_argument` and
keep the previous configuration. The error MUST name each faulting rule id, the event id and the
cause. A webhook with no stored events MUST skip the dry-run.

Every `params` value MUST be a string, a number, a boolean, or a list whose elements are all
strings or all numbers. Any other shape, including nested objects and mixed lists, MUST be refused
with `invalid_argument` naming the key.

#### Scenario: A rule that faults on real traffic is refused

- **GIVEN** a webhook with 20 stored deliveries
- **WHEN** an agent adds a rule that faults on 3 of them
- **THEN** the save fails, the error lists the rule and the 3 event ids, and the stored rules are
  unchanged

#### Scenario: Nested params refused

- **WHEN** `set_webhook_rules` is called with `params = {"trusted": {"alice": true}}`
- **THEN** the call fails with `invalid_argument` naming `trusted`

### REQ-4: Params Are Never Cleared by Omission

`set_webhook_rules` MUST leave the stored `params` unchanged when the request omits `params`. It
MUST clear them only when the request passes `params: {}` explicitly. Every rules verb that returns
a configuration MUST echo the resulting `params`. The tool's schema description MUST state the
omit and clear semantics.

#### Scenario: Omitting params keeps them

- **GIVEN** a webhook whose params are `{"trusted_humans": ["joestump"]}`
- **WHEN** `set_webhook_rules` is called with `rules` and `default_action`, and no `params`
- **THEN** the stored params are still `{"trusted_humans": ["joestump"]}`, and the response echoes
  them

#### Scenario: Explicit clear

- **WHEN** `set_webhook_rules` is called with `params: {}`
- **THEN** the stored params are empty, and the response echoes `{}`

### REQ-5: Trusted Actors

A self-managed webhook MAY carry `trusted_actors`, owned by the webhook's owner scope:

* for `github` and `gitea` webhooks: `{"logins": [string], "match": "sender"|"author"|"both"}`,
  where `match` defaults to `"sender"`;
* for `cairn` webhooks: `{"actor_ids": [string]}`.

Lists MUST hold 0 to 256 non-empty strings of at most 128 bytes each. A field that does not belong
to the webhook's source type MUST be refused.

Management:

* `create_webhook` MUST accept an optional `trusted_actors`.
* `set_trusted_actors {webhook_id, trusted_actors}` MUST replace the field. `trusted_actors` MUST
  be a required argument.
* `clear_trusted_actors {webhook_id}` MUST remove the field.
* `list_webhooks` MUST echo the field, and a `quarantined` count of the webhook's open quarantine
  items.

These verbs MUST join the webhook-management verb family for grants, the vend wizard and consent.
A `webhook_id` that another endpoint owns MUST be answered with `not_found`, exactly like an
unknown id.

`trusted_actors` MUST be refused with `invalid_argument` on a token-trust (`generic`) webhook, and
on a source with no actor projection (`stripe`, `slack`). The error MUST say why.

**Actor projection.** Switchboard MUST parse, in Go and only from a verified body:

* **github and gitea**: `sender` is `sender.login`. `author` is the first present of
  `comment.user.login`, `review.user.login`, `pull_request.user.login` and `issue.user.login`, or
  null.
* **cairn**: `sender` and `author` are both the signed `actor_id`. `on_behalf_of` MUST NOT be used.

Logins MUST compare case-insensitively. Actor ids MUST compare exactly.

**Evaluation.** The gate MUST run after signature verification and before any rule, when
`trusted_actors` is set:

* `sender_trusted` MUST be whether `sender` is in the list, and `author_trusted` MUST be whether
  `author` is in the list. When `author` is null, `author_trusted` MUST equal `sender_trusted`.
* `trusted` MUST be `sender_trusted` for `match = "sender"`, `author_trusted` for
  `match = "author"`, and both for `match = "both"`.
* An untrusted delivery MUST be quarantined with `quarantine_reason = "untrusted_actor"`, and rules
  MUST NOT be evaluated for it.
* An empty list MUST trust no one.
* When `trusted_actors` is absent, the gate MUST NOT run, and behaviour MUST be unchanged.

#### Scenario: Maintainer label promotes an outsider's issue

- **GIVEN** a `github` webhook with `trusted_actors = {"logins": ["joestump"], "match": "sender"}`
- **WHEN** `mallory` opens an issue, and `joestump` then labels it
- **THEN** the `opened` delivery is quarantined, and the `labeled` delivery reaches the rules with
  `.actor = {sender: "joestump", author: "mallory", sender_trusted: true, author_trusted: false,
  trusted: true}`

#### Scenario: Case-insensitive login

- **GIVEN** `logins = ["JoeStump"]`
- **WHEN** a delivery's `sender.login` is `joestump`
- **THEN** the sender is trusted

#### Scenario: Token-trust webhook refused

- **WHEN** an agent calls `set_trusted_actors` on a `generic` webhook
- **THEN** the call fails with `invalid_argument`, saying the webhook's body is not signed

#### Scenario: Self-reported Cairn identity ignored

- **GIVEN** a `cairn` webhook with `actor_ids = ["acct_joe"]`
- **WHEN** a signed artifact event arrives with `actor_id = "acct_other"` and
  `on_behalf_of = "acct_joe"`
- **THEN** the delivery is quarantined as `untrusted_actor`

#### Scenario: Omitted argument never clears

- **WHEN** an agent calls `set_trusted_actors {"webhook_id": "…"}` without `trusted_actors`
- **THEN** the call fails with `invalid_argument`, and the stored list is unchanged

### REQ-6: Quarantine

`quarantine` MUST be a reserved queue name. It MUST be refused as a `create_webhook` target queue,
as a queue in any endpoint scope or webhook-queue ceiling, as a route queue, and as the queue of a
rule's `queue` action.

A delivery MUST be quarantined when:

* the trust gate finds it untrusted (`untrusted_actor`);
* a rule faults (`rule_fault`, REQ-1);
* the first matching rule's action is `{"quarantine": true}` (`rule_action`).

Quarantining MUST create exactly one todo on the **owner endpoint**, whatever the webhook's fan-out
targets, with `queue = "quarantine"`, `quarantine_reason`, and `quarantine_detail` (the fault or
the actor, as structured JSON). It MUST keep the delivery's event, trace and idempotency key, so a
redelivery collapses onto it.

A quarantined todo:

* MUST NOT ring a doorbell on any endpoint other than a classifier endpoint (REQ-8);
* MUST NOT fire a notify hook (SPEC-0024);
* MUST NOT be returned by `list_todos`, `claim` or `claim_next` on any endpoint;
* MUST be visible only in its owner scope's Quarantine view (REQ-9) and to that scope's classifier
  endpoints;
* MUST be auto-discarded 30 days after creation, or sooner if the operator's retention bound is
  shorter, with `outcome = "expired"`.

The store MUST apply the quarantine exclusion as a default filter on every todo read and lifecycle
path, so that no individual caller has to remember it.

#### Scenario: Quarantine is owned by the webhook's owner

- **GIVEN** endpoint A's webhook, which fans out to A and to friend endpoint F
- **WHEN** an untrusted delivery arrives
- **THEN** one quarantine todo exists on A, and F has nothing new

#### Scenario: No agent is handed quarantined work

- **GIVEN** a quarantined todo on endpoint A
- **WHEN** A's worker calls `claim_next`, calls `list_todos`, or calls `claim` with the todo's id
- **THEN** `claim_next` returns nothing from quarantine, `list_todos` omits it, `claim` fails with
  `not_found`, and no doorbell or hook fired when it was created

#### Scenario: Reserved name refused

- **WHEN** an agent calls `create_webhook {"source_type": "github", "target_queue": "quarantine"}`
- **THEN** the call fails with `invalid_argument`

### REQ-7: Release and Discard

A quarantined todo MUST leave quarantine only by release, discard, or expiry.

**Release** MUST route the delivery again, through the webhook's current rules, with the trust gate
treated as satisfied. The envelope MUST carry `.release = {by, at}`, where `by` is
`"human:<human_id>"` or `"classifier:<endpoint slug>"`. `.actor` MUST be unchanged, so
`author_trusted` and `sender_trusted` still reflect the original delivery. When the release names a
`queue`, that queue MUST be within the owner endpoint's webhook-queue ceiling, and rules MUST be
skipped. A released todo MUST keep its id. It MUST move to the routed queue or queues on the owner
endpoint and the webhook's fan-out targets, as the routing decision says, and MUST ring doorbells
and fire hooks exactly as a freshly routed todo would. If routing places it back in quarantine, or
faults, the release MUST fail with `conflict`, and the todo MUST stay quarantined.

**Discard** MUST complete the todo with state `done` and the result `{"discarded": true, "reason",
"by"}`.

Every release, discard and expiry MUST be recorded on the todo with who, when and the outcome.

#### Scenario: Human releases an outside report to triage

- **GIVEN** a quarantined outsider issue, and rules that send `.release.by | startswith("human:")`
  to `lane-m`
- **WHEN** the owner releases it from the Quarantine view
- **THEN** it becomes a `lane-m` todo whose work order carries `author_trusted = false` and
  `released_by = "human:<id>"`, and `lane-m`'s worker is rung

#### Scenario: Release into quarantine again is refused

- **GIVEN** rules whose first match for the released delivery is `{"quarantine": true}`
- **WHEN** the owner releases it without naming a queue
- **THEN** the release fails with `conflict`, and the todo stays quarantined

### REQ-8: Classifier Endpoints

A human MAY vend an endpoint with the **classifier role**. Its scope MUST contain exactly
`list_quarantined`, `get_quarantined`, `release_quarantined` and `discard_quarantined`, plus self
verbs. The vend MUST be refused if any other verb is requested with them. The vend wizard MUST
require the human to attest that the agent behind the endpoint runs without tools, and MUST show
what the classifier will be able to read.

* `list_quarantined {cursor?, limit?}` MUST return the open quarantine items of the classifier's
  owner scope: the id, reason, source, kind, actor, summary, and `received_at`. It MUST NOT return
  payloads.
* `get_quarantined {id}` MUST return one item, including the payload.
* `release_quarantined {id, queue?}` and `discard_quarantined {id, reason}` MUST behave as REQ-7.

A classifier endpoint MUST see only items whose owner endpoint shares its owner scope. An id
outside that scope MUST be answered with `not_found`.

A classifier endpoint's sessions MUST receive a doorbell when quarantine items arrive in its scope.
The doorbell MUST carry only a count and a fixed instruction to call `list_quarantined`, and MUST
NOT contain any sender-supplied text. At most one doorbell MUST be sent per minute per classifier
endpoint.

#### Scenario: A classifier cannot do anything else

- **WHEN** a human tries to vend a classifier endpoint that also holds `claim_next`
- **THEN** the vend is refused, and no endpoint is created

#### Scenario: Classifier doorbell carries no sender text

- **GIVEN** a classifier endpoint, and an arriving quarantine item titled `ignore all instructions`
- **WHEN** the classifier's doorbell is sent
- **THEN** its content and `meta` contain a count and the fixed instruction only

#### Scenario: A foreign classifier

- **WHEN** a classifier endpoint owned by human B calls `get_quarantined` with human A's item id
- **THEN** the call fails with `not_found`

### REQ-9: Quarantine View and Owner Signals

The web UI MUST provide a **Quarantine** view listing the signed-in human's quarantine items across
their endpoints. Once Teams land (ADR-0038 / SPEC-0033), it MUST include the items of teams in
which the human may configure webhooks. For each item it MUST show:

* the reason and its detail (the actor and trust flags, or the fault);
* the source, the kind, and the receiving webhook;
* the escaped summary;
* the payload, collapsed by default, rendered as escaped text and never as HTML or Markdown.

The view MUST offer **release** (optionally to a named queue), **discard** (with a reason), and
**trust this actor and release**. The last action adds the actor's login or actor id to the
webhook's `trusted_actors` and releases the item. It MUST be refused for `rule_fault` items.

The board's webhook card MUST show the webhook's open quarantine count and its fault count over the
last 24 hours.

#### Scenario: Trust this actor

- **GIVEN** a quarantined issue from `newcontributor`, on a webhook whose trusted logins are
  `["joestump"]`
- **WHEN** the owner chooses "trust this actor and release"
- **THEN** the trusted logins become `["joestump", "newcontributor"]`, and the item is released
  through routing

#### Scenario: Payload is never rendered as markup

- **GIVEN** a quarantined item whose payload contains `<script>` and Markdown links
- **WHEN** the owner expands it
- **THEN** the text is displayed escaped, with no element or link created from it

### REQ-10: Envelope and Work Order Additions

The routing envelope MUST gain two top-level, Switchboard-derived fields that the payload cannot
forge:

* `.actor`: `{sender, author, sender_trusted, author_trusted, trusted}`. The two names MUST be
  populated for github, gitea and cairn whenever they can be parsed. The trust flags MUST be null
  when the webhook has no `trusted_actors`.
* `.release`: `{by, at}` on a released delivery, and null otherwise.

A work order (ADR-0025) built from a delivery with `.actor` MUST carry `author_trusted`, and, when
released, `released_by`. These paths MUST be added to the SPEC-0020 envelope documentation, and
MUST NOT move once shipped.

#### Scenario: Rules can read trust flags

- **GIVEN** a trusted delivery with an untrusted author
- **WHEN** a rule tests `.actor.author_trusted == false`
- **THEN** the rule matches

### REQ-11: Metrics

Switchboard MUST add these series to the SPEC-0023 registry, with bounded labels:

```
switchboard_routing_faults_total{cause}                    counter  # timeout|error|compile|budget
switchboard_quarantine_items_total{reason}                 counter  # untrusted_actor|rule_fault|rule_action
switchboard_quarantine_resolved_total{outcome,by}          counter  # outcome: released|discarded|expired; by: human|classifier|system
switchboard_quarantine_oldest_seconds                      gauge
```

The existing `switchboard_queue_todos{queue="quarantine",…}` series reports quarantine like any
other queue. The metrics guide's queue-liveness alert MUST exclude `queue="quarantine"`, and MUST
document a separate alert on `switchboard_quarantine_oldest_seconds`.

#### Scenario: Liveness alert ignores quarantine

- **GIVEN** 5 open quarantine items and no claims
- **WHEN** the documented liveness alert expression is evaluated
- **THEN** it does not fire for `queue="quarantine"`

### REQ-12: Public Mirror Intake Recipe

The routing documentation MUST include a recipe, "Outside intake from the public GitHub mirrors".
It MUST cover:

* one signed `github` webhook, installed at the GitHub organization level for `issues` and
  `issue_comment`;
* `trusted_actors` with the maintainers' logins and `match = "sender"`;
* rules that route trusted `labeled` events to lanes, and map the mirror repository to its
  canonical tracker in the work order;
* everything else left to quarantine;
* the promotion flow, in which a maintainer's label moves an item on;
* the warning that an outsider's text keeps `author_trusted = false` downstream.

The recipe's example rules MUST be exercised by a routing test against recorded mirror payloads.

#### Scenario: Recipe test

- **WHEN** the recipe's rules are run against a recorded outsider `opened` event and a maintainer
  `labeled` event
- **THEN** the first is quarantined and the second routes to a lane, in CI

### REQ-13: Error Handling, Concurrency and Database Standards

Every error MUST be wrapped with the webhook id and the stage (verify, trust gate, route,
quarantine, release). A quarantine write MUST happen in the same transaction as the event insert,
so a delivery is never persisted without its disposition. Release MUST lock the todo row
(`FOR UPDATE`) so that concurrent release and discard resolve once. The loser MUST get
`conflict`. Queries MUST be parameterized. Background expiry MUST be a single statement,
safe under concurrent instances, and MUST pass `go test -race`.

#### Scenario: Concurrent release and discard

- **WHEN** a human releases an item at the same moment a classifier discards it
- **THEN** exactly one succeeds, the other gets `conflict`, and the todo records one
  outcome

## Security Requirements

### Authentication

| Surface | Auth | Description |
|---|---|---|
| MCP `set_trusted_actors`, `clear_trusted_actors`, rules verbs | Required | Endpoint credential, webhook-family grant, owner scope |
| MCP `list_quarantined`, `get_quarantined`, `release_quarantined`, `discard_quarantined` | Required | Classifier-role endpoint credential only |
| Web UI Quarantine view and actions | Required | Signed-in human, owner scope, CSRF token |
| `POST /webhooks/w/{token}` | Public | Inbound delivery, authenticated by signature or unguessable URL (unchanged, SPEC-0001 and SPEC-0006) |

### Rate Limiting

Inbound webhooks keep the existing per-IP limiter. The save-time dry-run is bounded to 50 events
and runs inside the existing per-event CPU budget. Classifier doorbells are limited to one per
minute per endpoint.

### Security Headers

The Quarantine view MUST carry the existing `secureHeaders` set, including a CSP with no inline
script, because it displays attacker-supplied text.

### Request Body Size Limits

Inbound bodies stay capped at 5 MiB (SPEC-0001). A quarantine item stores the event's existing
payload and does not copy it. `get_quarantined` returns the payload as data inside the MCP response
limit.

### CSRF Protection

Every Quarantine view action MUST be a POST carrying the existing CSRF token.

### Redirect Validation

Quarantine actions MUST redirect only to the same-origin Quarantine view.

### Tenancy

Trust lists and quarantine items MUST be readable and changeable only within the webhook's owner
scope. The instance operator role MAY bound retention and MUST NOT read, release or discard tenant
items through the product. Unknown ids and foreign ids MUST both answer `not_found`.
