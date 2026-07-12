---
status: implemented
date: 2026-07-06
implements: [ADR-0003, ADR-0014]
---

# SPEC-0001: Webhook Ingestion (Push Adapters)

## Overview

Webhook ingestion is Switchboard's **push** ingestion family: inbound HTTP endpoints that receive
provider deliveries, verify their trust (per-provider signature or a shared-secret token), normalize
the delivery, persist an event for history, and enqueue a durable todo. It realizes the webhook half
of [ADR-0003](../../../adrs/ADR-0003-per-provider-ingestion-and-trust-model.md) (the `webhook`
provider family and its ordered trust modes `signed` / `token` / `open`) and the push family of
[ADR-0014](../../../adrs/ADR-0014-ingestion-adapters-push-pull.md) (push vs. pull adapters sharing
one normalization contract into the todo queue).

A webhook is *push*: the sender initiates an HTTP request, and the "ack" is the HTTP response.
Verification happens at receive, before any parsing, against the raw request body. The three trust
modes are honest and ordered — `signed` (HMAC verified, `verified=true`) is strictly stronger than
`token` (shared secret authenticates the caller, not the body, `verified=false`), which is stronger
than `open` (no check, `verified=false`, off by default). The trust mode is stored on every event and
surfaced everywhere so a human never has to guess whether a delivery was authenticated.

The reference implementation is the signed GitHub adapter (`POST /webhooks/github`,
`internal/ingest/ingest.go`); Stripe, Slack, and the token/open generic endpoint follow the same
contract. This capability covers *only* the push family; pull (queue) ingestion is SPEC-0002.

## Requirements

### Requirement: Signed Webhook Verification

For a webhook provider declared `signed` (GitHub, Stripe, Slack), the receiver MUST verify a
cryptographic signature over the **raw request body** before parsing the payload, using a
constant-time comparison. A missing, malformed, or failing signature MUST return HTTP 401 and MUST
NOT persist the payload; only a redacted rejection line MAY be logged (provider, event type if
known, source IP — never the secret or full signature). A verified request MUST persist the event
with `trust_mode='signed'`, `verified=true`, and a `verify_detail` naming the scheme (e.g.
`hmac-sha256 ok`). Signature comparison MUST use a constant-time primitive (`hmac.Equal`), never a
plain byte equality.

#### Scenario: Valid GitHub signature is accepted

- **WHEN** a `POST /webhooks/github` request arrives whose `X-Hub-Signature-256` header matches the
  HMAC-SHA256 of the raw body under the configured secret
- **THEN** the event is persisted with `source='github'`, `family='webhook'`,
  `trust_mode='signed'`, `verified=true`, `verify_detail='hmac-sha256 ok'`, a todo is created, and
  the response is HTTP 202 with `{id, queue, verified: true}`

#### Scenario: Missing or invalid signature is rejected without persisting

- **WHEN** a signed-provider request arrives with a missing, malformed, or non-matching signature
- **THEN** the response is HTTP 401, no event row and no todo are written, and a redacted rejection
  line (no secret, no full signature) MAY be logged

#### Scenario: Signature secret not configured

- **WHEN** a signed-provider request arrives but no signing secret is configured for that provider
- **THEN** the request is rejected without persisting (HTTP 503 for a globally-required-but-unset
  secret, or HTTP 404/403 for a provider that is not configured) and no signature is compared

### Requirement: Replay-Window Enforcement for Timestamped Signatures

For signed providers whose scheme signs a timestamp (Stripe `t=`, Slack
`X-Slack-Request-Timestamp`), the receiver MUST reject a delivery whose signed timestamp is outside
a freshness tolerance (default 300 seconds) even when the HMAC is otherwise valid, returning HTTP
401 and persisting nothing. For providers whose scheme does not sign a timestamp (GitHub), the
receiver MUST NOT fabricate a replay window.

#### Scenario: Stale Stripe timestamp is rejected

- **WHEN** a Stripe delivery presents a valid `v1=` HMAC but a `t=` timestamp more than the
  configured tolerance from now
- **THEN** the response is HTTP 401 and no event is persisted

#### Scenario: GitHub delivery has no timestamp check

- **WHEN** a GitHub delivery with a valid signature arrives
- **THEN** it is accepted regardless of age (GitHub does not sign a timestamp) and no freshness
  window is applied

### Requirement: Shared-Secret Token Authentication for Unsigned Webhooks

For webhook providers with no signing scheme (Docker Hub, homelab/self-hosted senders) served via
the generic endpoint, the receiver MUST require a configured **shared-secret token** the caller
presents on every request, compared in constant time. The token SHOULD be presented in an HTTP
header (`Authorization: Bearer <token>` or a dedicated token header) and MAY fall back to a URL
token (`?token=` or a path token) for senders that can only be configured with a URL. A generic
provider MUST be disabled until a token is configured, and MUST reject a request with a
missing/incorrect token with HTTP 403 without persisting. An accepted token request MUST persist the
event with `trust_mode='token'`, `verified=false`, and a `verify_detail` that states the caller is
authenticated but the body is not verified. `token` MUST NOT ever be presented as `signed`.

#### Scenario: Correct token is accepted as token trust

- **WHEN** a `POST /webhooks/generic/{name}` request presents the configured shared secret (header
  or URL fallback)
- **THEN** the event is persisted with `family='webhook'`, `trust_mode='token'`, `verified=false`, a
  todo is created, and the response is HTTP 202

#### Scenario: Missing or wrong token is rejected

- **WHEN** a generic request presents a missing or incorrect token
- **THEN** the response is HTTP 403 and no event or todo is written

#### Scenario: Generic provider disabled until token set

- **WHEN** an operator configures a generic provider without a token
- **THEN** the provider is disabled and every request to it is rejected (403) until a token is set

### Requirement: Explicit Open Trust Mode

A provider MAY be explicitly set to `open` (no verification) only by deliberate operator opt-in.
Open providers MUST default to disabled, MUST persist events with `trust_mode='open'`,
`verified=false`, and a plain `verify_detail` (e.g. `open — no verification`), and MUST be labeled as
the loudest/weakest tier everywhere. The system MUST NOT default any provider to `open`.

#### Scenario: Open provider only exists when explicitly created

- **WHEN** no operator has explicitly created an `open` provider
- **THEN** no `open` provider accepts deliveries; an unknown provider name returns HTTP 404

#### Scenario: Open delivery is labeled unverified

- **WHEN** a delivery arrives at an explicitly-created `open` provider
- **THEN** the event is persisted with `trust_mode='open'`, `verified=false` and labeled as
  unverified in the API and UI

### Requirement: Idempotency Key Extraction and Dedup

Each accepted webhook delivery MUST derive an idempotency key and use it to dedup redeliveries into a
single todo. The key SHOULD be derived from a provider delivery id where one exists (GitHub
`X-GitHub-Delivery`, Stripe event `id`) and MUST fall back to a body hash where the provider supplies
no delivery id (Slack, generic). If a non-terminal todo already exists in the target queue for the
derived key, ingestion MUST return the existing todo and create nothing new. Events MUST additionally
dedup on `(source, external_id)` so a duplicate delivery does not create a second event row.

#### Scenario: Redelivery of the same webhook creates one todo

- **WHEN** a provider redelivers a webhook with the same delivery id (e.g. GitHub retries the same
  `X-GitHub-Delivery`)
- **THEN** the second delivery matches the existing non-terminal todo, returns that todo, and creates
  no duplicate todo or event

#### Scenario: Distinct deliveries create distinct todos

- **WHEN** two deliveries carry different delivery ids
- **THEN** each derives a distinct idempotency key and each creates its own todo

### Requirement: Header and Secret Sanitization Before Persist

Before an event's `headers` are persisted, the receiver MUST redact all signature-, token-, and
secret-bearing header values (`X-Hub-Signature`, `X-Hub-Signature-256`, `Stripe-Signature`,
`X-Slack-Signature`, `Authorization`, `Cookie`, `X-Api-Key`, and any `?token=`) to a redaction
placeholder. Full signatures, tokens, and secrets MUST NOT be logged or persisted in full anywhere.

#### Scenario: Sensitive headers are redacted in storage

- **WHEN** a delivery carrying an `Authorization` header and a signature header is accepted
- **THEN** the persisted `headers` JSON shows those values as `«redacted»` and no log line contains
  the full secret

### Requirement: Enqueue Accepted Delivery as Todo

An accepted delivery MUST normalize to the common todo shape and create a durable todo in the target
queue via the shared back-half contract, so a push delivery and a pull delivery of the same logical
event yield the identical todo. The todo MUST reference the persisted event, carry a human-legible
title, and be published to the live hub so any attached agent session is nudged. Todo creation MUST
be idempotent on `(queue, idempotency_key)` among non-terminal rows.

#### Scenario: Accepted delivery becomes a durable todo

- **WHEN** a delivery passes verification
- **THEN** an event row is inserted, a todo is created in the configured queue referencing that
  event, the todo is published to the hub if newly created, and the response is HTTP 202 with the
  todo id and queue

### Requirement: Error Handling Standards

Ingestion errors MUST be wrapped with context at each boundary (read, verify, insert event, create
todo) and MUST NOT be silently swallowed. Domain rejections (bad signature, bad token, disabled
provider, unknown provider, oversized body) MUST map to specific HTTP status codes (401 / 403 / 404 /
413) rather than a generic 500. Unexpected internal failures MUST return HTTP 500 without leaking
internal detail, and MUST be logged with structured context.

#### Scenario: Store failure returns a clean 500

- **WHEN** the event insert or todo creation fails for an internal reason (e.g. database error)
- **THEN** the response is HTTP 500 with a generic message, and the failure is logged with structured
  context (never the payload secret)

## Security Requirements

### Authentication

All ingestion endpoints authenticate the *delivery*, not a Switchboard human/agent session: `signed`
endpoints verify a per-provider HMAC over the raw body, `token` endpoints require a shared-secret
token, and `open` endpoints are an explicit trusted-network opt-in. There are no anonymous
state-changing endpoints in this capability except the deliberately-opted-in `open` provider.

| Endpoint | Auth | Justification |
|----------|------|---------------|
| `POST /webhooks/github` | Required (signed) | HMAC-SHA256 over raw body verified constant-time; fail ⇒ 401, no persist |
| `POST /webhooks/{provider}` (stripe, slack) | Required (signed) | Per-provider HMAC + timestamp freshness window; fail ⇒ 401, no persist |
| `POST /webhooks/generic/{name}` | Required (token) | Shared-secret token authenticates the caller; disabled until token set; fail ⇒ 403 |
| `POST /webhooks/generic/{name}` (open mode) | Public | Explicit operator opt-in only, off by default, trusted-network only, labeled loudest tier |

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
