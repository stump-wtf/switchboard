---
status: amended
date: 2026-07-21
amends: [ADR-0010]
implements: [ADR-0010, ADR-0022]
requires: [SPEC-0009, SPEC-0003]
---

# SPEC-0010: A2A Discovery + Human-Vended Friending

## Overview

Switchboard runs two complementary protocols: **MCP** connects an agent to *tools* (the todo/webhook
verbs on its vended endpoint,
[ADR-0008](../../../adrs/ADR-0008-human-principal-vended-endpoints.md)), and **[A2A](https://a2a-protocol.org/)**
connects an agent to *other agents* for discovery. Personas are published as A2A Agent Cards
(SPEC-0009, [ADR-0009](../../../adrs/ADR-0009-personas-as-scoped-agent-cards.md)). This capability
defines how one agent gains scoped access to hand work to another: a **friend request** that a **human
approves**, where approval *is* the vend that mints a scoped MCP endpoint.

The controlling rule is that A2A grants nothing on its own; inbound work is governed by human vending and
lands as durable **todos** ([ADR-0007](../../../adrs/ADR-0007-todos-as-core-primitive.md)) regardless of
which wire protocol created them. Originally A2A was discovery/announcement only and cross-agent work
flowed exclusively through MCP's `create_for`; as of
[ADR-0021](../../../adrs/ADR-0021-a2a-task-delegation-transport.md) /
[SPEC-0018](../a2a-tasks/spec.md), a grant-holder may **also** hand work in via A2A's own `SendMessage` —
but only once a grant from this spec's flow exists. A2A discovery still grants nothing by itself, and this
spec's flow remains the *only* way to acquire a grant. Friend edges are per-direction, revocable, and
non-transitive, and every request MUST carry verifiable, OIDC-signed provenance of the requesting human.

This spec realizes [ADR-0010](../../../adrs/ADR-0010-a2a-discovery-human-vended-friending.md). It depends
on SPEC-0009 for the personas/Agent Cards that are discovered, and on
[ADR-0011](../../../adrs/ADR-0011-identity-assurance-oidc-passkey-deferred.md) for provenance.

## Requirements

### Requirement: Friend-Request Lifecycle

A friend edge MUST progress through the states `pending → approved | denied`, with `approved` edges
further transitioning to `revoked`. Sending a friend request MUST create a **pending edge that grants no
access**. Approval MUST transition the edge to `approved` and mint a scoped MCP endpoint; denial MUST
transition it to `denied` (terminal, no grant); revocation MUST transition an approved edge to `revoked`
and kill the vended endpoint. A request MUST carry a `requested_scope` (queues + verbs), a `reason`, and
verifiable OIDC-signed provenance of the requesting human.

#### Scenario: Pending edge grants nothing

- **WHEN** agent A sends a friend request to persona B and the edge is created in `pending`
- **THEN** A MUST have no access to B until a human approves; no MCP endpoint MUST exist for the edge
  while it is `pending`

#### Scenario: Approval mints the endpoint

- **WHEN** B's owning human approves the pending request
- **THEN** the edge MUST transition to `approved` and a scoped MCP endpoint MUST be minted granting A the
  approved access to B

#### Scenario: Denial is terminal

- **WHEN** B's owning human denies the pending request
- **THEN** the edge MUST transition to `denied`, no endpoint MUST be minted, and the requester MAY
  re-request subject to quotas

### Requirement: Approval Is the Vend, Narrow-Only

Approval MUST be the sole act that mints access; there MUST be no separate vend step. At approval the
human MAY narrow the scope, and the `granted_scope` MUST be a subset of the `requested_scope` — the
granted access MUST NOT exceed what was requested, and MUST NOT exceed what the human allows. The minted
endpoint's scope MUST equal `requested_scope ∩ human-narrowing`.

#### Scenario: Granted scope cannot exceed requested

- **WHEN** a human approves a request and supplies a `granted_scope`
- **THEN** the system MUST reject any `granted_scope` that is not a subset of the `requested_scope`, and
  the minted endpoint MUST carry only the narrowed scope

#### Scenario: Approval with no narrowing grants the requested scope

- **WHEN** a human approves without narrowing
- **THEN** the minted endpoint's scope MUST equal the `requested_scope`

### Requirement: Verifiable OIDC-Signed Provenance

A friend request MUST carry verifiable, OIDC-signed provenance of the requesting **human**
([ADR-0011](../../../adrs/ADR-0011-identity-assurance-oidc-passkey-deferred.md)), not the agent's
self-assertion of who owns it. A request whose provenance does not validate MUST be rejected as
`unauthenticated` and MUST NOT create a pending edge. The pending edge MUST carry a
`provenance_verified` flag that is `true` only when provenance validated.

#### Scenario: Missing or invalid provenance is rejected

- **WHEN** a friend request arrives without valid OIDC-signed human provenance
- **THEN** it MUST be rejected as `unauthenticated` and no pending edge MUST be created

#### Scenario: Approver sees an attested counterparty

- **WHEN** a pending edge is recorded for a validly-attested request
- **THEN** its `from_human` field MUST be the OIDC subject of the requesting human (attested, not
  self-asserted) and `provenance_verified` MUST be `true`

### Requirement: Approval Surfaced from the Friend Edge

*Amended 2026-07-21 (was "Approval Delivered as a Todo") per
[ADR-0022](../../../adrs/ADR-0022-endpoint-scoped-todo-ownership.md).*

A pending friend request MUST be surfaced to the target human directly from its **`friend_edges`
row** — it MUST NOT be mirrored into the todo queue. The pending row is already the durable record,
and it MUST carry the legible **who / why / requested-scope** summary — the edge id (`request_id`),
`from_human`, `from_persona`, `to_persona`, `requested_queues`, `requested_verbs`, `reason`, and
`provenance_verified` — so the human decides with full context rather than a raw blob.

The approval view MUST be owner-scoped on `to_human`: a pending request MUST be visible to the
targeted human and MUST NOT be visible to any other human. Because the view renders the edge's own
`state`, deciding the edge is what clears it — approval, denial, and revocation MUST each remove the
request from the pending view by the same write that transitions the edge, with no second record to
reconcile. A duplicate request for a directional pair that already holds a live (`pending` or
`approved`) edge MUST collide rather than produce a second listing.

Rationale: a friend approval is **human** work with no owning endpoint, and under ADR-0022 every todo
is endpoint-owned (`todos.endpoint_id` is NOT NULL), so an approval todo cannot exist. The mirror was
also redundant — it duplicated a `friend_edges` row that the Friends view already reads — and its
idempotency key never actually deduped, because the ON CONFLICT arm it relied on hung off a null
`endpoint_id`. Duplicate suppression now rests solely on the partial unique index
`idx_friend_edges_live (from_persona, to_persona, direction) WHERE state IN ('pending','approved')`,
whose three indexed columns are all NOT NULL, so no NULLS-distinct escape hatch exists.

**Forward-looking:** DELEGATING an approval decision to an agent (rather than a human deciding it in
the Friends view) is a different case and is out of scope here. When it lands it WILL mint a normal
endpoint-owned todo pinned to the **delegate's** endpoint; the tenant-isolation rules in
[SPEC-0003](../todo-queue/spec.md) REQ "Endpoint Ownership (Tenant Isolation)" already govern that
todo, and this requirement's no-mirroring rule is unaffected.

#### Scenario: Pending request is visible to the targeted human

- **WHEN** a valid friend request is accepted for a persona owned by human B
- **THEN** the pending edge MUST appear in B's pending approvals carrying the
  who/why/requested-scope summary and the `provenance_verified` flag, and no todo MUST be created for
  it

#### Scenario: A request is not visible to another human

- **WHEN** a friend request targeting human B's persona is pending
- **THEN** it MUST NOT appear in any other human's pending approvals

#### Scenario: Deciding the edge clears the pending view

- **WHEN** the target human approves, denies, or revokes the edge
- **THEN** the request MUST no longer appear in the pending approvals view, because the view reads
  the edge's own `state` and no companion record exists

#### Scenario: A duplicate request collides rather than double-listing

- **WHEN** a second friend request arrives for a `(from_persona, to_persona, direction)` that already
  holds a `pending` or `approved` edge
- **THEN** it MUST be refused as a conflict, no second edge MUST be created, and the target human's
  pending approvals MUST still show exactly one request for that pair

### Requirement: Per-Direction, Revocable, Non-Transitive Edges

Friend grants MUST be per-direction: an A→B grant MUST NOT imply a B→A grant; a B→A grant requires its
own request and approval. Either grant MUST be revocable at any time, and revocation MUST be instant and
one-sided — revoking one direction MUST leave the other intact. Friending MUST be non-transitive:
friending B MUST reveal nothing about B's other friends, B's other personas, or B's queues beyond the
one granted; there MUST be no graph traversal across edges.

#### Scenario: Approval is one-directional

- **WHEN** A's request to hand work to B is approved
- **THEN** B MUST NOT thereby gain any access to A; a B→A grant MUST require its own request and approval

#### Scenario: Revocation is one-sided

- **WHEN** one direction of a mutual friendship is revoked
- **THEN** that endpoint MUST be killed instantly and the other direction MUST remain intact

#### Scenario: Friendship reveals nothing transitive

- **WHEN** A friends B
- **THEN** A MUST NOT be able to enumerate B's other friends, B's other personas, or any of B's queues
  beyond the granted one

### Requirement: Work Flows as Todos, Whether Via MCP or A2A

> Amended 2026-07-20 by [ADR-0021](../../../adrs/ADR-0021-a2a-task-delegation-transport.md) /
> [SPEC-0018](../a2a-tasks/spec.md). Originally this requirement read "Work Flows as Todos, Not A2A
> Tasks" and forbade any A2A task intake. The grant-acquisition invariant this requirement protects —
> nothing lands in a queue without a grant minted by this spec's approval flow — is unchanged; only the
> set of wire protocols that can *use* an existing grant has grown.

After a grant is minted, cross-agent work MUST flow as todos created in the granted queue — via MCP's
`create_for`, or via A2A's `SendMessage` once [SPEC-0018](../a2a-tasks/spec.md) is implemented — and MUST
be durable, owned, dedup'd, and leaseable
([ADR-0007](../../../adrs/ADR-0007-todos-as-core-primitive.md)) regardless of which protocol created it.
A2A's `SendMessage` MUST enforce the exact same vended-endpoint authorization `create_for` does; it MUST
NOT accept a task from a caller that does not hold a grant minted by this spec's flow. A2A's role in
*acquiring* a grant remains limited to discovery/announcement — `SendMessage` is only usable after a grant
already exists.

#### Scenario: Cross-agent work arrives as a todo regardless of protocol

- **WHEN** A, holding an approved grant, hands work to B via either `create_for` or A2A's `SendMessage`
- **THEN** the work MUST land as a todo in B's granted queue, indistinguishable in the store from work
  created via the other protocol

#### Scenario: A2A task intake still requires a grant

- **WHEN** a caller without an approved grant invokes A2A's `SendMessage` against a persona
- **THEN** switchboard MUST reject the call exactly as it would reject an unauthenticated `create_for`
  call, and MUST NOT create a todo

### Requirement: Anti-Spam — Bounded Discovery and Quotas

Discovery MUST be limited to a bounded set of known directories, not the open internet, so an agent
cannot be friend-requested by an arbitrary unknown party at will. Friend requests MUST be quota'd and
rate-limited per requester to blunt flooding. Every pending request MUST present a legible
who/why/scope summary.

#### Scenario: Requests over quota are refused

- **WHEN** a requester exceeds its friend-request quota or rate limit
- **THEN** further requests MUST be refused until the quota window resets, and no pending edge MUST be
  created for the refused requests

## Security Requirements

### Authentication

Every endpoint in this capability defaults to requiring authentication. The friend-request intake is
authenticated by OIDC-signed human provenance (not a bearer credential and not the agent's
self-assertion); the approval/deny/revoke actions are authenticated as the target human via the web-UI
session; work handoff is authenticated by the vended MCP endpoint credential
([ADR-0008](../../../adrs/ADR-0008-human-principal-vended-endpoints.md)).

| Endpoint | Auth | Justification |
|----------|------|---------------|
| `send_friend_request` (A2A intake) | Required | Must carry verifiable OIDC-signed human provenance; invalid → `unauthenticated`, no edge. |
| `list_pending_approvals` | Required | Authenticated target human only; lists their own pending friend edges (`to_human` scoped). |
| `approve` / `deny` | Required | Authenticated target human only; approval mints an endpoint (the vend). |
| `revoke` | Required | Authenticated owning human only; kills a vended endpoint instantly. |
| `create_for` (work handoff) | Required | Bearer credential of the minted, scoped MCP endpoint; verb + queue enforced at the boundary. |

There are no public endpoints in this capability. (Persona **discovery** cards are public but belong to
SPEC-0009; friending itself grants and mutates access and is authenticated end-to-end.)

### Rate Limiting

Friend requests MUST be quota'd and rate-limited **per requesting human/persona** (RECOMMENDED default:
a bounded number of pending requests per target and a per-hour request ceiling per requester) to blunt
flooding. Rate-limit rejections MUST NOT create pending edges. Approve/deny/revoke inherit the web-UI
session rate limits. Work handoff (`create_for`) is bounded by the todo-queue's idempotency and the
endpoint's scope.

### Security Headers

All HTTP responses MUST include:
- `Content-Security-Policy`: `default-src 'self'` for the human approval web UI; `default-src 'none'`
  for JSON API responses.
- `X-Frame-Options`: DENY
- `X-Content-Type-Options`: nosniff
- `Referrer-Policy`: strict-origin-when-cross-origin

### Request Body Size Limits

All endpoints accepting bodies MUST bound them with `http.MaxBytesReader`. Default limit: 64 KiB for
friend-request and approval payloads (a `requested_scope` is a small list of queues/verbs plus a reason
string, not a large blob). Oversized bodies MUST be rejected before parsing.

### CSRF Protection

State-changing human actions — `approve`, `deny`, `revoke` — MUST implement CSRF protection via the
switchboard web UI's session-bound token strategy (SameSite session cookie + per-form token). The
`send_friend_request` A2A intake is not a browser-form flow; it is protected by OIDC-signed provenance
rather than a CSRF token.

### Redirect Validation

No user-supplied redirects exist in this capability. Approval and revocation return to fixed, server-owned
web-UI paths; the requesting persona's `url` is server-derived (SPEC-0009), not caller-supplied. Open
redirects MUST NOT be permitted.
