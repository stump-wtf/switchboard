# Design: Rule Packs as Installable Presets

## Context

[SPEC-0031](spec.md) realizes [ADR-0036](../../../adrs/ADR-0036-rule-packs-as-installable-presets.md) on
top of SPEC-0020's routing stage:

* `internal/routing` is a leaf package of pure functions (`Validate`, `Route`, the sandbox child), so
  ingest, the rule verbs and the dry-run already share one evaluator.
* `internal/mcp/webhook_rules.go` holds the seven rule verbs, including `test_webhook_rules`, which
  routes one `event_id` or `payload`, and `mutateRules`, the row-locked read-modify-validate-write.
* `internal/store/routing.go` reads a webhook's config (`WebhookRoutingForHuman`) and one stored event
  scoped to the webhook (`EventForWebhook`).
* The seed packs are `docs/routing/rule-packs/fleet.json` and `pool-review.json`, with their tests
  `internal/routing/fleet_pack_test.go` and `pool_review_pack_test.go`, and fixtures under
  `internal/routing/testdata/fleet/`.

Known defects this work touches: dry-run rejects id-less candidates, faults are no-match,
and `set_webhook_rules` clears params. The last two are fixed under ADR-0031 / SPEC-0026. This
design only depends on their outcomes, and does not duplicate them.

## Goals / Non-Goals

### Goals

* Installing a pack is impossible without a replay over the webhook's own stored deliveries.
* Packs are versioned, tested and embedded, and compose without clobbering.
* The replay report answers "what would this change?" in counts and event ids.
* The seed packs are expressed as compositions that route their fixture suites identically.

### Non-Goals

* User-authored or shared packs (needs ADR-0038 ownership and a review model). Operator-supplied
  packs are in scope only as the off-by-default `SWITCHBOARD_RULE_PACK_SOURCES` opt-in (REQ-1).
* A web UI for routing.
* Changing evaluation semantics. Fault handling belongs to ADR-0031 / SPEC-0026.
* An LLM-suggested pack. ADR-0024's triage stage stays separate.
* Operator ceilings per tenant on installed packs (design review 2026-09-22).
* An operator CLI or HTTP install path. It is a P2 follow-up; v1 installs through MCP.

## Decisions

### Packs compile into rules; there is no second evaluator

**Choice**: an install writes ordinary rules into `endpoint_webhooks.routing_rules`, and records
provenance beside them.
**Rationale**: one evaluation path means the replay, the dry-run and ingest can't disagree. SPEC-0023's
`rule_id` counters work unchanged, and the counters for `<pack>--<local-id>` ids line up with the
plan's predictions.
**Alternatives considered**:
- Evaluate packs by reference at delivery time: this is a second path, and a catalog upgrade would
  change live routing without a plan.

### Two-phase install with a stored plan

**Choice**: phase one stores the candidate and its digest in a plan row and returns a token. Phase two
applies the stored candidate.
**Rationale**: applying the *stored* candidate, rather than recomputing it, guarantees that what saves
is what was replayed. The digest and config version make concurrent edits fail loudly (`plan_stale`).
**Alternatives considered**:
- Stateless signed tokens that embed the candidate: large tokens, and still single-use tracking.
- A `confirm: true` flag on one call: that is option E of the ADR again, with evidence that can be
  skipped.

### Replay is a batch over stored events through the sandbox

**Choice**: load up to `limit` events of the webhook, newest first, and evaluate the saved and candidate
configs for each through the sandbox child, in batches.
**Rationale**: the sandbox is the production router, and the memory bound matters because stored bodies
are attacker-supplied. Batching amortizes the child's start-up over many events.
**Alternatives considered**:
- In-process evaluation: faster, but it isn't the production router (SPEC-0020 requires the child for
  production evaluation), and it can't bound memory.

### Typed params with shared names

**Choice**: `params_schema` types are checked at plan time. Shared names, such as `trusted_humans`, are
declared once in a registry of shared types, and the build fails if two packs disagree.
**Rationale**: it closes the mistyped-allowlist fault at the door for pack params, while keeping the
`| arrays` guards in the rules as defence in depth.

### Provenance as a jsonb column, drift by digest

**Choice**: `endpoint_webhooks.rule_packs jsonb` holds the installed packs. Drift is computed at read
time by re-hashing each installed rule.
**Rationale**: provenance changes only under the same row lock as the rules, so one column keeps them
atomic. Hashing at read time needs no triggers.

## Architecture

```mermaid
sequenceDiagram
  participant A as agent (endpoint with install_rule_pack)
  participant M as MCP rule-pack verbs
  participant C as catalog (embedded)
  participant S as store
  participant X as sandbox child (production router)

  A->>M: install_rule_pack(webhook, name, version, params)
  M->>S: WebhookRoutingForOwner(webhook, owner scope)
  M->>C: load pack@version (schema, rules, checks, fixtures)
  M->>M: type-check params, run param_checks, merge into candidate, validate vs grant
  M->>S: last N events on this webhook (headers, body, kind, verified)
  loop batches
    M->>X: route(saved config, events) and route(candidate, events)
  end
  M->>X: route(candidate, pack fixtures)
  M->>S: INSERT rule_pack_plans(candidate, digest, config_version, report, token hash, expiry)
  M-->>A: plan (report, plan_token)
  A->>M: install_rule_pack(..., confirm=plan_token)
  M->>S: lock webhook row; check config_version and token; write rules, params, rule_packs; mark plan used
  M-->>A: installed (provenance, rules, grant)
```

### Pack file shape (`docs/routing/rule-packs/<name>/v<N>.json`)

```json
{
  "name": "drop-ci-noise",
  "version": "1.0.0",
  "title": "Drop CI and bot noise",
  "summary": "Deletions, bot chatter and non-failing CI events never become todos.",
  "placement": "prepend",
  "requires": {"source_types": ["gitea", "github"], "capabilities": [], "packs": []},
  "params_schema": {
    "bot_actors":      {"type": "string_list", "default": ["gitea-actions", "renovate-bot", "renovate"], "description": "exact sender logins"},
    "drop_deleted":    {"type": "bool", "default": true},
    "keep_ci_success": {"type": "bool", "default": false}
  },
  "param_checks": [],
  "rules": [
    {"id": "deleted", "name": "deleted objects cannot be worked",
     "expr": "($params.drop_deleted != false) and ((.payload.action // \"\") == \"deleted\")",
     "action": {"drop": true}}
  ],
  "fixtures": [
    {"name": "gitea issue_comment deleted", "source": "gitea",
     "headers": {"X-Gitea-Event": "issue_comment"}, "body_file": "fixtures/gitea-issue-comment-deleted.json",
     "verified": true, "expect": {"drop": true, "rule": "deleted"}}
  ]
}
```

The rule shown is illustrative. Each built-in's real expressions are written against captured real
deliveries in its fixture directory, and must pass its tests. REQ-9's equivalence tests assert the
compositions against each case's `want` in `internal/routing/testdata/fleet/cases.json` and the
`pool_review_pack_test.go` cases. Once they pass, `fleet.json` and `pool-review.json` are deleted in
that story, with a CHANGELOG line.

### Schema (migration `00NN_rule_packs.sql`)

```sql
-- 00NN_rule_packs — installed-pack provenance and install plans (ADR-0036, SPEC-0031).
-- Additive: an empty rule_packs list changes nothing; plans are short-lived.
ALTER TABLE endpoint_webhooks
    ADD COLUMN rule_packs     jsonb NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN config_version bigint NOT NULL DEFAULT 0;   -- bumped by every routing write

CREATE TABLE rule_pack_plans (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    token_hash     text NOT NULL UNIQUE,                    -- SHA-256 of the token; the token is shown once
    webhook_id     uuid NOT NULL REFERENCES endpoint_webhooks(id) ON DELETE CASCADE,
    owner_human_id uuid NOT NULL,                           -- owner scope; team column follows SPEC-0033
    action         text NOT NULL CHECK (action IN ('install','remove')),
    pack_name      text NOT NULL,
    pack_version   text,
    params         jsonb,
    candidate      jsonb NOT NULL,                          -- the exact routing.Config to apply
    candidate_hash text NOT NULL,
    config_version bigint NOT NULL,                         -- the webhook's version when planned
    report         jsonb NOT NULL,
    proven         boolean NOT NULL,
    truncated      boolean NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    expires_at     timestamptz NOT NULL,
    used_at        timestamptz
);
CREATE INDEX idx_rule_pack_plans_expiry ON rule_pack_plans (expires_at);
```

Every existing routing write (`set_webhook_rules`, `add`, `update`, `move`, `remove_webhook_rule`)
increments `config_version` inside its lock, so any concurrent edit invalidates outstanding plans.
Expired plans are pruned by the hourly retention task.

Provenance entry:

```json
{"name": "no-self-review", "version": "1.0.0", "rule_ids": ["no-self-review--review-request-not-for-me", "..."],
 "rules_digest": "sha256:…", "param_keys": ["identity", "pairs", "families", "require_cross_family"],
 "installed_at": "…", "installed_by": {"endpoint_id": "…", "human_id": "…"},
 "proven": true, "replay": {"events": 200, "changed": 41, "window_from": "…", "window_to": "…"}}
```

### MCP verbs

| Verb | Input | Output |
|---|---|---|
| `list_rule_packs` | `webhook_id?` | catalog entries with `source` (`builtin` or `external`); installed packs with `version`, `upgrade_available`, `proven`, `truncated`, `drift` |
| `install_rule_pack` | `webhook_id, name, version?, params?, replaces?, replay?{limit,since}, confirm?, allow_unproven?, overwrite_local_edits?` | plan (phase 1) or installed state (phase 2) |
| `remove_rule_pack` | `webhook_id, name, replay?, confirm?` | plan, or the updated state |
| `test_webhook_rules` | adds `replay?{limit,since}` | the REQ-5 report without a token |

New error codes: `plan_stale`, `pack_rule_faults`, `requires_pack`, `default_conflict`, `local_edits`,
`unknown_pack`, `unavailable_version`, `busy`. Existing codes are reused where they fit
(`invalid_params`, `too_many_rules`, `not_found`, `forbidden`).

### Replay engine (`internal/routing/replay.go`, `internal/mcp/webhook_rules.go`)

* `store.RecentEventsForWebhook(ctx, webhookID, limit, since)` returns events newest first, scoped by
  `webhook_id`.
* `routing.Replay(ctx, router, saved, candidate, grant, envs)` returns a `ReplayReport`. It routes both
  configs per envelope, tallies first-match counts from `Trace.RuleID`, collects faults from
  `Trace.Faults`, and groups the transitions.
* The sandbox child gains a batch request (many envelopes, one config) behind the same protocol prefix,
  so a 1,000-event replay is a handful of child runs, not two thousand. The per-event budget and the
  memory watchdog are unchanged. The replay's total budget (30 s) is enforced by the parent.
* The todos-per-day estimate is the non-drop count scaled by the window's time span, and it is labelled
  as an estimate.

### Composition algorithm

1. Start from the saved config. Take out the rules the target pack installed, if this is an upgrade or a
   removal, and the hand-written rules named in `replaces`, if any.
2. Group the rules: pack groups in placement order (`prepend`, then non-pack rules, then `append`),
   topologically sorted by `requires.packs`.
3. Insert the target pack's rules, renamed `<name>--<id>`.
4. Merge params: the caller's values first, then values already owned, then schema defaults. Take out
   keys owned only by a removed pack.
5. Set `default_action` if the pack declares one (refuse on conflict).
6. Validate: types, `param_checks` (in the sandbox), SPEC-0020 `Validate` against the grant, and the
   32-rule limit.

## Risks / Trade-offs

* **Replay cost** → batches, the sandbox slots and a per-webhook mutex. The 30 s budget and the
  truncated flag keep a slow replay from holding anything open.
* **Unrepresentative history** → `proven: false`, the `never_matched` flags and the window dates in the
  report. Owners can widen `since`.
* **Shared params couple packs** → the plan names every pack a param change touches, and the replay
  shows the combined effect.
* **Catalog upgrades need a release** → acceptable for a security gate. Upgrades are opt-in per
  webhook, never automatic.
* **Plans hold candidate configs** → they are owner-scoped, short-lived, pruned, and hold no secrets.
  Params are allowlists, not credentials.

## Migration Plan

1. Land the replay engine in `test_webhook_rules`, with the id-less dry-run fix. It is useful on its own.
2. Land the catalog format, the embedding, and the four v1 packs with equivalence tests against
   `fleet.json` and `pool-review.json`.
3. Land the provenance column, `config_version`, the plan table and the three verbs.
4. Migrate the fleet webhooks by adopting their pasted-in rules. On each router webhook, plan
   `trusted-actors@1` with `replaces` naming the four hand-installed gate rules, then `handoff-lanes@1`
   with `replaces` naming the rest. On each pool webhook, plan `no-self-review@1` and `drop-ci-noise@1`
   the same way. Each step stays within the 32-rule limit, keeps the webhook's params (adopted, not
   reset), and must show `changed: 0` before it is confirmed. That is a live proof that the split is
   behaviour-preserving. `overwrite_local_edits` plays no part, because hand-installed rules carry no
   provenance.
5. Ship `trusted-actors@2` with SPEC-0026, retiring `trusted-actors@1` from the catalog in the same
   release (REQ-9), with an upgrade note.

Rollback: packs are ordinary rules, so removing the verbs leaves every installed webhook routing
exactly as it did. The provenance column is inert.

## Open Questions

* **Should the operator CLI and API gain a `webhook pack plan|install` path?** Resolved (design review 2026-09-22): deferred to a P2
  follow-up story, once ADR-0038's operator-API owner-scope shape is implemented. v1 installs through
  MCP only.
* **Operator ceilings per tenant on installed packs.** Resolved (design review 2026-09-22): out of scope for this program.
* **Should replay predict the decision counters and a later job compare them?** Resolved (design review 2026-09-22): not in v1.
* **Can a future user-pack feature reuse this format, owned per ADR-0038?** Resolved (design review 2026-09-22): out of scope here.
  The format is kept compatible with it, and user packs need their own ADR.
