---
status: draft
date: 2026-09-22
implements: [ADR-0036]
requires: [SPEC-0006, SPEC-0020]
related: [SPEC-0023]
---

# SPEC-0031: Rule Packs as Installable Presets

## Overview

A **rule pack** is a named, versioned routing preset shipped inside switchboard: rules, a default, typed
params, install-time checks, and fixtures. An owner installs one on a webhook with
`install_rule_pack`. The install is always two calls. The first builds the candidate configuration and
**replays** it against the webhook's own stored deliveries through the production router, then returns
a plan: what would change, which rules would match, which would fault, and which deliveries would stop
becoming work. Only the second call, presenting that plan's token, saves anything. Installed packs
carry provenance, so they can be listed, upgraded with a diff, and removed. See
[ADR-0036](../../../adrs/ADR-0036-rule-packs-as-installable-presets.md).

This spec extends [SPEC-0020](../event-routing/spec.md). It adds a replay mode to REQ "Routing Dry-Run",
and it adds verbs to REQ "Rule Management Tools" and to the webhook family of
[SPEC-0006](../agent-tools/spec.md). It changes no evaluation semantics: installed pack rules are
ordinary rules. The built-in packs' seed is `docs/routing/rule-packs/`.

Parallel records, cited by number until they merge: the fail-closed trusted-actor gate, `.actor` and
quarantine are ADR-0031 / SPEC-0026; owner scopes and team roles are ADR-0038 / SPEC-0033.

Terms:

* **Catalog**: the set of packs embedded in the running binary.
* **Candidate**: the webhook configuration an install, upgrade or removal would produce.
* **Replay**: evaluating the saved configuration and a candidate, side by side, over a window of the
  webhook's stored deliveries.
* **Plan**: the result of a replay, with a single-use token that authorizes applying exactly that
  candidate.
* **Provenance**: the record, on the webhook, of each installed pack.

## Requirements

### Requirement: REQ-1 Pack Format and Catalog

Each pack in the catalog MUST have:

* `name`: lower-case `[a-z0-9-]`, 3–24 characters;
* a semantic `version`;
* `title` and `summary`;
* `params_schema`: each param's `type` (`string`, `bool`, `int`, `string_list`, `object`, or a registered
  shared type), `required`, `default` and `description`;
* `param_checks`: zero or more `{id, expr, message}` jq predicates over `$params`;
* `rules`, whose ids are pack-local, and an optional `default_action`;
* `placement`: `prepend` or `append`;
* `requires`: `source_types`, `capabilities` and `packs`;
* `fixtures`: each a real-shaped delivery (source, headers, body, verified) with its expected
  decision.

A param name used by more than one pack MUST have the same type in all of them. The built-in catalog
MUST be embedded in the binary at build time. An operator MAY add external packs with
`SWITCHBOARD_RULE_PACK_SOURCES` (comma-separated `file://` directories or `https://` URLs), which MUST
be unset by default. External packs MUST load once at startup and MUST NOT be refetched while running;
each MUST pass the built-in validation and fixture routing or be skipped with a logged error; none MAY
reuse a built-in name; they MUST be listed with `source: external`; and they MUST install only through
plan-then-confirm. Every catalog version MUST
have a repository test that validates it and routes every one of its fixtures to its expected decision,
both in-process and through the sandbox child. The build MUST fail if two packs declare one param name
with different types.

#### Scenario: No external packs by default

- **WHEN** `SWITCHBOARD_RULE_PACK_SOURCES` is unset
- **THEN** `list_rule_packs` returns only built-ins, and nothing is fetched or read from outside the
  binary

#### Scenario: An operator adds external packs

- **GIVEN** `SWITCHBOARD_RULE_PACK_SOURCES=file:///etc/switchboard/packs`, holding two packs, one of
  which fails its fixtures
- **WHEN** the server starts
- **THEN** the valid pack is listed with `source: external`, and the failing pack is skipped with a
  logged error naming it

#### Scenario: A pack that contradicts its fixtures does not build

- **WHEN** a pack's rule is changed so that one of its fixtures no longer routes to the expected
  decision
- **THEN** the repository test fails, naming the pack, the version and the fixture

#### Scenario: Shared params agree

- **WHEN** `trusted-actors` and `handoff-lanes` both declare `trusted_humans`
- **THEN** both declare it as `string_list`, and a change that gives one of them a different type fails
  the build

### Requirement: REQ-2 Listing Packs

The webhook family MUST expose `list_rule_packs {webhook_id?}`. Without `webhook_id`, it MUST return
every catalog pack with its versions, `title`, `summary`, `params_schema`, `requires`, and whether each
version is available on this instance, which depends on its `requires.capabilities`. With a
`webhook_id` the caller's owner scope owns, it MUST also return, per installed pack:

* the installed `version` and whether a newer version is available;
* `proven`, and the date of the last replay;
* `drift`: `none`, `edited` (an installed rule's current content differs from its installed digest) or
  `removed` (an installed rule id is no longer present), with the affected rule ids.

A `webhook_id` that is unknown, malformed or owned by another owner scope MUST return `not_found`,
and the three cases MUST be indistinguishable.

#### Scenario: Drift is visible

- **WHEN** an owner edits a `no-self-review` rule with `update_webhook_rule` and then lists packs for
  that webhook
- **THEN** `no-self-review` reports `drift: edited`, naming that rule's id

#### Scenario: A version that needs a missing capability

- **WHEN** the instance does not implement SPEC-0026 and a caller lists packs
- **THEN** `trusted-actors` version 2 is listed with `available: false` and the missing capability
  named

### Requirement: REQ-3 Plan Before Apply

`install_rule_pack {webhook_id, name, version?, params?, replaces?, replay?}` without `confirm` MUST NOT change the
webhook. It MUST:

1. build the candidate from the saved configuration and the pack (REQ-6);
2. validate the candidate exactly as a save would under SPEC-0020 REQ "Rule Validation at Save Time",
   including the grant, and also validate param types against `params_schema` and evaluate every
   `param_checks` predicate;
3. replay the saved configuration and the candidate (REQ-4);
4. route the pack's fixtures through the candidate using the production router;
5. return the plan (REQ-5).

`install_rule_pack {…, confirm: plan_token}` MUST apply exactly the candidate the plan described, in one
write under the webhook's row lock, and MUST refuse with `plan_stale` when any of these holds:

* the webhook's configuration changed after the plan was made;
* the token expired (15 minutes);
* the token was already used.

A confirmed call's other arguments MUST match the plan's, or be omitted. There MUST be no path that
saves a pack's rules, params or provenance without a confirmed plan token.

#### Scenario: The first call saves nothing

- **WHEN** an owner calls `install_rule_pack` without `confirm`
- **THEN** `list_webhook_rules` returns the same configuration as before, and the response carries a
  plan and a `plan_token`

#### Scenario: A stale plan is refused

- **WHEN** an owner plans an install, another session then adds a rule to the same webhook, and the
  owner confirms the plan
- **THEN** the confirm fails with `plan_stale`, and nothing is saved

#### Scenario: A plan cannot be reused

- **WHEN** an owner confirms a plan and then confirms the same token again
- **THEN** the second call fails with `plan_stale`

### Requirement: REQ-4 Replay Against Stored Deliveries

A replay MUST evaluate both the saved configuration and the candidate over the webhook's most recent
stored deliveries: `limit` defaults to 200 and MUST be at most 1,000, optionally narrowed by `since`.
It MUST read only events recorded on that webhook, reconstruct each envelope from the event's stored
headers, body, kind and verification result, and use the same router (the sandboxed child) that ingest
uses. It MUST NOT create todos, claim once-keys, or write anything but the plan.

A pack rule that faults on any replayed delivery or any fixture MUST refuse the plan with
`pack_rule_faults`, naming the rule and up to 20 event ids. When SPEC-0026's fail-closed engine is
present, a faulted delivery counts as a fault here too, whatever that engine then does with it.

When the replay window holds no delivery of any of the pack's `requires.source_types`, the plan MUST
report `proven: false`, and the confirm MUST require `allow_unproven: true`.

A replay MUST run under a total time budget (30 s by default). When the budget runs out, it MUST return
the partial results marked `truncated: true`. A truncated plan MUST report `proven: false`, its confirm
MUST require `allow_unproven: true`, and the installed provenance MUST record `truncated: true`. At most one
replay per webhook MAY run at a time. A second MUST fail with `busy`.

#### Scenario: A truncated replay applies only by opt-in

- **WHEN** a replay runs out of its time budget and returns `truncated: true`
- **THEN** a confirm without `allow_unproven: true` is refused, and with it the install records
  `proven: false, truncated: true`

#### Scenario: The rebinding trap is caught

- **WHEN** a pack rule reads `.payload.sender.login` inside `any()`, where `.` is the generator's
  string element, and so faults on every stored comment delivery
- **THEN** the plan is refused with `pack_rule_faults`, naming the rule and example event ids

#### Scenario: A mistyped allowlist is refused before replay

- **WHEN** an install supplies `trusted_humans: "alice"` (a string) to a pack whose schema declares
  `string_list`
- **THEN** the plan is refused with `invalid_params` naming `trusted_humans`, and no replay runs

#### Scenario: A brand-new webhook

- **WHEN** an owner installs `drop-ci-noise` on a webhook that has recorded no deliveries
- **THEN** the plan reports `proven: false`, the confirm without `allow_unproven: true` is refused, and
  with it the install succeeds and its provenance records `proven: false`

#### Scenario: Another tenant's deliveries never enter a replay

- **WHEN** two owners' webhooks both receive deliveries from the same forge
- **THEN** each replay reads only events recorded on its own webhook

### Requirement: REQ-5 Plan Contents

A plan MUST report:

* `events_replayed`, `window_from` and `window_to`;
* decision totals before and after, per queue, `drop`, default and `fault`;
* `changed`, the count of deliveries whose decision differs. Transitions MUST be grouped as `from → to`,
  each with up to 20 example event ids, and the groups that turn a queue into a drop MUST be listed
  first;
* per rule, for every rule in the candidate: first-match count, fault count, and whether it came from
  the pack. Pack rules with a zero first-match count MUST be flagged `never_matched`;
* the fixture results;
* the estimated todos per day before and after, extrapolated from the window;
* the rule diff (added, removed and changed, by id) and the param diff (added, changed and removed
  keys);
* `proven` and `truncated`;
* the `plan_token` and its expiry.

A plan MUST NOT contain delivery payload text or header values. Event ids are the only link to the
deliveries, which the owner can inspect with `get_webhook_event`.

#### Scenario: Lost work is shown first

- **WHEN** a candidate would drop deliveries the saved configuration routes to `forge`
- **THEN** the plan's first transition group is `forge → drop`, with its count and example event ids

#### Scenario: A dead rule is flagged, not blocked

- **WHEN** a pack rule matches none of 200 replayed deliveries and faults on none
- **THEN** the plan flags it `never_matched`, and the plan is still confirmable

### Requirement: REQ-6 Merge and Composition

A candidate MUST keep every rule and param the pack does not install or own. Installed rule ids MUST be
`<name>--<local-id>`, and MUST fit SPEC-0020's id rules. `prepend` packs MUST be inserted before all
non-pack rules, and `append` packs after them. Within the pack group, a pack MUST follow every pack
named in its `requires.packs`, and an install whose required pack is absent MUST be refused with
`requires_pack`. A param the caller supplies MUST be set. A param the caller omits MUST keep the
webhook's current value for that key if it has one, whether or not a pack owns it, and otherwise MUST
take the schema default. A required param with neither MUST be refused with `invalid_params`. Keys the
pack declares become owned by the pack, and the plan's param diff MUST list keys it adopted from
existing values. At most one installed pack MAY set
`default_action`. A second MUST be refused with `default_conflict`. A candidate over SPEC-0020's
32-rule limit MUST be refused with `too_many_rules`, naming the installed packs and their rule counts.

**Adopting hand-written rules.** `replaces` MAY name rules on the webhook that carry no pack provenance.
The candidate MUST remove them before it inserts the pack's rules, so a pack can take over the job of
rules that were pasted in by hand. The rule diff MUST list them as `replaced`, and the replay compares
the saved configuration, which still has them, with the candidate, which does not. `changed: 0` is
therefore the proof that the adoption preserves behaviour. `replaces` MUST NOT name a rule installed by
a pack (use an upgrade or `remove_rule_pack` for those), and an id that is not on the webhook MUST be
refused with `invalid_argument`.

#### Scenario: Installing a second pack keeps the first

- **WHEN** an owner installs `drop-ci-noise` on a webhook that already has `no-self-review` and two
  hand-written rules
- **THEN** after confirming, all of those rules and their params are still present, in their prior
  relative order

#### Scenario: Order is enforced

- **WHEN** an owner installs `handoff-lanes` on a webhook without `trusted-actors`
- **THEN** the plan is refused with `requires_pack` naming `trusted-actors`

#### Scenario: A shared param changes for both packs

- **WHEN** an owner installs `handoff-lanes` with a new `trusted_humans` on a webhook where
  `trusted-actors` already owns `trusted_humans`
- **THEN** the plan's param diff names both packs, and the replay shows the combined effect

#### Scenario: Adopting a pasted-in configuration

- **WHEN** a webhook holds the 27 rules of `fleet.json`, pasted in by hand with its params, and the
  owner plans `trusted-actors@1` and then `handoff-lanes@1` with `replaces` naming those rules
- **THEN** the candidates stay within 32 rules, the params are adopted rather than reset to defaults,
  the rule diff lists the replaced rules, and the replay reports `changed: 0`

### Requirement: REQ-7 Provenance, Upgrade and Removal

A confirmed install MUST record provenance on the webhook: `name`, `version`, the installed rule ids, a
digest of those rules and of any `default_action` as installed, the owned param keys, `installed_at`,
`installed_by` (the endpoint and its owning human), `proven`, and a replay summary.

Installing another version of an installed pack MUST plan an upgrade or a downgrade. Its plan MUST add
the rule diff and the param diff between the installed and target versions, and its replay MUST compare
the saved configuration with the upgraded candidate. When any installed rule of that pack has drift, the
plan MUST be refused with `local_edits`, listing the rules, unless `overwrite_local_edits: true` is set.
`overwrite_local_edits` applies only to rules of an installed pack. Rules without provenance are
adopted through `replaces` (REQ-6).

`remove_rule_pack {webhook_id, name}` MUST plan and confirm exactly as an install does. Its candidate
removes the pack's installed rules, its `default_action` if it set one, and the params it alone owns.

#### Scenario: Upgrade over a hand edit

- **WHEN** an owner has edited a `drop-ci-noise@1` rule and then plans an install of `drop-ci-noise@2`
- **THEN** the plan is refused with `local_edits` naming the rule; with `overwrite_local_edits: true`
  the plan proceeds and its rule diff shows the edit being replaced

#### Scenario: Removal is replayed too

- **WHEN** an owner plans `remove_rule_pack` for `drop-ci-noise`
- **THEN** the plan shows how many replayed deliveries would become todos again, and nothing changes
  until the owner confirms

### Requirement: REQ-8 Replay for Hand-Written Rules

`test_webhook_rules` MUST accept `replay: {limit?, since?}` in place of `event_id` or `payload`, with
candidate `rules`, `default_action` and `params` as today. It MUST return the REQ-5 report without a
`plan_token`. Candidate rules without an `id` MUST be given ids, as `set_webhook_rules` gives them (#196),
so any candidate that would save also replays. Supplying more than one of `event_id`, `payload` and
`replay`, or none, MUST return `invalid_argument`.

#### Scenario: A hand-written rule gets the same evidence

- **WHEN** an agent replays a candidate drop rule with a wrong event-kind list over 200 stored
  deliveries
- **THEN** the report shows that rule with zero first matches, flagged `never_matched`

#### Scenario: Id-less candidates replay

- **WHEN** an agent replays candidate rules that carry no `id`
- **THEN** the replay runs, and the report names the minted ids

### Requirement: REQ-9 Built-in Packs

The catalog MUST ship these packs at version 1.

**`no-self-review`**, with params `identity` (`string`), `pairs` (a list of author and reviewer login
pairs), `families` (an object mapping logins to model families), and `require_cross_family` (`bool`).
With `identity` set:

* it MUST drop `pull_request` review requests whose requested reviewer is not `identity`;
* it MUST drop review triggers (`opened`, `reopened`, `synchronized`, `synchronize`, `edited`,
  `ready_for_review`, `review_requested`) on PRs authored by `identity`;
* it MUST drop review outcomes on PRs not authored by `identity`.

With `pairs` also set, it MUST additionally drop review triggers and review requests on PRs whose author
is not paired with `identity` as reviewer. With `require_cross_family: true`, a `param_check` MUST
refuse any pair whose author and reviewer map to the same family. With no `identity`, every review
request MUST fail closed.

**`trusted-actors`**, with params `require_verified`, `trusted_humans`, `trusted_agents` and
`cairn_actors`:

* version 1 MUST drop unverified deliveries, Cairn handoffs from actors outside `cairn_actors`, issues by
  authors outside the trusted lists, and label events by senders outside them. Every allowlist MUST be
  read so that a malformed value admits no one;
* version 2 MUST require the `trusted_actors` capability of SPEC-0026. Its install MUST set the
  webhook's first-class `trusted_actors` from the params, and its single rule MUST route
  `.actor.trusted | not` to `{"quarantine": true}`.

**`drop-ci-noise`**, with params `bot_actors`, `drop_deleted` (default true) and `keep_ci_success`
(default false). It MUST drop:

* deliveries whose action is `deleted`, when `drop_deleted` is true;
* `issue_comment`, `pull_request_comment`, `pull_request_approved` and `pull_request_rejected` events
  whose `sender.login` is in `bot_actors`;
* CI status, workflow and check events that are not completed failures, unless `keep_ci_success` is
  true.

**`handoff-lanes`**, with params `repo_prefixes` and `lane_queues`, and the shared trusted-actor
params. It MUST require `trusted-actors`, and MUST route Cairn handoffs and forge issues into lanes, hold
and triage exactly as `fleet.json` rules 2, 4–16 and 19–27 do today, with `default_action` drop.

The composition `trusted-actors@1` plus `handoff-lanes@1` MUST route every case in
`internal/routing/testdata/fleet/cases.json` to that case's expected decision (`want`). The composition
`no-self-review@1` (identity mode) plus `drop-ci-noise@1` with `drop_deleted: false` and
`keep_ci_success: true` MUST route every case of `pool_review_pack_test.go` to its expected decision.
Once they do, `docs/routing/rule-packs/fleet.json` and `pool-review.json` MUST be deleted in the same
change, with a CHANGELOG line, and the tests MUST read the cases' expectations instead of those files.

When a built-in pack ships a new major version that supersedes an older one (for example
`trusted-actors@2`), the older version MUST leave the catalog in the same release, with an upgrade note.
Webhooks that installed it MUST keep routing unchanged and MUST list `upgrade_available`; planning the
retired version afterwards MUST fail with `unavailable_version`.

#### Scenario: A cross-family pair map

- **WHEN** an owner installs `no-self-review` with `identity: reviewer-b`, `pairs: [{author:
  author-a, reviewer: reviewer-b}]`, families mapping the two logins to different model families, and
  `require_cross_family: true`
- **THEN** a PR by `author-a` requesting `reviewer-b` reaches the pool, while a PR by a human author, and
  a PR by `reviewer-b` itself, are dropped

#### Scenario: Same-family pairs are refused at install

- **WHEN** the two logins in a pair map to the same family and `require_cross_family` is true
- **THEN** the plan is refused with `invalid_params`, naming the `param_check` and the pair

#### Scenario: The seed packs are preserved

- **WHEN** the fleet fixture suite runs against a webhook with `trusted-actors@1` and `handoff-lanes@1`
  installed
- **THEN** every case routes to the decision it routes to under `fleet.json`

### Requirement: REQ-10 Authorization

`list_rule_packs`, `install_rule_pack` and `remove_rule_pack` MUST be members of the webhook-rules verb
family: gated by the endpoint's verb allowlist, and listed by the vend wizard and the consent screen.
Every verb that names a webhook MUST require the caller's owner scope to own it (ADR-0038), and MUST
answer `not_found` otherwise. A `plan_token` MUST be bound to the webhook, the owner scope, the pack
version, the params and the candidate digest, and MUST be refused as `not_found` when it is presented
for another owner scope.

#### Scenario: A token does not cross owners

- **WHEN** human B's endpoint presents a `plan_token` minted for human A's webhook
- **THEN** the call fails with `not_found`, and nothing is saved

#### Scenario: An endpoint without the verb

- **WHEN** an endpoint whose allowlist lacks `install_rule_pack` connects
- **THEN** the tool is not offered, and a direct call is refused

## Security Requirements

* **Authentication.** Every verb in this spec is an MCP tool on the vended endpoint surface, and
  requires the endpoint's bearer credential. No HTTP route is added. The operator surfaces are
  unchanged.

  | Surface | Auth | Notes |
  |---|---|---|
  | `list_rule_packs` | Required | Catalog is identical for every caller; webhook detail is owner-scoped |
  | `install_rule_pack`, `remove_rule_pack` | Required | Owner-gated; plan tokens bound to owner scope |
  | `test_webhook_rules` replay | Required | Owner-gated; reads only that webhook's events |

* **Rate limiting.** At most one replay runs per webhook at a time, and each endpoint MAY start at most
  10 replays per minute, on top of the vended surface's existing per-endpoint limiter.
* **Security headers, request size and CSRF.** These are unchanged: the MCP transport's limits and
  bearer authentication apply (SPEC-0014). No cookie-authenticated or browser surface is added.
* **Redirects.** None.
* **Supply chain.** The built-in catalog is compiled into the binary. No surface fetches, imports or
  evaluates a pack from outside the binary unless the operator sets `SWITCHBOARD_RULE_PACK_SOURCES`
  (unset by default). External packs load once at startup, pass the built-in validation and fixtures,
  and never replace a built-in.
* **Sandboxing.** Replay and fixture evaluation run in the existing sandbox child under SPEC-0020's
  expression restrictions and per-event budgets. `param_checks` predicates are held to the same
  restrictions.
* **No disclosure.** A plan carries decisions, counts and event ids, never payload text or header
  values.
* **No widening.** Pack rules are validated against the webhook's grant at plan time and again at
  apply, and re-enforced at evaluation. A pack cannot route to a queue or endpoint the webhook cannot
  already reach.

## Accessibility Requirements

Not applicable: this spec adds no UI. Rule packs are managed through MCP tools.
