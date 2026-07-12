---
status: implemented
date: 2026-07-06
implements: [ADR-0009]
---

# SPEC-0009: Personas as Scoped Agent Cards

## Overview

A **persona** is a named, scoped face of a single registered agent. One agent runtime is often used
for several different jobs — the same base model might act as a code **reviewer** in one context and a
**deployer** in another — and each job wants a different system prompt, advertises different skills, and
MUST carry different, least-privilege access. This capability defines the persona record, the rule that
derives a persona's advertised skills from what it is actually vended, and the way each persona is
published as an [A2A](https://a2a-protocol.org/) **Agent Card** served at a well-known HTTP endpoint for
discovery.

This spec realizes [ADR-0009](../../../adrs/ADR-0009-personas-as-scoped-agent-cards.md) (personas as
scoped Agent Cards). It draws its capability slice from the agent's vended endpoints
([ADR-0008](../../../adrs/ADR-0008-human-principal-vended-endpoints.md)) and its published cards are the
units discovered during friending
([ADR-0010](../../../adrs/ADR-0010-a2a-discovery-human-vended-friending.md), SPEC-0010).

## Requirements

### Requirement: Persona Record

A persona MUST be composed of exactly three authored parts: the base agent runtime it is a face of, a
human-authored system prompt, and a subset of that agent's vended verbs (its capability slice). A
persona record MUST reference exactly one `agent_id` (the registered agent, per
[ADR-0008](../../../adrs/ADR-0008-human-principal-vended-endpoints.md)). The `system_prompt` MUST be
authored by the owning human; the system MUST NOT derive or infer it. The `verb_subset` and `queues`
fields are the unit of capability scoping and MUST each be a subset of the agent's vended verb/queue
grant. A persona record SHOULD carry an optional `description` surfaced on its Agent Card.

#### Scenario: Two personas of one agent carry different access

- **WHEN** an agent has a `reviewer` persona (`verb_subset` = `list_todos, claim, complete`, `queues` =
  `reviews`) and a `deployer` persona (`verb_subset` adds webhook verbs, `queues` = `deploys`)
- **THEN** each persona's access is bounded by its own `verb_subset`/`queues`, and the reviewer persona
  MUST NOT be able to act on the `deploys` queue nor use the deployer's verbs, and vice versa

#### Scenario: Persona scope may not exceed the agent's vended grant

- **WHEN** a persona is created or updated with a `verb_subset` or `queues` value that is not a subset of
  the underlying agent's vended scope
- **THEN** the request MUST be rejected and the persona MUST NOT be persisted

### Requirement: Skills Derived From Vended Capability

A persona's advertised `skills` MUST be derived from its `verb_subset`, never hand-declared
independently. The system MUST maintain an authoritative verb→skill map; a skill whose required verb is
absent from the persona's `verb_subset` MUST NOT appear on the persona's Agent Card. Recomputation MUST
occur whenever the `verb_subset` changes so that "advertised capability" equals "actual capability" by
construction.

#### Scenario: A skill requiring an ungranted verb is never advertised

- **WHEN** a persona's `verb_subset` does not include `create_for`
- **THEN** the `delegate-work` skill (which requires `create_for`) MUST NOT appear on the persona's
  Agent Card

#### Scenario: Changing the subset changes the card

- **WHEN** a verb is added to or removed from a persona's `verb_subset`
- **THEN** the persona's advertised `skills` MUST be recomputed so the card reflects only skills whose
  required verbs are present

### Requirement: Agent Card Mapping

Each persona MUST be published as a schema-valid A2A Agent Card. The card's `name` MUST map from the
persona `name`; `description` from the persona `description` or a summary of its `system_prompt`; `url`
from the persona's A2A discovery endpoint; `provider`/owner from the owning human's identity chain
([ADR-0008](../../../adrs/ADR-0008-human-principal-vended-endpoints.md), attestable via OIDC per
[ADR-0011](../../../adrs/ADR-0011-identity-assurance-oidc-passkey-deferred.md)); and `skills` from the
derived set. The card's `capabilities` MUST advertise only the A2A features switchboard supports
(discovery/announcement) and MUST NOT advertise direct task delegation. The card's `url` is a discovery
identifier only and MUST NOT be treated as a work-intake channel — work arrives as todos per
[ADR-0010](../../../adrs/ADR-0010-a2a-discovery-human-vended-friending.md).

#### Scenario: Card provider ties to the owning human

- **WHEN** a persona's Agent Card is generated
- **THEN** its `provider`/owner field MUST resolve to the owning human's identity chain, not the agent's
  self-asserted identity

#### Scenario: Card does not advertise delegation intake

- **WHEN** any persona's Agent Card is served
- **THEN** its `capabilities` MUST NOT claim A2A direct task delegation, and its `url` MUST NOT be
  documented as accepting work

### Requirement: Well-Known Card Endpoint

Each persona's Agent Card MUST be served at the A2A well-known path `/.well-known/agent-card.json`,
disambiguated per persona by a per-persona base path (e.g.
`/a/{persona_id}/.well-known/agent-card.json`) so the well-known path resolves to exactly one persona's
card. The card endpoint MUST be read-only (`GET`) and MUST reflect the persona's current `verb_subset`.

#### Scenario: Well-known path resolves to one persona

- **WHEN** a peer requests `/a/{persona_id}/.well-known/agent-card.json`
- **THEN** the response MUST be the schema-valid A2A Agent Card for exactly that persona and no other

#### Scenario: Card endpoint is read-only

- **WHEN** a client issues a state-changing method (POST/PUT/PATCH/DELETE) against a persona card
  endpoint
- **THEN** the request MUST be rejected; the endpoint MUST expose only a read (`GET`) of the current card

### Requirement: Discoverability Is Owner-Controlled

A persona MUST appear in a bounded discovery directory only if its owning human has marked it
discoverable. The well-known URL is the canonical card location; presence in a directory MUST be an
explicit owner choice, consistent with the anti-spam posture of
[ADR-0010](../../../adrs/ADR-0010-a2a-discovery-human-vended-friending.md).

#### Scenario: Non-discoverable persona stays out of the directory

- **WHEN** a persona's owner has not marked it discoverable
- **THEN** the persona MUST NOT appear in any bounded discovery directory, even though its well-known
  card URL may still resolve

## Security Requirements

### Authentication

Agent Card endpoints publish only outward-facing, owner-approved discovery metadata (persona name,
description, derived skills, owner provenance) and grant nothing — they are the A2A analogue of a
public profile. Serving them publicly is required for A2A interoperability with peers that have not yet
friended the persona. They therefore default to public, but only for personas the owner has explicitly
marked discoverable; every other endpoint in this capability requires authentication.

| Endpoint | Auth | Justification |
|----------|------|---------------|
| `GET /a/{persona_id}/.well-known/agent-card.json` | Public | A2A discovery requires peers to read the card before any friendship exists; the card grants no access and exposes only owner-approved metadata. Served only for owner-marked-discoverable personas. |
| Persona create/update/delete (human web UI) | Required | Authenticated human owner only; mutates a scoped identity/capability surface. |
| Directory listing of discoverable personas | Required | Bounded directory access is authenticated to support quotas and anti-spam (SPEC-0010). |

### Rate Limiting

The public card endpoint MUST be rate-limited per source IP (RECOMMENDED default: 60 requests/minute)
to blunt scraping and enumeration of persona ids. Directory listing MUST be rate-limited per
authenticated principal. Owner mutation endpoints inherit the human web UI's session rate limits.

### Security Headers

All HTTP responses MUST include:
- `Content-Security-Policy`: `default-src 'none'` for the JSON card endpoint (no active content);
  `default-src 'self'` for any HTML persona-management surface.
- `X-Frame-Options`: DENY
- `X-Content-Type-Options`: nosniff
- `Referrer-Policy`: strict-origin-when-cross-origin

The card endpoint MUST set `Content-Type: application/json`.

### Request Body Size Limits

All endpoints accepting bodies (persona create/update) MUST bound them with `http.MaxBytesReader`.
Default limit: 64 KiB (system prompts are human-authored prose, not large blobs). The public card
endpoint accepts no body.

### CSRF Protection

State-changing persona-management endpoints served from the human web UI MUST implement CSRF protection
via the same session-bound token strategy as the rest of the switchboard web UI (SameSite cookie +
per-form token). The public card `GET` endpoint is not state-changing and requires no CSRF token.

### Redirect Validation

No user-supplied redirects exist in this capability. The persona `url` published on a card is derived by
switchboard from the persona's own base path and MUST NOT be caller-supplied.
