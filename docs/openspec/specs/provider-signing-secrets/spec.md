---
status: draft
date: 2026-09-22
implements: [ADR-0037]
requires: [SPEC-0001, SPEC-0006, SPEC-0020]
---

# SPEC-0032: Provider-Issued Signing Secrets, Slack URL Verification, and the Linear and Plain Kinds

## Overview

Some providers issue their own webhook signing secret and will not accept one. This spec lets a
self-managed webhook hold a **provider-issued** secret, written once and never read back; verifies
Stripe, Slack, Linear and Plain as their vendors document today; answers Slack's URL-verification
handshake; and keys each provider's retries on an id it signs or promises is stable. See
[ADR-0037](../../../adrs/ADR-0037-provider-issued-signing-secrets.md).

It amends, without editing:

* [SPEC-0006](../agent-tools/spec.md) REQ "Webhook Self-Management Within a Vended Ceiling" and REQ
  "Switchboard Owns Secrets, Verification, and Idempotency": provider-origin types are not minted,
  start `awaiting_secret`, and take a supplied secret (REQ-1 to REQ-4 below).
* [SPEC-0001](../webhook-ingestion/spec.md) REQ "Signed Webhook Verification", REQ "Replay-Window
  Enforcement for Timestamped Signatures" and REQ "Idempotency Key Extraction and Dedup": new schemes,
  per-scheme windows, signed delivery ids (REQ-5 to REQ-9).
* [SPEC-0020](../event-routing/spec.md) REQ "Routing Envelope": event kinds for the new sources
  (REQ-10).

Priority: provider-issued secret input and the Stripe and Slack fixes (REQ-1 to REQ-9) are P1; the
Linear and Plain kinds (REQ-10, REQ-11) are P2.

Terms:

* **Minted origin**: Switchboard generates the secret and reveals it once (`github`, `gitea`, `cairn`).
* **Provider origin**: the provider generates the secret and the tenant supplies it (`stripe`, `slack`,
  `linear`, `plain`).

## Requirements

### REQ-1: Secret Origin per Source Type

Each signed source type MUST declare a secret origin, fixed in code:

| Source | Trust mode | Origin |
|---|---|---|
| `github`, `gitea`, `cairn` | signed | minted |
| `stripe`, `slack`, `linear`, `plain` | signed | provider |
| `generic` | token | none |

For a minted-origin type, `create_webhook` and `rotate_webhook` MUST behave exactly as SPEC-0006
specifies today, and a caller-supplied secret MUST be refused with `invalid_argument`.

#### Scenario: Supplied secret for a forge

- **WHEN** an agent calls `create_webhook {source_type: github, target_queue: forge, signing_secret: "x"}`
- **THEN** the call fails with `invalid_argument` and no webhook is created

### REQ-2: Provider-Origin Webhooks Await a Secret

For a provider-origin type, `create_webhook` MUST NOT mint a secret. Without a supplied secret it MUST
create the webhook in state `awaiting_secret` and return its `ingest_url`, `trust_mode = signed`,
`secret_origin = provider` and `secret_set = false`, with no `signing_secret` field.

A delivery to an `awaiting_secret` webhook MUST be answered `503` with the body
`{"error": "webhook awaiting signing secret"}`, MUST persist nothing (no event row, no todo), and MUST
be counted as a rejection with reason `awaiting_secret`.

`list_webhooks` MUST report `secret_origin`, `secret_set`, and, when set, the secret fingerprint (first
12 hex characters of SHA-256) and `secret_set_at`.

#### Scenario: Stripe webhook before its secret

- **GIVEN** an agent created a `stripe` webhook with no secret
- **WHEN** Stripe delivers an event to its URL
- **THEN** the response is `503`, nothing is persisted, and Stripe retries later

#### Scenario: List shows the state

- **WHEN** the agent lists webhooks
- **THEN** the Stripe webhook shows `secret_origin: provider`, `secret_set: false`

### REQ-3: Supplying a Provider-Issued Secret

A provider-issued secret MUST be settable, for a webhook in the caller's own scope, by:

1. the web UI, on the webhook's row, by the owning human or, for a team-owned endpoint, a role
   SPEC-0033 (Teams and tenancy, in flight) allows to configure the team;
2. the MCP verb `set_webhook_secret {webhook_id, signing_secret, keep_previous_for?}`, a member of the
   webhook verb family, enforced by the scope guard, and absent from the basics and quick vends;
3. `create_webhook` with `signing_secret`, for a provider-origin type only.

The secret MUST be at least 16 bytes and at most 512 bytes of printable ASCII, and MUST pass the
per-kind format check where one applies (a `stripe` secret MUST start with `whsec_`). A valid secret
MUST move the webhook out of `awaiting_secret`.

The secret MUST be stored through the `internal/cred` envelope. When no secret encryption key is
configured, every write of a provider-issued secret MUST be refused with `encryption_required`. The
secret MUST NOT appear in any response, page, log line, metric label, routing trace, event row or error
message; responses MUST report only `secret_set: true`, the fingerprint and `secret_set_at`.

`set_webhook_secret` on a minted-origin webhook, or on another endpoint's webhook, MUST fail with
`invalid_argument` or `not_found` respectively.

#### Scenario: Owner pastes Stripe's secret in the UI

- **WHEN** the owner pastes the `whsec_` value Stripe displayed into the webhook's secret field
- **THEN** the page shows `secret set` with a fingerprint, the field is empty on reload, and the next
  signed Stripe delivery is accepted

#### Scenario: No encryption key

- **GIVEN** `SWITCHBOARD_SECRET_ENCRYPTION_KEY` is unset
- **WHEN** a secret is set on a Slack webhook by any path
- **THEN** it fails with `encryption_required` and the webhook stays `awaiting_secret`

#### Scenario: Another endpoint's webhook

- **WHEN** endpoint F calls `set_webhook_secret` with endpoint E's webhook id
- **THEN** the call fails with `not_found` and E's webhook is unchanged

#### Scenario: Malformed Stripe secret

- **WHEN** a Stripe secret without the `whsec_` prefix is supplied
- **THEN** the call fails with `invalid_argument` naming `signing_secret`, without echoing the value

### REQ-4: Rotation and Overlap

Setting a new provider-issued secret MUST replace the current one. With `keep_previous_for` (a
duration, at most 24 hours; the web UI offers the same choice) the replaced secret MUST remain valid for
verification until that time passes, after which it MUST be erased. Without it, the old secret MUST stop
verifying immediately.

During an overlap, a delivery MUST be accepted when its signature verifies under either secret. For
Stripe, where one header can carry several `v1` values, any `v1` verifying under either secret MUST be
accepted.

`rotate_webhook` on a provider-origin webhook MUST rotate only the ingest token (a new URL) and MUST
NOT change or erase the secret; its result MUST say `secret_unchanged: true`.

#### Scenario: Rolling Stripe's secret

- **GIVEN** a Stripe webhook with secret S1
- **WHEN** the owner sets S2 with `keep_previous_for: 24h`
- **THEN** deliveries signed with S1 or S2 are accepted for 24 hours, and after that only S2

#### Scenario: Rotating the URL keeps the secret

- **WHEN** an agent calls `rotate_webhook` on a Slack webhook
- **THEN** the ingest URL changes, the secret fingerprint is unchanged, and the result carries
  `secret_unchanged: true`

### REQ-5: Signature Verification per Scheme

A delivery to a signed webhook MUST be verified per its source type, over the exact raw request body,
comparing in constant time:

| Source | Header | Signed content | Encoding |
|---|---|---|---|
| `stripe` | `Stripe-Signature: t=<unix>,v1=<hex>[,v1=<hex>…]` | `<t>` + `.` + body | hex; only `v1` entries count; other schemes (`v0`) are ignored |
| `slack` | `X-Slack-Signature: v0=<hex>` and `X-Slack-Request-Timestamp` | `v0:` + timestamp + `:` + body | hex |
| `linear` | `Linear-Signature` | body | hex |
| `plain` | `Plain-Request-Signature` | body | hex |

All four use HMAC-SHA256 keyed with the webhook's secret (or, during an overlap, either secret). A
missing, malformed or non-matching signature MUST be `401` with nothing persisted, logged with the
webhook id and reason and never the signature or secret. The existing `github`, `gitea` and `cairn`
schemes are unchanged.

#### Scenario: Valid Linear delivery

- **GIVEN** a Linear webhook with secret S
- **WHEN** a delivery arrives whose `Linear-Signature` is the hex HMAC-SHA256 of its body under S and
  whose body `webhookTimestamp` is 5 seconds old
- **THEN** it is persisted with `verified = true` and routed

#### Scenario: Stripe v0 only

- **WHEN** a Stripe header carries a valid `v0` signature and no valid `v1`
- **THEN** the delivery is `401`

### REQ-6: Replay Windows on Signed Timestamps

After the signature verifies, the signed timestamp MUST be within the source's window of the server's
clock, in either direction:

| Source | Timestamp | Window |
|---|---|---|
| `stripe` | `t` in `Stripe-Signature` | 5 minutes |
| `slack` | `X-Slack-Request-Timestamp` (part of the signed base string) | 5 minutes |
| `linear` | body `webhookTimestamp`, Unix milliseconds | 60 seconds |
| `plain` | body `webhookMetadata.webhookDeliveryAttemptTimestamp`, ISO 8601 UTC | 5 minutes |

A delivery outside its window MUST be `401` with reason `stale_timestamp`, and nothing persisted. Unsigned
timestamp headers (`Linear-Timestamp`, `Plain-Webhook-Delivery-Attempt-Timestamp`) MUST NOT be used for
this check. A missing or unparseable signed timestamp MUST be `401`.

#### Scenario: Linear header lies

- **WHEN** a Linear delivery's body `webhookTimestamp` is 90 seconds old and its `Linear-Timestamp`
  header is current
- **THEN** the delivery is `401` with reason `stale_timestamp`

#### Scenario: Future clock skew

- **WHEN** a Slack request timestamp is 6 minutes in the future
- **THEN** the delivery is `401`

### REQ-7: Signature Replay Guard

For the timestamped schemes above, Switchboard MUST record `(webhook id, SHA-256 of the signature
header value)` for every accepted delivery, retained for the source's replay window. A delivery whose
pair is already recorded MUST be answered `202` with `{"created": 0, "duplicate": true}` and MUST create
no event row and no todo. The record MUST be shared across instances.

#### Scenario: Replay under a fresh delivery id

- **GIVEN** a Linear delivery was accepted 20 seconds ago
- **WHEN** the byte-identical request is sent again with a different `Linear-Delivery` header
- **THEN** the response is `202` with `duplicate: true` and nothing new exists

### REQ-8: Delivery Ids for Dedup

The idempotency key for these sources MUST be the webhook id, a colon, and:

| Source | Delivery id | Fallback |
|---|---|---|
| `stripe` | body `id` (the event id) | body hash |
| `slack` | body `event_id` for `event_callback` | body hash |
| `linear` | `Linear-Delivery` header | body hash |
| `plain` | body `id` | body hash |

Body fields MUST be read from the verified body. A delivery id longer than 256 bytes MUST be ignored in
favour of the fallback. The `X-Slack-Retry-Num` and `X-Slack-Retry-Reason` headers, when present, MUST be
kept in the sanitized headers so a retry is visible on the event.

#### Scenario: Stripe retry

- **WHEN** Stripe retries event `evt_1` with a new `t` and signature and the same body
- **THEN** no second todo is created

#### Scenario: Plain retry with new metadata

- **WHEN** Plain retries event `id = ev_9` with a new `webhookDeliveryAttemptId` and timestamp in the
  body
- **THEN** no second todo is created

### REQ-9: Slack URL Verification

For a `slack` webhook, after signature and timestamp verification succeed, a JSON body whose `type` is
`url_verification` MUST be answered `200` with `Content-Type: application/json` and body
`{"challenge": "<challenge>"}`. The challenge MUST be a string of at most 256 printable ASCII characters,
else the request is `400`. Switchboard MUST NOT write an event row, a todo, a routing trace or a
doorbell for it, and MUST record `handshake_at` on the webhook, shown on the endpoint card and in
`list_webhooks`.

A `url_verification` that fails verification MUST be `401`. A `url_verification` to an
`awaiting_secret` webhook MUST be `503` (REQ-2).

#### Scenario: Handshake

- **GIVEN** a Slack webhook with the app's signing secret set
- **WHEN** Slack posts a signed `url_verification` with `challenge = 3eZbrw1aBm2rZgRNFdxV2595E9CY3gmdALWMmHkvFXO7tYXAYM8P`
- **THEN** the response is `200` with that challenge in JSON, and no event or todo exists

#### Scenario: Forged handshake

- **WHEN** a `url_verification` arrives with an invalid signature
- **THEN** the response is `401` and no challenge is echoed

### REQ-10: Linear and Plain Kinds

`linear` and `plain` MUST be supported source types with `trust_mode = signed` and provider origin, and
MUST be offerable in a vend's allowed source types (the vend wizard and the operator API).

The routing envelope's event kind MUST be, from the verified body: `linear` → `type` + `.` + `action`
(for example `Issue.create`, `Comment.create`); `plain` → `type` (for example `thread.thread_created`);
`stripe` → `type`; `slack` → the inner `event.type` for `event_callback`, else the top-level `type`.
Todo titles MUST be legible per kind (for example `linear Issue.create: ENG-123 Fix login`,
`plain thread.thread_created`), built from sender-controlled fields escaped as titles are today.

#### Scenario: Vend with Linear

- **WHEN** a human vends an endpoint whose allowed source types include `linear`, and its agent creates a
  `linear` webhook
- **THEN** the webhook is created `awaiting_secret` and is verified with REQ-5 once its secret is set

#### Scenario: Routing on a Linear kind

- **GIVEN** a rule `{expr: ".kind == \"Issue.create\"", action: {queue: "triage"}}`
- **WHEN** a verified Linear issue-created delivery arrives
- **THEN** its todo is on `triage`

### REQ-11: Linear and Plain Subjects

For `linear` issue and comment events, and for `plain` thread events, Switchboard SHOULD parse a subject
(SPEC-0020 REQ "Issue Envelope Projection") from the verified body so routing rules and work orders can
match on it: for Linear the issue identifier, title, URL, labels and team key; for Plain the thread id,
title and status. Reply addresses for these kinds belong to SPEC-0028 (reply to source, in flight).

#### Scenario: Linear subject

- **WHEN** a Linear `Issue.update` for `ENG-123` arrives verified
- **THEN** the envelope's subject carries identifier `ENG-123`, its title and its URL

### REQ-12: Error Handling Standards

Errors MUST be wrapped with context at each boundary. Sentinels MUST exist for `awaiting_secret`,
`stale_timestamp`, `bad_signature`, `encryption_required` and `invalid_argument`, and each rejection MUST
increment the verify-failure counter SPEC-0023 REQ-4 defines, with its reason. Logs MUST carry the
webhook id, source and reason, never a signature, secret or body.

#### Scenario: Rejection is counted

- **WHEN** a Stripe delivery fails the timestamp window
- **THEN** `switchboard_webhook_verify_failures_total{provider="stripe",reason="stale_timestamp"}`
  increments and the log line carries no signature

## Security Requirements

### Authentication

| Surface | Auth | Justification |
|---|---|---|
| `POST /webhooks/w/{token}` | Public, then verified | Providers cannot hold a Switchboard credential; the unguessable token routes and the per-kind HMAC authenticates. Unverified deliveries are refused. |
| MCP `create_webhook`, `set_webhook_secret`, `rotate_webhook`, `list_webhooks` | Required | Vended endpoint credential with the verb in scope. |
| Web UI secret field | Required | Human session with CSRF; owner or team role. |
| `PUT /api/v1/webhooks/{id}/secret` | Required | Operator OAuth bearer; owner or team role; `404` outside the caller's scopes. |

### Rate Limiting

The ingest route keeps its existing limits. `set_webhook_secret` MUST be limited per endpoint (10 per
hour), and the web and API secret writes per human (10 per hour), to bound guessing at format checks.

### Security Headers

Web and API responses MUST carry the standard operator headers. The ingest route's responses carry the
existing JSON headers; the challenge response is `application/json` with `X-Content-Type-Options:
nosniff`.

### Request Body Size Limits

The ingest route keeps its existing body limit, applied before verification. Secret-write bodies MUST be
bounded at 4 KiB.

### CSRF Protection

The web UI secret field MUST use the existing CSRF protection. The ingest route and the operator API are
not cookie-authenticated.

### Redirect Validation

This capability issues no redirects to caller-supplied URLs.
