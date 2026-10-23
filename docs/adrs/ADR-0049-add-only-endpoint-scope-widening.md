---
status: proposed
date: 2026-10-21
decision-makers: Proposed by joestump-agent, pending Joe
extends: [ADR-0008, ADR-0012]
related: [ADR-0022, ADR-0023, ADR-0042, ADR-0043, ADR-0047]
---

# ADR-0049: Add-Only Endpoint Scope Widening — One More Lane Should Not Cost a Re-Vend

**Status note:** this ADR is **proposed**. It ships with an implementation so the choice can be argued
against working code rather than prose; nothing here is settled until Joe accepts it. The requirement
text it implies for SPEC-0006 and SPEC-0007 is listed under Consequences rather than applied: accepted
records stay as accepted until this ADR is accepted.

## Context and Problem Statement

An endpoint's scope is fixed at vend (ADR-0008), and the only way to change it has been
`switchboard endpoint revoke` followed by a fresh `endpoint vend`. For a change that *removes* something
that is defensible: the credential is what the grant is computed from, so dropping a queue or a verb
should be a decision with a reveal attached.

For a change that only *adds*, re-vending is worse than the change it is meant to make. A webhook belongs
to the endpoint that created it, so a re-vend mints a new ingest URL and a new signing secret, and the
producer — a repository's hook configuration, a form's automation, a partner's relay — has to be edited
to point at them. Meanwhile the old endpoint's todo history, its in-flight claims and its routes go with
it. Issue #590 is precisely that report: a lane that needed one more queue in its `webhook_queues` got a
403 `not_granted` on the delivery, and the documented answer was to tear the endpoint down and vend it
again.

The asymmetry is worth naming exactly. A widening cannot break a route that works today: every additive
change leaves the union that routing already computes strictly larger. A narrowing can. So the gap is not
"make scope editable" but "give the safe half of scope editing a path".

## Decision Drivers

* The credential must not change. A widen that rotated the token would move the problem from the
  producer's hook configuration into the lane's env file, which is the failure this exists to remove.
* Additive must be **structural**, not a matter of passing the right flag. If a request body can express
  a removal, then eventually a removal happens through it.
* An authorization change that lands silently is the thing a reviewer is trying to prevent. Every field
  that moves needs a record, and the record cannot be allowed to drift from the change.
* Ownership and tenant isolation cannot loosen on the way: the same reach rules as every other
  human-facing endpoint write (ADR-0022), `state = 'active'` in the predicate.
* One command with the confirm-before-write shape the rest of the CLI already uses.

## Considered Options

* **(A) Re-vend, documented harder.** Add a guide section walking the revoke/vend/re-point dance. Costs
  the producer a config edit and loses the endpoint's history, which is the part operators actually miss.
* **(B) General scope PATCH.** Let `PATCH /api/v1/endpoints/{ref}` take scope fields, additively or not.
  One surface for everything, but it puts a silent narrowing — a verb-list rewrite — behind the same call
  that edits a lease TTL.
* **(C) Add-only widen.** A dedicated route and command that can only union arrays and raise the webhook
  ceiling, refuses any key that could remove, records the before and after of every field it moves in the
  same transaction, and never touches the credential.

## Decision Outcome

Chosen: **(C)**, add-only widen.

1. `POST /api/v1/endpoints/{ref}/scope/widen` accepts `add_webhook_queues`, `add_webhook_source_types`,
   `add_scope_verbs` and `webhook_max` (a ceiling to raise, never lower). Any other key — including the
   bare `webhook_queues` a caller might guess — is refused with 400 by a decoder that rejects unknown
   fields, so "replace this list" has no spelling.
2. Without `confirm: true` the call is a **preview**: it reports `before`, `after`, which fields would
   move, and `requires_confirmation: true`, and writes nothing. With `confirm: true` it applies. The CLI
   is the same two calls — preview, print the diff, one `[y/N]` prompt, confirm — and `--yes` skips the
   prompt for scripts.
3. Widening `webhook_queues` needs **no routing change**. The grant answering "may this endpoint receive
   on queue Q" is already a union computed over the owner's active endpoints, so the widened value is
   honored on the next read. That is the whole mechanism's claim to safety, and the regression test
   asserts the reported 403 through the routing decision rather than against a row.
4. `webhook_max` refuses a decrease with `ErrScopeNarrowing` → 400. Raising a ceiling is additive;
   lowering one strands webhooks that already exist.
5. Re-adding a value the endpoint already has is a **successful no-op that writes no audit row**: idempotent
   scripts stay quiet and the trail stays a record of changes.
6. `GET /api/v1/endpoints/{ref}/scope/widenings` returns the trail newest-first — field, before, after,
   timestamp — for an endpoint the caller owns, with `switchboard endpoint scope history SLUG` as its
   command, so SPEC-0035's CLI parity holds.
7. `switchboard endpoint scope widen SLUG|ID --add-queue a,b [--add-source-type …] [--add-verb …]
   [--webhook-max N] [--yes] [--json]`, with `switchboard endpoint scope add-queue SLUG|ID QUEUE` as the
   one-flag form for the case the report describes.

## Mechanics

* `internal/db/migrations/0034_endpoint_scope_widenings.sql` adds `endpoint_scope_widenings(endpoint_id,
  actor_human_id, field, before, after, created_at)`, with `field` constrained to the four widen-able
  fields and `endpoint_id` cascading on delete. There was no table to put it in: a revoke's audit trail
  *is* the terminal endpoint row, and `events` is ingest data, so an authorization delta needed its own.
* `internal/store/endpoint_scope.go` locks the row with `SELECT … FOR UPDATE` under the ownership and
  `state = 'active'` predicate, unions the arrays, applies one `UPDATE`, and inserts one audit row per
  field that actually moved — all in one transaction, so a change and its record cannot disagree. Naming
  nothing to add, or only values the endpoint already has, returns `ErrNothingToWiden` before any write.
  The reserved `quarantine` queue is refused, as everywhere else a queue name enters the system.
* Verbs are resolved with the same resolver the vend path and the consent screen use (ADR-0023,
  ADR-0047), so a widen cannot name a verb the vend path would reject, and notify-hook verbs (ADR-0029)
  stay name-only there. Ingest source types are validated against the server's own set.
* A widen reads no credential and writes none. The token that authenticates the endpoint before the call
  authenticates it afterwards, which the store test asserts through the credential lookup itself.

### Consequences

* Not breaking. Nothing that works today changes, and no new flag is mandatory. The vend path, its
  defaults and the web wizard are untouched.
* **`scope_queues` is deliberately out of scope.** Those are the queues an endpoint may *claim*; adding
  one changes which work the lane can take, which is not an additive change to a route. A lane that needs
  a second queue to drain is a second endpoint, or a re-vend — on purpose.
* **The web UI is a follow-up**, not part of this slice: the widen form and the vend wizard's copy want
  the same framing ("one more lane is a widen, not a re-vend"), and shipping the API and CLI first lets
  the shape be argued before the form is built. Until it exists, the CLI is how you widen.
* Widening is not a way to hand administration to a lane that never had it. A lane needs its verbs named
  at vend (ADR-0047); this is for the lane whose *routes* outgrew its grant.
* Implied requirement text, for SPEC-0006 "Webhook Self-Management Within a Vended Ceiling" and
  SPEC-0007 once this ADR is accepted: an operator MAY widen, and only widen, an active endpoint they own;
  a request that would remove a granted value MUST be refused; every field a widen moves MUST be recorded
  with its before and after in the same transaction as the change; and a widen MUST NOT alter the
  endpoint's credential.

### Confirmation

`internal/store/endpoint_scope_test.go` pins the union, the reported 403 from the issue closing through
the routing grant, the idempotent no-op that writes no history row, the refusal of a narrowing
`webhook_max`, ownership and revoked-endpoint reach, and that the credential still resolves the same
endpoint afterwards; `internal/server/api_endpoint_scope_test.go` pins preview-then-confirm, that a second
widen keeps the first queue, the refusal matrix (a removal key, a replacement key, an empty body, an
unknown source type or verb, a negative and a lowering `webhook_max`), and 404/409 reach;
`cmd/switchboard/endpoint_scope_test.go` pins that declining the prompt calls the API once and writes
nothing, that `--yes` confirms, and that a usage error never reaches the API.
