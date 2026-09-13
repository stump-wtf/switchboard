---
status: proposed
date: 2026-09-13
decision-makers: [joestump]
governs: [SPEC-0011]
related: [ADR-0007, ADR-0008, ADR-0013, ADR-0022, ADR-0023, ADR-0025]
---

# ADR-0026: Operator-Authored Todos Ring the Doorbell

## Context and Problem Statement

Every todo in switchboard is born from a delivery: a webhook verified per source
([ADR-0003](ADR-0003-per-provider-ingestion-and-trust-model.md)), a queue message, or a work order
routed out of one of those ([ADR-0025](ADR-0025-handoff-work-orders-and-difficulty-lanes.md)). The
human who vended the endpoint ([ADR-0008](ADR-0008-human-principal-vended-endpoints.md)) has no way
to hand that endpoint a todo directly. The only operator-side write is `POST /dev/todos`, which is
dev-mode only and — by design — never rings the doorbell: the sender gate
([ADR-0013](ADR-0013-channels-push-delivery.md), SPEC-0011) pushes only todos persisted together
with a verified delivery event, and a dev todo has none.

In daily operation the missing verb is the one a human reaches for most: *"look at PR 7"*, *"the
build on main is red, find out why"*, *"re-run the morning brief"*. Today the workaround is to
POST to the endpoint's own token-trust ingest URL, pretending to be a producer — which persists the
hand-off as an anonymous, unverified generic delivery, attributes it to nobody, and shows up on the
board as exactly the kind of untrusted event the trust model exists to flag. A human standing in
front of their own agent should not have to impersonate a webhook to be heard.

## Decision Drivers

* **Provenance, not text, makes work eligible** ([ADR-0025](ADR-0025-handoff-work-orders-and-difficulty-lanes.md)).
  The sender gate's purpose is attribution: a push must be traceable to a verified sender and an
  accountable human. An operator authenticated over the operator API's OAuth grant
  ([ADR-0023](ADR-0023-mvp-mcp-api-first-basics.md)) *is* that human — the strongest provenance
  switchboard ever has.
* **Semi-trust is not permission.** The todo's title and payload are still text the agent reads;
  they must be handled exactly as any other todo's content — never as instructions that widen what
  the worker may do — and rendered through the same injection-safe doorbell path.
* **Least privilege still holds** ([ADR-0022](ADR-0022-endpoint-scoped-todo-ownership.md)). An
  operator may only hand work to endpoints they own, and only onto queues inside the endpoint's
  vended scope. Nothing here widens an endpoint.
* **One ledger, one shape.** The board, the sweep, the event history and dedup all key off the
  delivery event. An operator hand-off must be an ordinary event with an ordinary trust mode, not
  a special-cased todo that every downstream reader has to learn about.
* **The API is the product surface** ([ADR-0023](ADR-0023-mvp-mcp-api-first-basics.md)). The verb
  belongs on `/api/v1` and the CLI, not behind the web UI.

## Considered Options

* **(A) Keep using the ingest URL.** *Rejected:* it records a human hand-off as an anonymous,
  unverified delivery, and the trust model then correctly distrusts it.
* **(B) Ring the doorbell for dev todos.** *Rejected:* dev mode is unauthenticated by definition,
  which is the one sender the gate must never admit.
* **(C) An MCP verb (`create_todo`) on the vended endpoint.** *Rejected:* the principal on that
  surface is the agent, not the human; an agent minting work for itself is a loop, not a hand-off,
  and it is exactly the self-authorisation ADR-0025 forbids.
* **(D) An operator-API route that mints a verified `operator` delivery event and its todo, ringing
  the doorbell through the existing sender gate.** *(chosen)*

## Decision Outcome

Chosen option: **(D)**.

`POST /api/v1/endpoints/{ref}/todos` — under the same operator OAuth guard as every other
`/api/v1` route — mints one todo on an endpoint the caller owns and rings its doorbell. It does so
by recording a delivery event with `source = operator`, `family = operator`, `trust_mode =
operator`, `verified = true`, and a `verify_detail` naming the human, then fanning it out through
`CreateEventTodos` exactly as an ingest receiver does. Nothing downstream is special-cased: the
store's doorbell hook fires because the event is verified; the heartbeat sweep and the pull path
apply their existing predicates; the board shows an `operator` trust badge; event history shows who
handed the work over.

* **Scope.** The endpoint must belong to the caller (unknown and foreign endpoints answer the
  same not-found, as revoke does), must be active (409 otherwise), and the queue must be one of the
  endpoint's vended queues (400 otherwise; an endpoint with exactly one queue needs no `queue`).
* **Dedup.** An optional `key` makes the push idempotent: it is scoped to the endpoint, so
  repeating it returns the existing live todo with `created: false` and rings nothing. Without a
  key every push is a new todo.
* **Content.** `title` is required and bounded; `payload` is optional JSON; `kind` defaults to
  `operator`. All three reach the agent as untrusted content, through the same `</channel>`
  neutralisation every doorbell gets.
* **CLI.** `switchboard todo push ENDPOINT "title" [--queue Q] [--kind K] [--payload JSON|@file]
  [--key K]` wraps the route.

This amends [ADR-0013](ADR-0013-channels-push-delivery.md)'s statement of the sender gate — *only
verified, human-attributed todos are ever pushed* — by naming the operator as a verified sender.
It does not change the gate's mechanism.

### Consequences

* Good, because a human can hand their own agent a todo through the front door, with their name on
  it, and the agent is rung the same way it is rung for any verified delivery.
* Good, because the trust model gets *more* honest: an operator hand-off no longer masquerades as
  an anonymous generic delivery.
* Good, because nothing downstream learns a new shape — the sweep, the board, history and dedup all
  see an ordinary verified event.
* Bad, because `operator` is a new trust mode and a new event family that every enumeration of
  those values (badges, filters, docs) has to carry.
* Bad, because an operator's OAuth grant now authorises minting work, so a leaked operator token
  can ring agents — the same blast radius it already had for vending endpoints, and the same
  remedy (`logout`; the grant expires on its own schedule).

### Confirmation

SPEC-0011 gains the scenario "Operator-authored todo is pushed". Tests assert: a push by the owner
mints a todo pinned to the endpoint, backed by a verified `operator` event, and fires the doorbell
hook exactly once; a repeated `key` returns the same todo and rings nothing; a foreign or unknown
endpoint is not-found; an out-of-scope queue, an empty title and a non-JSON payload are 400; a
revoked endpoint is 409; no bearer is 401. The CLI tests cover the verb's argument shape, its
rendered output, and `--json`.
