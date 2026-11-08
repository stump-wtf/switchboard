---
status: proposed
date: 2026-11-05
decision-makers: Proposed by joestump-agent, pending Joe
extends: [ADR-0025, ADR-0036]
related: [ADR-0022, ADR-0024, SPEC-0020, SPEC-0001]
---

# ADR-0058: A Cairn Delivery Is Lane Work, Never Pool Work

**Status note:** this ADR is **proposed**. The rule and its tests ship with it so the choice can be
argued against working routing rather than prose. [Issue #439](https://gitea.stump.rocks/stump.wtf/switchboard/issues/439)
and the epic it belongs to (stumpcloud/stumpcloud#466) decide *that* an identity pool must not act on a
Cairn handoff (decision D5, handoffs dispatched to lanes only); nothing there says *how* a pool learns
that, and this record decides the mechanism.

## Context and Problem Statement

Both identity pools subscribe to the same org events, and the Cairn webhook's deliveries fan out to the
router **and** to both pool endpoints. Over 09-13 → 09-19, 24 of 59 handoffs were worked in full by both
pools: one artifact, two workers, two pull requests. Phase 0 of the epic fixed the two forge-side classes
the same way (own echo, assignment-as-dispatch) with rules 6 and 7 of `docs/routing/rule-packs/pool-review.json`,
and left this one open.

What the code actually does, verified on `main` rather than inferred from the issue:

- A delivery is routed **once**, by the rules of the webhook that received it (`internal/ingest/routing.go:48`),
  and one decision mints a todo on every target it selects (`internal/ingest/selfmanaged.go:342`). Rules
  belonging to a fan-out target's own endpoint are never consulted on that path.
- So a rule in `pool-review.json` cannot intercept a delivery received on the **Cairn** webhook. That
  webhook's rules are `fleet.json`, whose cairn actions are `exclusive`, and the pool endpoints' scope
  holds no lane queue.
- Removing the two pool routes from the Cairn webhook is the change that stops today's fan-out, and it is
  an operator action (`remove_webhook_route`). Per ADR-0036 a pack file is the source of what an operator
  installs; it never installs anything itself.

A rule that matches nothing looks exactly like one that works, which is why this record states plainly
what the rule does and does not do: it is the declarative, reviewable statement of D5 on the pool side,
and it is the guard that keeps a handoff out of a pool the day a pool owns a cairn webhook or someone
re-adds a route. It is not a substitute for removing the routes.

## Decision

1. **`pool-review.json` gains rule 8, `cairn-is-not-pool-work`, and it drops.** A pool webhook is a
   forge-review hook; no cairn event carries a review signal, so every cairn delivery on one is either a
   pack installed on the wrong hook or a route nobody meant to keep. Both readings have the same safe
   answer: no todo.
2. **It keys on the envelope's `.source`, nothing else.** `.source` is the receiving webhook's source
   type, written by Switchboard, and a producer cannot place it (SPEC-0020 REQ "Isolation and Tenant
   Safety"). Pool webhooks are token-trust generic hooks: anyone who can post to one can write
   `"source": "cairn"` and a cairn-shaped body, and that must not let the payload choose its own routing
   — still less empty a pool's queue by imitating a source.
3. **It drops every cairn delivery on a pool, not only those tagged `handoff`.** The narrower form was
   considered: a pool that receives a non-handoff artifact has no more to do with it than with a handoff,
   and a rule that has to read tags is a rule that changes when cairn changes its tag vocabulary.
4. **The installation is two steps, and both are operator steps.** Install the pack on both pool webhooks,
   and remove both pool routes from the Cairn webhook. Neither alone is enough: the routes let a handoff
   reach a pool without the pool's rules ever running, and the routes alone leave a pool-owned cairn hook
   free to make pool work of a handoff.

## Considered Options

- **Routes removed only.** Correct for today, invisible in review: nothing in the repo would say that a
  pool must not work a handoff, and a re-added route would silently restore the double-build.
- **A rule on the Cairn webhook that excludes the pool endpoints.** An action's `endpoints` are live
  endpoint ids, so the pack would carry deployment state and stop being installable anywhere else
  (ADR-0036).
- **Keying on `.artifact != null` or on `.payload` tags.** Both reach producer-shaped data where `.source`
  already says what is needed, and both would need a second rule for artifacts cairn has not tagged.
- **A new `cairn_*` rule kind in the pack schema.** Rule kinds are `queue` / `drop` / `quarantine`; adding
  one for a single-source drop would widen the schema, its validation, and the sandbox, for what one jq
  equality expresses.

## Consequences

- `pool_review_pack_test.go` gains `TestPoolReviewPackDropsCairnDeliveries`: handoff artifacts dropped on
  both pools and with no `identity` param, a handoff pinned to another lane dropped here too, an
  untagged artifact dropped, and two cases pinning that a producer-written cairn shape on a generic hook
  still routes. Five of them fail without rule 8.
- If a pool ever legitimately needs a cairn delivery, rule 8 has to be narrowed on purpose, with a test
  for what it then admits. Until then a pool waking on a handoff is a wiring fault the rule hides, so the
  PR body and the pack README both name the operator step it does not replace.
- No routing, storage, or trust code changes: this is rule-pack data, its tests, and the record of why.
