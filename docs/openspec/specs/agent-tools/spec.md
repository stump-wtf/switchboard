---
status: implemented
date: 2026-07-06
implements: [ADR-0012, ADR-0005]
requires: [SPEC-0005, SPEC-0003]
---

# SPEC-0006: Agent-Facing MCP Tool Set

## Overview

Switchboard vends a scoped MCP/HTTP endpoint to each agent
([ADR-0012](../../../adrs/ADR-0012-agents-self-manage-webhooks.md),
[ADR-0005](../../../adrs/ADR-0005-mcp-tool-and-resource-contract.md)). This capability defines the
agent-facing *work* surface exposed on that endpoint — the verbs an agent actually uses to do its
job: draining the durable todo queue (`list_todos`, `claim`, `complete`, `fail`, plus lease
`heartbeat`), self-managing its own ingestion sources within a human-vended ceiling
(`create_webhook`, `list_webhooks`, `rotate_webhook`, `delete_webhook`), and deciding which
endpoints those sources deliver to (`add_webhook_route`, `list_webhook_routes`,
`remove_webhook_route`), and routing each delivery with jq rules (`list_webhook_rules`,
`set_webhook_rules`, `add_webhook_rule`, `update_webhook_rule`, `move_webhook_rule`,
`remove_webhook_rule`, `test_webhook_rules`; [SPEC-0020](../event-routing/spec.md)).

Every request is authenticated by a bearer credential that resolves to an endpoint and its immutable
scope (`internal/agentapi/agentapi.go`). Scope is enforced at the boundary: a verb outside the
endpoint's `verbs` allowlist, or a queue outside its `queues` grant, is refused with `forbidden`
before any state changes. Webhook self-management operates strictly within a per-endpoint ceiling
(max count, allowed source types, allowed target queues); switchboard mints and **holds** the signing
secret (server-side, so it can HMAC-verify inbound deliveries per SPEC-0003) and owns verification and
idempotency. For a signed-type webhook the secret is revealed to the agent exactly once, at
create/rotate, for the agent to configure the producer; every later read returns only the ingest URL,
never the secret. This surface is distinct from, but complements, the shared read-only event-history contract
of [SPEC-0005](../mcp-tools/spec.md); it depends on that contract's shapes/errors and on the trust
model of [SPEC-0003](../../../adrs/ADR-0003-per-provider-ingestion-and-trust-model.md).

## Requirements

### Requirement: Bearer-Scoped Endpoint Authentication

Every request to the vended agent surface MUST carry a bearer credential in the `Authorization`
header. A missing credential MUST be rejected with `unauthenticated` (HTTP 401). A credential that
does not resolve to a live endpoint (revoked or unknown) MUST be rejected with `unauthenticated`. The
resolved endpoint's scope (`queues`, `verbs`) MUST be treated as immutable for the life of the
request; the agent MUST NOT be able to widen its own scope through any verb.

#### Scenario: Missing credential is rejected

- **WHEN** a request arrives with no bearer credential
- **THEN** the server MUST respond `unauthenticated` and MUST NOT perform any read or mutation

#### Scenario: Revoked credential is rejected

- **WHEN** a request presents a credential whose endpoint has been revoked
- **THEN** the server MUST respond `unauthenticated`

### Requirement: Scope Enforcement at the Boundary

Before executing any verb, the server MUST verify the verb is in the endpoint's allowlist and, for
todo-targeting verbs, that the todo's queue is in the endpoint's granted queues. The one exception is
the **self verbs** `clock_in`, `clock_out` and `presence`
([SPEC-0022](../endpoint-presence/spec.md) REQ "Self Verbs Are Unscoped"). They are allowed for every
authenticated endpoint without appearing in its allowlist, because they act only on the caller's own
presence and take no identifier that could address anything else. A verb outside the
allowlist MUST be refused with `forbidden`. A queue filter or a target todo whose queue is outside
the grant MUST be refused with `forbidden`. These checks MUST occur before any state-changing
operation.

#### Scenario: Verb not in allowlist

- **WHEN** an endpoint whose allowlist lacks `complete` calls `complete`
- **THEN** the server MUST respond `forbidden` and MUST NOT transition the todo

#### Scenario: Self verb outside the allowlist

- **WHEN** an endpoint whose allowlist lacks `clock_out` calls `clock_out`
- **THEN** the call succeeds and changes only that endpoint's presence

#### Scenario: Todo outside granted queues

- **WHEN** an endpoint calls `claim` on a todo whose queue is not in the endpoint's granted queues
- **THEN** the server MUST respond `forbidden` and MUST NOT claim the todo

### Requirement: Todo Drain Verbs

The surface MUST expose `list_todos`, `claim`, `complete`, and `fail`. `list_todos` MUST return
compact todo rows filtered to the endpoint's granted queues (optionally narrowed by a `queue` and/or
`state` filter within scope) with a bounded `limit` (default 50). `claim` MUST atomically transition
a `pending` todo to `claimed`, acquiring a lease with a caller-supplied or default TTL, and MUST fail
with `conflict` if the todo is not in the expected claimable state. `complete` MUST transition a
todo the endpoint holds to `done`. `fail` MUST park a leased todo in `failed` with a scheduled
backoff retry window if attempts remain — it re-enters `pending` when the window elapses, per
[SPEC-0003](../todo-queue/spec.md) Bounded Retries — otherwise dead-letter it to `failed` with no
window. The todo owner MUST be recorded as the acting agent
identity (`agent:<agent_id>`). A todo returned or transitioned MUST carry at minimum `id`, `queue`,
`state`, and `attempt`.

#### Scenario: Claim races resolve to one winner

- **WHEN** two endpoints call `claim` on the same `pending` todo concurrently
- **THEN** exactly one MUST succeed (todo → `claimed`) and the other MUST receive `conflict`

#### Scenario: Fail retries until attempts are exhausted

- **WHEN** `fail` is called on a leased todo and retry attempts remain
- **THEN** the todo MUST park in `failed` with a scheduled retry window (`next_retry_at`) and
  return to `pending` once the window elapses; when no attempts remain it MUST transition to
  `failed` with no retry window

#### Scenario: List is confined to granted queues

- **WHEN** `list_todos` is called with no `queue` filter
- **THEN** it MUST return only todos in the endpoint's granted queues

### Requirement: Lease Lifecycle and Crash Safety

`claim` MUST acquire a time-bounded lease. The surface SHOULD expose `heartbeat` to extend a lease
the endpoint holds and MAY expose `release` to voluntarily requeue. A todo whose lease expires
without completion MUST be reclaimable — a background reaper MUST requeue expired-lease todos (or
dead-letter them when attempts are exhausted) so that a crashed agent does not strand work. Lease
extension and completion MUST be permitted only to the endpoint that holds the lease.

#### Scenario: Expired lease is requeued

- **WHEN** an agent claims a todo and then crashes without completing it, and the lease TTL elapses
- **THEN** the reaper MUST return the todo to `pending` (or `failed` if attempts are exhausted) so
  another agent can claim it

#### Scenario: Heartbeat extends only the holder's lease

- **WHEN** an endpoint that does not hold a todo's lease calls `heartbeat` on it
- **THEN** the server MUST refuse it (`forbidden` or `conflict`) and MUST NOT extend the lease

### Requirement: Webhook Self-Management Within a Vended Ceiling

The surface MUST expose `create_webhook`, `list_webhooks`, `rotate_webhook`, and `delete_webhook`,
bounded by the endpoint's ceiling: maximum webhook count, allowed source types, and allowed target
queues ([ADR-0012](../../../adrs/ADR-0012-agents-self-manage-webhooks.md)). `create_webhook` MUST be
refused with `ceiling_exceeded` when it would exceed the max count, `forbidden_source_type` for a
disallowed source type, and `forbidden` for a target queue outside the grant. `create_webhook` and
`rotate_webhook` MUST return the ingest URL (and `trust_mode`); for a signed-type webhook they MUST
also reveal the minted signing secret exactly once (for the agent to configure the producer), and
MUST NOT return it on any later call. `list_webhooks` MUST return webhook metadata plus the ceiling
(max, allowed source types, allowed queues, used) and MUST NOT return secret values. `delete_webhook`
MUST tear down the webhook.

The trust mode MUST be derived by switchboard from the source type, never supplied by the agent.
`github`, `gitea`, `stripe`, `slack`, and `cairn` are `signed`: switchboard mints and holds an HMAC
secret, and cairn deliveries additionally carry signed replay defenses
([SPEC-0001](../webhook-ingestion/spec.md) REQ "Cairn Signed Deliveries"). `generic` is `token`.

#### Scenario: Create beyond the count ceiling is refused

- **WHEN** an endpoint at its max webhook count calls `create_webhook`
- **THEN** the server MUST respond `ceiling_exceeded` and MUST NOT create a webhook

#### Scenario: Disallowed source type is refused

- **WHEN** an endpoint whose ceiling allows only `github`/`generic` calls `create_webhook` with
  source type `stripe`
- **THEN** the server MUST respond `forbidden_source_type` and MUST NOT create a webhook

### Requirement: Webhook Route Fan-Out Under Ownership and Friendship

A webhook's deliveries always mint a todo owned by the webhook's **owning endpoint**; a *route* adds
further target endpoints, and the receiver mints one endpoint-owned todo per target
([ADR-0022](../../../adrs/ADR-0022-endpoint-scoped-todo-ownership.md),
[SPEC-0001](../webhook-ingestion/spec.md) REQ "Deterministic Route Fan-Out (Token-Free)"). The
surface MUST expose `add_webhook_route`, `list_webhook_routes`, and `remove_webhook_route` to manage
that target set. Routing MUST name an explicit target **endpoint id** — never a queue name — so two
tenants sharing a queue string can never acquire visibility into each other's work.

All three verbs MUST require that the caller **owns the webhook**: the webhook's owning endpoint MUST
be the calling endpoint itself (`endpoint_webhooks.endpoint_id`). Another endpoint of the same human
does not own it: a friend endpoint is vended on its approver's agent, and one endpoint's credential
must not reach every webhook its human owns ([SPEC-0033](../teams-tenancy/spec.md) REQ "Closing the
Audited Surfaces", F3 and F19). A webhook id that is unknown, malformed, or owned by any other
endpoint MUST be refused with `not_found`, and the refusals MUST be indistinguishable from one
another, so a routing verb cannot confirm the existence of a webhook the caller does not own.

`add_webhook_route` MUST additionally authorize the **target**:

- A target endpoint owned by the **caller's own human** MUST be allowed with no further condition.
- A target endpoint owned by **another human** MUST require an approved friend edge in the
  **delivering direction** — `friend_edges` with `from_human` = the caller's human, `to_human` = the
  target's human, and `state = 'approved'`
  ([ADR-0010](../../../adrs/ADR-0010-a2a-discovery-human-vended-friending.md),
  [SPEC-0010](../friending/spec.md) REQ "Per-Direction, Revocable, Non-Transitive Edges"). This is
  the same direction `create_for` relies on: the target human's approval is what consents to
  receiving the requester's work. An edge that is absent, `pending`, `denied`, `revoked`, or
  recorded only in the **opposite** direction MUST NOT authorize the route; honoring the opposite
  direction would be a privilege escalation.
- A target endpoint that is unknown, malformed, or revoked MUST be refused.

Every target-authorization failure — unknown id, revoked endpoint, or an unfriended human — MUST
return the **same** `forbidden` code and message, so the verb cannot be used to enumerate which
endpoint ids exist or which humans the caller is friends with.

`add_webhook_route` and `remove_webhook_route` MUST be idempotent: they state an end condition, so a
repeat call MUST succeed rather than return `conflict`, and MUST leave exactly one (or zero) route.
The webhook's owning endpoint is an implicit, unremovable target — `remove_webhook_route` against it
MUST be a no-op, and `list_webhook_routes` MUST report it alongside the explicit routes so the
reported fan-out set matches where deliveries actually land. `list_webhook_routes` and
`remove_webhook_route` MUST require webhook ownership **only**, not target authorization, so a route
whose friendship was later revoked remains visible and removable by the webhook's owner.

#### Scenario: Agent routes its own webhook to its own second endpoint

- **WHEN** an agent calls `add_webhook_route` from the endpoint that owns the webhook, naming a
  target endpoint the same human owns
- **THEN** the route MUST be recorded with no friend edge required, `list_webhook_routes` MUST report
  it alongside the owning endpoint, and a subsequent delivery MUST mint one todo per target, each
  pinned to its own endpoint

#### Scenario: Routing to another human's endpoint without an approved edge is refused

- **WHEN** an agent calls `add_webhook_route` targeting an endpoint owned by another human, and no
  `approved` friend edge runs from the caller's human to that human
- **THEN** the server MUST respond `forbidden` and MUST NOT record a route — whether the edge is
  absent, `pending`, `denied`, or `revoked`

#### Scenario: A friend edge in the opposite direction does not authorize delivery

- **WHEN** human B holds an `approved` edge authorizing B to hand work to human A, and A calls
  `add_webhook_route` targeting an endpoint owned by B
- **THEN** the server MUST respond `forbidden` and MUST NOT record a route; only an edge from A to B
  authorizes A's deliveries into B

#### Scenario: Routing a webhook the caller does not own leaks nothing

- **WHEN** an agent calls any routing verb naming a webhook owned by another endpoint (of another
  human or of its own), and separately names a webhook id that does not exist
- **THEN** both MUST be refused with an identical `not_found` response, and no route MUST be recorded

#### Scenario: Adding the same route twice is idempotent

- **WHEN** `add_webhook_route` is called twice with the same webhook and target
- **THEN** both calls MUST succeed and exactly one route MUST exist; the resolved target set MUST
  contain the target once

### Requirement: Webhook Routing Rules

The surface MUST expose these verbs as members of the webhook verb family, so they are enumerated by
the vend and consent screens with the rest of that family:

- `list_webhook_rules`
- `set_webhook_rules`
- `add_webhook_rule`
- `update_webhook_rule`
- `move_webhook_rule`
- `remove_webhook_rule`
- `test_webhook_rules`

Each verb MUST be gated by the endpoint's verb allowlist (`forbidden` outside it) and by **endpoint**
ownership of the webhook: only the webhook's own endpoint may call them. The ownership check follows
the route verbs: unknown, malformed, and any other endpoint's webhook ids (including a sibling
endpoint of the same human) MUST all return an identical `not_found`.

Saving rules MUST validate the whole list against the webhook's grant: its target queue, its owner's
allowed webhook queues (as defined in [SPEC-0020](../event-routing/spec.md) REQ "Rule Validation at
Save Time"), and its live delivery targets. A failed validation MUST leave the
previous list in force. A rule MUST only ever narrow a delivery to targets the webhook already has;
adding a target remains `add_webhook_route`'s job.

`test_webhook_rules` MUST NOT persist anything. It MUST reach stored events only on the named
webhook.

Rule semantics, error codes, and the dry-run contract are specified in
[SPEC-0020](../event-routing/spec.md) REQ "Rule Management Tools" and REQ "Routing Dry-Run".

#### Scenario: Rule verbs outside the allowlist are refused

- **WHEN** an endpoint whose allowlist lacks `set_webhook_rules` calls it
- **THEN** the server MUST respond `forbidden` and MUST NOT change any rule

#### Scenario: A rule cannot reach another human's endpoint

- **WHEN** an agent saves a rule on its own webhook whose action names an endpoint owned by another
  human
- **THEN** the server MUST respond `forbidden` and MUST NOT change the rule list

### Requirement: Switchboard Owns Secrets, Verification, and Idempotency

For any self-created webhook, switchboard MUST mint the signing secret and **hold** it server-side in
PostgreSQL — the plaintext, not a hash, because switchboard must recompute the provider HMAC over each
inbound body to verify it. Switchboard reveals the secret to the agent **exactly once**, in the
`create_webhook`/`rotate_webhook` result for a signed-type webhook, so the agent can configure the
producer; it MUST NOT return the secret on any later call (e.g. `list_webhooks`). A self-created
signed-type webhook MUST be verified exactly as
[SPEC-0003](../../../adrs/ADR-0003-per-provider-ingestion-and-trust-model.md) mandates: switchboard
recomputes the provider HMAC-SHA256 over the raw body against the held secret in constant time, and on
a valid signature persists the delivery as `verified=true` under `trust_mode=signed`, while a
missing or invalid signature is rejected and nothing is
persisted. Switchboard — not the agent — owns the trust mode: the agent MUST NOT be able to downgrade
the trust mode, disable signature checks, or otherwise alter how a delivery is verified, and
switchboard MUST NOT report a delivery as `verified` unless it verified the body signature per
SPEC-0003. Duplicate deliveries to a self-created webhook MUST dedup into a single todo on the
idempotency key. `rotate_webhook` MUST mint a new secret and retire the old one.

#### Scenario: Secret is revealed exactly once to the agent

- **WHEN** an agent calls `create_webhook` or `rotate_webhook` for a signed-type webhook
- **THEN** the response MUST contain the ingest URL and the minted signing secret revealed once; the
  secret MUST be held server-side and MUST NOT be returned again on any later call

#### Scenario: Self-created signed webhook is verified per SPEC-0003

- **WHEN** a delivery arrives at a self-created signed-type webhook signed with the minted secret
- **THEN** switchboard MUST recompute the provider HMAC over the raw body against the held secret and,
  on a valid signature, persist the delivery `verified=true` under `trust_mode=signed`; an invalid or
  missing signature MUST be rejected with nothing persisted

#### Scenario: Trust mode cannot be downgraded by the agent

- **WHEN** an agent attempts to create or alter a `signed`-type webhook to weaken its verification
- **THEN** switchboard MUST derive and enforce the trust mode itself, MUST verify the webhook per
  SPEC-0003 regardless, and MUST NOT honor any agent-supplied trust downgrade

### Requirement: Structured Output and Stable Error Shape

Every verb MUST accept a declared input shape and return structured output. Errors MUST use a stable
machine `code` plus a human `message` that MUST NOT contain secret material. The defined codes are:
`unauthenticated` (missing/invalid credential), `forbidden` (verb not in allowlist, or queue/target
outside grant), `conflict` (lost claim race / wrong state), `not_found` (unknown todo/webhook id),
`invalid_argument` (bad input), `ceiling_exceeded` / `forbidden_source_type` (webhook ceiling), and
`internal` (unexpected server-side failure).

#### Scenario: Unknown todo id raises not_found

- **WHEN** a transition verb targets a todo id that does not exist
- **THEN** the server MUST respond `not_found`

### Requirement: Error Handling Standards

Errors crossing a boundary (store, outbound HTTP, request decode) MUST be wrapped with context and
MUST NOT be silently swallowed. Domain failures MUST be surfaced as sentinel errors (e.g. not-found,
conflict) and mapped to the stable error codes above; unexpected failures MUST map to `internal` and
be logged with structured context. A store or transport failure MUST NOT leak internal detail or
secret material into the client-visible `message`.

#### Scenario: Store failure maps to internal and is logged

- **WHEN** a store operation returns an unexpected error during a verb
- **THEN** the server MUST respond `internal`, MUST log the underlying error with context, and MUST
  NOT expose the underlying error text to the client

### Requirement: Concurrency Safety

The endpoint MUST propagate the request context for cancellation and timeout into all store and
outbound operations. The new-todo fan-out stream MUST be a best-effort doorbell that never blocks a
producer and never gates correctness — the durable todo queue remains the ledger. The background
lease reaper MUST have an explicit lifecycle (clean startup, graceful shutdown on context
cancellation) and MUST be race-safe. Concurrent claims MUST be resolved atomically by the store, not
by application-level locking.

#### Scenario: Slow stream consumer never blocks producers

- **WHEN** a subscribed agent's stream buffer is full while new todos are created
- **THEN** the server MUST drop the pushed notification for that subscriber rather than block, and
  the todo MUST remain durably in the queue for the agent to claim

#### Scenario: Graceful shutdown stops the reaper

- **WHEN** the process receives a shutdown signal (request context cancelled)
- **THEN** the reaper MUST stop cleanly without leaking goroutines

### Requirement: Database Operation Standards

Multi-step atomic mutations (claim-with-lease, complete, fail-with-retry-or-dead-letter) MUST execute
under a transaction or an equivalent atomic conditional update so a todo cannot be left in an
inconsistent state. All queries MUST be parameterized; string interpolation of user-supplied values
into SQL MUST NOT be used. Connections MUST have explicit lifecycle and timeouts via the propagated
context.

#### Scenario: Claim is atomic

- **WHEN** `claim` transitions a todo from `pending` to `claimed`
- **THEN** the state change and lease assignment MUST be applied atomically so no partial state is
  observable

## Security Requirements

### Authentication

Every endpoint on this surface is served over HTTP and MUST require authentication by default via a
bearer credential resolved to a scoped endpoint. There are no public verbs on this surface.

> **Transport superseded (ADR-0017; [SPEC-0014](../mcp-transport/spec.md)).** These verbs now ship as
> MCP tools over Streamable HTTP at `/mcp/{endpoint}` — `list_todos` / `claim` / `complete` / `fail` /
> `heartbeat` plus the webhook self-management verbs, with the new-todo doorbell delivered as
> `notifications/claude/channel` on the MCP notification stream. The bespoke `/agent/*` REST routes and
> the `/agent/stream` SSE below are **retired** and no longer served; the table is kept as the record of
> the verb set, auth model, and scope semantics that SPEC-0014 carries over MCP.

| Endpoint (retired REST shape) | Auth | Justification |
|----------|------|---------------|
| `GET /agent/whoami` | Required | Returns the caller's identity + scope. |
| `GET /agent/todos` (`list_todos`) | Required | Exposes queue contents. |
| `POST /agent/todos/{id}/claim` (`claim`) | Required | State-changing lease acquisition. |
| `POST /agent/todos/{id}/complete` (`complete`) | Required | Terminal state transition. |
| `POST /agent/todos/{id}/fail` (`fail`) | Required | State transition / dead-letter. |
| `GET /agent/stream` (new-todo doorbell) | Required | Streams queue activity in scope. |
| `create_webhook` / `list_webhooks` / `rotate_webhook` / `delete_webhook` | Required | Manages ingestion sources within the vended ceiling. |

### Rate Limiting

Webhook self-management verbs (`create_webhook`, `rotate_webhook`, `delete_webhook`) MUST be
rate-limited per endpoint to prevent noisy churn within the ceiling. Todo drain verbs SHOULD be
rate-limited per endpoint to bound store load. A per-endpoint token bucket is RECOMMENDED; the ceiling
count cap is a hard secondary bound on webhook creation.

### Security Headers

All HTTP responses on this surface MUST include:
- `Content-Security-Policy`: `default-src 'none'` (JSON/SSE API, not browser assets)
- `X-Frame-Options`: DENY
- `X-Content-Type-Options`: nosniff
- `Referrer-Policy`: strict-origin-when-cross-origin

### Request Body Size Limits

All verbs accepting request bodies (claim/complete/fail, webhook create/rotate) MUST bound them with
`http.MaxBytesReader`. Limit: **1 MiB** per request — the MCP-wide cap applied to every `/mcp/*`
request before JSON-RPC parsing (`maxBodyBytes` in `internal/mcp/mcp.go`), which these verbs ride.
Oversized bodies MUST be rejected before decoding.

### CSRF Protection

This surface is a machine-to-machine bearer-authenticated JSON/SSE API, not a cookie-authenticated
browser session; classic form CSRF does not apply. State-changing verbs MUST authenticate via the
bearer credential and MUST NOT be reachable through ambient cookie authentication. No HTML forms are
served here.

### Redirect Validation

Agents supply webhook `target_queue` and `source_type`, not outbound redirect targets — this surface
issues no user-supplied outbound redirects. The `ingest_url` returned by `create_webhook` is minted by
switchboard, not supplied by the agent, so there is no open-redirect or SSRF vector on this surface.
(The read-only event-history surface's `replay` target validation lives in
[SPEC-0005](../mcp-tools/spec.md).)

## Cross-References

- Self-management split & ceiling: [ADR-0012](../../../adrs/ADR-0012-agents-self-manage-webhooks.md).
- Contract shape / structured output / errors: [ADR-0005](../../../adrs/ADR-0005-mcp-tool-and-resource-contract.md), [SPEC-0005](../mcp-tools/spec.md).
- Trust model switchboard enforces regardless of who created the webhook: [SPEC-0003 / ADR-0003](../../../adrs/ADR-0003-per-provider-ingestion-and-trust-model.md).
- Design & architecture for this capability: [design.md](design.md).
