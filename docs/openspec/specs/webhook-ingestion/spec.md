---
status: amended
date: 2026-07-21
amends: [ADR-0003, ADR-0014]
implements: [ADR-0003, ADR-0012, ADR-0022]
---

# SPEC-0001: Webhook Ingestion (Push Adapters)

## Overview

Webhook ingestion is Switchboard's only ingestion surface: inbound HTTP endpoints that receive
provider deliveries, verify their trust (per-provider signature or the ingest-URL token), normalize
the delivery, persist an event for history, and enqueue a durable todo. It realizes the verification
and trust model of
[ADR-0003](../../../adrs/ADR-0003-per-provider-ingestion-and-trust-model.md) on the self-managed
webhooks of [ADR-0012](../../../adrs/ADR-0012-agents-self-manage-webhooks.md).

A webhook is *push*: the sender initiates an HTTP request, and the "ack" is the HTTP response.
Verification happens at receive, before any parsing, against the raw request body. The trust
modes are honest and ordered — `signed` (HMAC verified, `verified=true`) is strictly stronger than
`token` (the unguessable ingest URL authenticates the caller, not the body, `verified=false`). The
trust mode is stored on every event and
surfaced everywhere so a human never has to guess whether a delivery was authenticated.

**All webhook ingestion flows through agent self-managed webhooks**
([ADR-0012](../../../adrs/ADR-0012-agents-self-manage-webhooks.md)) served at
`POST /webhooks/w/{ingest_token}` (`internal/ingest/selfmanaged.go`). Every webhook is owned by
exactly one vended MCP endpoint, and every todo produced by a delivery is pinned to that endpoint
(per the [todo-queue spec](../todo-queue/spec.md) "Endpoint Ownership" requirement). The agent
creates the webhook; switchboard derives the trust mode from the source type and mints and holds the
HMAC secret.

> **Amended 2026-09-21, with the shared-receiver removal.** The operator-configured receivers (`/webhooks/{provider}`,
> `/webhooks/generic/{name}`), their env-configured secrets and tokens, and the `open` trust mode
> they alone could produce were removed, along with pull ingestion
> ([SPEC-0002](../queue-adapters/spec.md), retired): instance-wide ingestion belongs to no tenant, so
> it cannot name the endpoint that owns the todos it mints (ADR-0022). The verification, replay,
> dedup, and sanitization requirements below are unchanged.

## Requirements

### Requirement: Signed Webhook Verification

For a webhook whose source type is `signed` (GitHub, Gitea, Stripe, Slack, Cairn), the receiver MUST verify a
cryptographic signature over the **raw request body** before parsing the payload, using a
constant-time comparison. A missing, malformed, or failing signature MUST return HTTP 401 and MUST
NOT persist the payload; only a redacted rejection line MAY be logged (provider, event type if
known, source IP — never the secret or full signature). A verified request MUST persist the event
with `trust_mode='signed'`, `verified=true`, and a `verify_detail` naming the scheme (e.g.
`hmac-sha256 ok`). Signature comparison MUST use a constant-time primitive (`hmac.Equal`), never a
plain byte equality.

#### Scenario: Valid GitHub signature is accepted

- **WHEN** a self-managed github webhook delivery arrives whose `X-Hub-Signature-256` header matches
  the HMAC-SHA256 of the raw body under the minted signing secret
- **THEN** the event is persisted with `source='github'`, `family='webhook'`,
  `trust_mode='signed'`, `verified=true`, `verify_detail='hmac-sha256 ok'`, a todo is created, and
  the response is HTTP 202 with `{todos, created, verified: true, trust_mode: 'signed'}`

#### Scenario: Valid Gitea self-managed signature is accepted

- **WHEN** a self-managed gitea webhook delivery arrives whose `X-Gitea-Signature` header matches
  the bare hex HMAC-SHA256 of the raw body under the minted signing secret (no `sha256=` prefix)
- **THEN** the event is persisted with `source='gitea'`, `family='webhook'`,
  `trust_mode='signed'`, `verified=true`, `verify_detail='hmac-sha256 ok'`, a todo is created, and
  the response is HTTP 202 with `{todos, created, verified: true, trust_mode: 'signed'}`

#### Scenario: Missing or invalid signature is rejected without persisting

- **WHEN** a signed-provider request arrives with a missing, malformed, or non-matching signature
- **THEN** the response is HTTP 401, no event row and no todo are written, and a redacted rejection
  line (no secret, no full signature) MAY be logged

#### Scenario: Signed webhook with no stored secret

- **WHEN** a delivery arrives for a `signed` webhook whose signing secret is absent from the store
- **THEN** the request is rejected with HTTP 503 without persisting, and no signature is compared

#### Scenario: Unknown ingest token

- **WHEN** a request arrives at `POST /webhooks/w/{ingest_token}` with a token that matches no webhook
- **THEN** the response is HTTP 404 and no event or todo is written

### Requirement: Replay-Window Enforcement for Timestamped Signatures

For signed providers whose scheme signs a timestamp (Stripe `t=`, Slack
`X-Slack-Request-Timestamp`), the receiver MUST reject a delivery whose signed timestamp is outside
a freshness tolerance (default 300 seconds) even when the HMAC is otherwise valid, returning HTTP
401 and persisting nothing. For providers whose scheme does not sign a timestamp (GitHub, Gitea), the
receiver MUST NOT fabricate a replay window.

#### Scenario: Stale Stripe timestamp is rejected

- **WHEN** a Stripe delivery presents a valid `v1=` HMAC but a `t=` timestamp more than the
  configured tolerance from now
- **THEN** the response is HTTP 401 and no event is persisted

#### Scenario: GitHub delivery has no timestamp check

- **WHEN** a GitHub delivery with a valid signature arrives
- **THEN** it is accepted regardless of age (GitHub does not sign a timestamp) and no freshness
  window is applied

### Requirement: Cairn Signed Deliveries

A self-managed webhook of source type `cairn` is `signed`. It MUST verify cairn's outbound
`artifact.created` deliveries (cairn ADR-0017 / SPEC-0012) as follows.

1. **Signature.** `X-Cairn-Signature: sha256=<hex>` MUST equal the HMAC-SHA256 of the **raw request
   body** under the minted secret, compared in constant time.
2. **Replay defenses.** Cairn signs no timestamp header, so replay defenses MUST come from the signed
   body:
   - It MUST carry a non-empty `event_id`, which MUST be the delivery's idempotency key
     (`<webhook-id>:<event_id>`).
   - It MUST carry a `created_at` (RFC 3339) within the replay tolerance of now, in either direction
     (default 300 seconds).
3. **Event-id header.** The unsigned `X-Cairn-Event-Id` header, when present, MUST equal the signed
   `event_id`.

Failing any check MUST return HTTP 401 and persist nothing. The event's `event_type` MUST be the
signed body's `kind`. The todo title MUST name the kind, the artifact title, and the share type.

#### Scenario: Tampered cairn body is rejected

- **WHEN** a cairn delivery's body is altered after signing
- **THEN** the response is HTTP 401 and no event or todo is persisted

#### Scenario: Replay after the window is rejected

- **WHEN** a correctly signed cairn delivery arrives whose signed `created_at` is outside the replay
  tolerance
- **THEN** the response is HTTP 401 and nothing is persisted

#### Scenario: Replay inside the window collapses

- **WHEN** an identical signed cairn delivery is replayed within the window
- **THEN** it derives the same idempotency key from the signed `event_id` and collapses onto the
  original todo

#### Scenario: Forged event-id header cannot mint a fresh dedup key

- **WHEN** a captured cairn delivery is replayed with a different `X-Cairn-Event-Id` header
- **THEN** the response is HTTP 401 and nothing is persisted

### Requirement: Shared-Secret Token Authentication for Unsigned Webhooks

For senders with no signing scheme (Docker Hub, homelab/self-hosted senders), served by a webhook of
source type `generic`, the **shared-secret token** is the webhook's unguessable ingest token, minted
by switchboard and presented in the URL path (`POST /webhooks/w/{ingest_token}`). The receiver MUST
reject a request whose token matches no webhook with HTTP 404 without persisting. An accepted token
request MUST persist the
event with `trust_mode='token'`, `verified=false`, and a `verify_detail` that states the caller is
authenticated but the body is not verified. `token` MUST NOT ever be presented as `signed`.

#### Scenario: Correct token is accepted as token trust

- **WHEN** a request arrives at the ingest URL of a `generic` webhook
- **THEN** the event is persisted with `family='webhook'`, `trust_mode='token'`, `verified=false`, a
  todo is created, and the response is HTTP 202

#### Scenario: Missing or wrong token is rejected

- **WHEN** a request presents an ingest token that matches no webhook (wrong, rotated, or deleted)
- **THEN** the response is HTTP 404 and no event or todo is written

### Requirement: Trust Mode Derived From Source Type

A webhook's trust mode MUST be derived by switchboard from its source type when the webhook is
created (`github`, `gitea`, `stripe`, `slack`, `cairn` ⇒ `signed`; `generic` ⇒ `token`). The agent
MUST NOT be able to supply or downgrade it, and a source type with no derivation MUST be refused at
create. An ingestion path MUST NOT produce `trust_mode='open'`: a sender that can present no signature
is served by a `generic` webhook, which is `token`.

#### Scenario: Generic webhook is token, never open

- **WHEN** an agent creates a webhook with source type `generic`
- **THEN** the webhook's trust mode is `token`, and its deliveries persist with `trust_mode='token'`,
  `verified=false`

#### Scenario: Unsupported source type is refused

- **WHEN** `create_webhook` names a source type switchboard cannot derive a trust mode for
- **THEN** the call is refused and no webhook is created

### Requirement: Idempotency Key Extraction and Dedup

Each accepted webhook delivery MUST derive an idempotency key and use it to dedup redeliveries into a
single todo. The key SHOULD be derived from a provider delivery id where one exists (GitHub
`X-GitHub-Delivery`, Gitea `X-Gitea-Delivery`, Stripe event `id`) and MUST fall back to a body hash
where the provider supplies no delivery id (Slack). A generic sender has no provider id, but MAY
stamp its own on each delivery as `X-Delivery-Id` (or the Standard Webhooks `Webhook-Id`); when one
is present and no longer than 256 bytes the receiver MUST use it as the delivery id, and MUST
otherwise fall back to the body hash. A sender-asserted id is trusted exactly as much as the body it
travels with: it MUST be scoped to the self-managed webhook it arrived on, so a sender
can only ever collapse its own deliveries. If a non-terminal todo already exists
in the target queue for the derived key, ingestion MUST return the existing todo and create nothing
new. Events MUST additionally dedup on `(source, external_id)` so a duplicate delivery does not
create a second event row.

#### Scenario: Redelivery of the same webhook creates one todo

- **WHEN** a provider redelivers a webhook with the same delivery id (e.g. GitHub retries the same
  `X-GitHub-Delivery`)
- **THEN** the second delivery matches the existing non-terminal todo, returns that todo, and creates
  no duplicate todo or event

#### Scenario: Distinct deliveries create distinct todos

- **WHEN** two deliveries carry different delivery ids
- **THEN** each derives a distinct idempotency key and each creates its own todo

#### Scenario: Generic redelivery with the same delivery id dedups

- **WHEN** a generic sender delivers twice with the same `X-Delivery-Id` (or `Webhook-Id`) and a
  byte-different body (a fresh timestamp, a re-serialized payload)
- **THEN** the second delivery derives the same idempotency key, matches the existing non-terminal
  todo, and creates no duplicate todo or event

#### Scenario: Oversized delivery id falls back to the body hash

- **WHEN** a generic sender's delivery id exceeds 256 bytes
- **THEN** the receiver ignores it and derives the key from the body hash, exactly as if no id had
  been sent

### Requirement: Header and Secret Sanitization Before Persist

Before an event's `headers` are persisted, the receiver MUST redact all signature-, token-, and
secret-bearing header values (`X-Hub-Signature`, `X-Hub-Signature-256`, `X-Gitea-Signature`,
`Stripe-Signature`, `X-Slack-Signature`, `Authorization`, `Cookie`, `X-Api-Key`, and any `?token=`)
to a redaction placeholder. Full signatures, tokens, and secrets MUST NOT be logged or persisted in
full anywhere.

#### Scenario: Sensitive headers are redacted in storage

- **WHEN** a delivery carrying an `Authorization` header and a signature header is accepted
- **THEN** the persisted `headers` JSON shows those values as `«redacted»` and no log line contains
  the full secret

### Requirement: Enqueue Accepted Delivery as Endpoint-Owned Todo

Governing: ADR-0022, ADR-0007. An accepted delivery MUST normalize to the common todo shape and
create a durable todo **pinned to the webhook's owning endpoint** (`endpoint_id` from
`endpoint_webhooks.endpoint_id`), via the shared back-half contract. The todo MUST reference the
persisted event, carry a human-legible title, and be published to the live hub so any attached agent
session is nudged. Todo creation MUST be idempotent on `(endpoint_id, idempotency_key)` among
non-terminal rows (see the [todo-queue spec](../todo-queue/spec.md) "Per-Endpoint Idempotency and
Dedup"). The `idempotency_key` passed to the store MUST be namespaced by the target endpoint id so
that routed fan-out to N endpoints dedups per-target independently.

#### Scenario: Accepted delivery becomes a durable endpoint-owned todo

- **WHEN** a delivery passes verification on a self-managed webhook owned by endpoint A
- **THEN** an event row is inserted, a todo is created pinned to endpoint A (NOT to a global
  queue namespace) referencing that event, the todo is published to the hub if newly created, and
  the response is HTTP 202 with the todo id and queue

The 202 body MUST represent the FULL set of todos the delivery produced, since a routed
delivery yields N:

```json
{
  "todos": [{"id": "td_…", "endpoint_id": "…", "queue": "reviews", "created": true}],
  "created": 1,
  "id": "td_…", "queue": "reviews",
  "verified": true, "trust_mode": "signed"
}
```

`id` and `queue` MUST name the OWNING endpoint's todo, which the resolved target order puts
first — so an unrouted webhook (the common case) returns a body byte-identical to the
pre-fan-out shape. `created` is the count of newly-minted todos; a wholly idempotent
redelivery reports `0` while still returning the existing todos. `endpoint_id` is included
per todo so a fan-out is auditable: the producer already knows the webhook it posted to, and
the response is the only place the delivery says where the work actually landed.

#### Scenario: Routed delivery reports every todo it created

- **WHEN** a webhook routed to endpoints A and B receives one verified delivery
- **THEN** the 202 body's `todos` array MUST hold two entries — one per target, each naming its
  `endpoint_id` — with `created: 2`, and `id`/`queue` MUST name the owner endpoint A's todo

### Requirement: Deterministic Route Fan-Out (Token-Free)

Governing: ADR-0022. A webhook MAY be routed to N target endpoints via the `webhook_routes` table.
When a delivery arrives, the receiver MUST resolve the webhook's target endpoints and create **one
todo per target endpoint**, each pinned to that target, in a single transaction with the event row
(atomic across targets: all commit or none). If no routes are configured, the target set is the
singleton `{webhook.endpoint_id}`. If the resolved target set is EMPTY — the owning endpoint is not
resolvable — the receiver MUST treat the delivery as a misconfiguration and answer HTTP 503 with
nothing persisted, so the producer retries. It MUST NOT answer 202: a delivery that produces no work
anywhere has been dropped, and reporting success for it hides the broken webhook indefinitely.
Routing is deterministic and **token-free**: once a route exists,
every delivery fans out server-side without any agent spending model tokens on a `create_for` call.
Routes are populated by human-approved actions (friending, a future routing verb) — never by a
per-delivery agent decision.

After the target set is resolved and before anything is written, the webhook's routing rules
([SPEC-0020](../event-routing/spec.md)) choose the queue and MAY narrow the delivery to a subset of
those targets. They MAY also drop it: the event is persisted with its trace, no todo is created, and
the response is HTTP 202 with `{"todos": [], "created": 0, "dropped": true}`. Rules MUST NOT add a
target. The top-level `id`/`queue` name the first target's todo, which is the owner's unless a rule
narrowed the owner out.

#### Scenario: Single delivery, two routes, two todos

- **WHEN** a webhook with routes to endpoints A and B receives one verified delivery
- **THEN** exactly two todos are created — one pinned to A, one pinned to B — each independently
  claimable, and the event row commits atomically with both

#### Scenario: No routes configured falls back to owner

- **WHEN** a webhook with no rows in `webhook_routes` receives a delivery
- **THEN** exactly one todo is created, pinned to the webhook's owning endpoint

#### Scenario: Unresolvable target set is refused, not dropped

- **WHEN** a delivery passes verification but the webhook resolves to no target endpoints
- **THEN** the response MUST be HTTP 503, no event row and no todo MUST be persisted, and the
  condition MUST be logged with the webhook id

#### Scenario: Route fan-out is token-free

- **WHEN** a webhook with a route to endpoint B receives a delivery and endpoint B's agent is idle
  (no model invocation in flight)
- **THEN** the todo pinned to endpoint B is still created and doorbelled; routing required no agent
  action and zero model tokens

### Requirement: Error Handling Standards

Ingestion errors MUST be wrapped with context at each boundary (read, verify, insert event, create
todo) and MUST NOT be silently swallowed. Domain rejections (bad signature, unknown ingest
token, oversized body) MUST map to specific HTTP status codes (401 / 404 /
413) rather than a generic 500. Unexpected internal failures MUST return HTTP 500 without leaking
internal detail, and MUST be logged with structured context.

#### Scenario: Store failure returns a clean 500

- **WHEN** the event insert or todo creation fails for an internal reason (e.g. database error)
- **THEN** the response is HTTP 500 with a generic message, and the failure is logged with structured
  context (never the payload secret)

## Security Requirements

### Authentication

All ingestion endpoints authenticate the *delivery*, not a Switchboard human/agent session. The
only ingestion endpoint is the self-managed webhook receiver.

| Endpoint | Auth | Justification |
|----------|------|---------------|
| `POST /webhooks/w/{ingest_token}` (signed source type) | Required (signed) | Per-provider HMAC-SHA256 over raw body verified constant-time against the secret switchboard minted and holds; fail ⇒ 401, no persist |
| `POST /webhooks/w/{ingest_token}` (token source type) | Required (token) | The unguessable ingest token in the URL authenticates the caller; the body is not signature-verified; fail (unknown token) ⇒ 404 |

### Rate Limiting

Rate limiting is deferred at the application layer for the MVP: the service is loopback/trusted-network
bound behind a reverse proxy (Caddy), where connection-level and proxy-level throttling apply. When
exposed publicly, per-source-IP rate limiting on the `/webhooks/*` routes SHOULD be added at the
proxy or middleware layer. Body-size bounding (below) provides the primary abuse mitigation in the
MVP.

### Security Headers

All HTTP responses MUST include:
- `Content-Security-Policy`: `default-src 'none'` (webhook endpoints return JSON, never HTML, so no
  script/style/image sources are needed)
- `X-Frame-Options`: DENY
- `X-Content-Type-Options`: nosniff
- `Referrer-Policy`: strict-origin-when-cross-origin

### Request Body Size Limits

All endpoints accepting bodies MUST bound them with a limiting reader before verification. Default
limit: **5 MiB** (matches `maxBody` in `internal/ingest/ingest.go`). A body exceeding the limit MUST
be rejected (HTTP 413) without persisting. The limiting reader MUST wrap the body so the raw bytes
used for HMAC verification are the same bounded bytes.

### CSRF Protection

Webhook endpoints are machine-to-machine POSTs authenticated by signature or shared-secret token, not
by an ambient browser cookie, so classic form-CSRF does not apply: a cross-site request cannot forge a
valid HMAC or present the shared secret. No cookie or session credential is honored on `/webhooks/*`.

### Redirect Validation

No webhook endpoint issues a redirect and none accepts a user-supplied redirect target. No
user-supplied redirects exist in this capability.
