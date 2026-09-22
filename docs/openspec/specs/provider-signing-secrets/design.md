---
status: draft
date: 2026-09-22
implements: [ADR-0037]
---

# Design: Provider-Issued Signing Secrets, Slack URL Verification, and the Linear and Plain Kinds

## Context

[SPEC-0032](spec.md) fixes a gap #291 left: with the instance-wide receivers gone, the only ingestion
path is `create_webhook`, which mints every signed secret, and Stripe, Slack, Linear and Plain issue
their own. The code this touches:

* `internal/mcp/webhooks.go`: `webhookTrustModes`, `createWebhookIn`, `createWebhookTool`,
  `rotateWebhookTool`, `mintSecretFor`.
* `internal/store/webhooks.go`: `CreateWebhook`, `RotateWebhookSecret`, `GetWebhookSecretByToken`,
  `sealSecret` / `openSecret`.
* `internal/ingest/selfmanaged.go`: `SelfManaged` (delivery id selection, verification, routing) and
  `verifySelfManagedSigned`.
* `internal/ingest/verify.go`: `verifyStripe`, `verifySlack`, `freshTimestamp`, `idempotencyKey`,
  moved verbatim from the removed receivers and still correct.
* `internal/routing`: `EventKind`, `SubjectOf`.

## Goals / Non-Goals

### Goals

- Stripe and Slack work again, per tenant, fully verified.
- Linear and Plain become native signed kinds.
- Vendor secrets are write-only, encrypted, and can be entered by a human without an agent in the loop.
- Retries dedupe on stable ids; an unsigned header cannot mint a duplicate.

### Non-Goals

- Restoring any env-configured secret or instance-wide receiver.
- Slack `ssl_check` (slash-command certificate checks), interactivity payloads, and slash commands:
  form-encoded Slack traffic is a separate kind if anyone needs it.
- Plain's mTLS and URL basic-auth options.
- Provider IP allowlisting.
- Reply addresses for Linear and Plain (SPEC-0028's follow-up).

## Decisions

### A secret-origin table beside the trust-mode table

**Choice**: extend `webhookTrustModes` into a per-source descriptor:

```go
type sourceKind struct {
    TrustMode    string        // signed | token
    Origin       string        // minted | provider | none
    ReplayWindow time.Duration // 0 = scheme has no signed timestamp
    FormatCheck  func(string) error
}
```

**Rationale**: every per-kind fact lives in one place, so adding a provider is one row plus a verifier,
and the MCP layer, the store and the receiver read the same answer.

### `awaiting_secret` is derived, not a new column

**Choice**: a provider-origin webhook with a NULL `signing_secret` is awaiting its secret. The receiver
already answers `503 webhook not configured` for a signed webhook with no secret; this changes the body
to name the state and adds the rejection reason.

**Rationale**: the invariant "a signed webhook without a secret refuses deliveries" already exists and
is tested; making the state a function of it avoids two sources of truth. The CHECK in
`CreateWebhook` (`signing_secret` non-NULL only when signed) is relaxed to allow NULL for provider-origin
signed webhooks.

### Previous secret in its own columns

**Choice**: `previous_signing_secret` (enc:v1) and `previous_secret_expires_at` on `endpoint_webhooks`,
erased by the retention sweep once expired and on the next secret write.

**Rationale**: overlap is at most one old secret for at most 24 hours; a side table would be heavier
than the feature.

### Replay guard in a small table

**Choice**: `webhook_signatures_seen (webhook_id, sig_sha256, seen_at)` with a primary key on the first
two, inserted with `ON CONFLICT DO NOTHING` in the delivery's transaction, pruned by the retention sweep
past each source's window.

**Rationale**: it must hold across instances, and it must be atomic with the event write so a crash
between the two cannot both accept and forget. Only timestamped schemes use it, and only for their
window, so it stays small.

**Alternatives considered**:
- In-memory LRU: rejected, not shared across instances.
- Rely on delivery ids alone: rejected for Linear, whose delivery id is an unsigned header.

### Delivery ids from the verified body

**Choice**: `SelfManaged`'s delivery-id block gains a per-source extractor that runs **after**
verification on the verified body: Stripe `id`, Slack `event_id`, Plain `id`, and Linear's
`Linear-Delivery` header.

**Rationale**: today the id is chosen before verification from headers; for body ids that ordering has
to flip, and reading ids only from verified bytes means a forger cannot choose the key. The received-lane
card (SPEC-0015) keeps using the pre-verification body hash, as it does now.

### Slack handshake short-circuits before routing

**Choice**: in `SelfManaged`, after verification and the replay check, a `slack` body with
`"type": "url_verification"` returns the challenge and updates `handshake_at`, before `ResolveWebhookTargets`
and before any insert.

**Rationale**: a handshake must not become work, must not consume a dedup slot, and must not depend on
routing rules the owner has not written yet.

### Human entry first

**Choice**: the endpoint card's webhook row gets a password-type secret field (never pre-filled) with a
"keep the old secret for" select (none, 1 hour, 24 hours), and the operator API gets
`PUT /api/v1/webhooks/{id}/secret`. The MCP verb exists for automation but is not in basics vends.

**Rationale**: a vendor secret pasted through an agent passes through a model transcript; the UI path
avoids that entirely.

## Schema

The migration number is the next free one when the story lands.

```sql
ALTER TABLE endpoint_webhooks
    ADD COLUMN secret_origin text NOT NULL DEFAULT 'minted',   -- minted|provider|none
    ADD COLUMN secret_set_at timestamptz,
    ADD COLUMN previous_signing_secret text,                   -- enc:v1 only
    ADD COLUMN previous_secret_expires_at timestamptz,
    ADD COLUMN handshake_at timestamptz;
UPDATE endpoint_webhooks SET secret_origin = 'provider' WHERE source_type IN ('stripe', 'slack');
UPDATE endpoint_webhooks SET secret_origin = 'none' WHERE trust_mode <> 'signed';

CREATE TABLE webhook_signatures_seen (
    webhook_id  uuid NOT NULL REFERENCES endpoint_webhooks(id) ON DELETE CASCADE,
    sig_sha256  text NOT NULL,
    seen_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (webhook_id, sig_sha256)
);
```

Existing `stripe` and `slack` rows hold a minted secret that can never verify. The migration marks them
`provider`, and a follow-up statement NULLs their `signing_secret`, which puts them in
`awaiting_secret`: honest, where today they fail every delivery with `401`.

## MCP Surface

`create_webhook` input gains an optional field:

```json
{"source_type": "slack", "target_queue": "inbox", "signing_secret": "…"}
```

Its result, for a provider-origin type:

```json
{"webhook_id": "…", "ingest_url": "https://sb.example.net/webhooks/w/…",
 "source_type": "slack", "target_queue": "inbox", "trust_mode": "signed",
 "secret_origin": "provider", "secret_set": true, "secret_fingerprint": "7cdbdb2b6b73"}
```

New verb:

```json
{"name": "set_webhook_secret",
 "description": "Set the signing secret a provider issued (Stripe, Slack, Linear, Plain) on one of this endpoint's webhooks. Write-only: the secret is never returned. Prefer having the owner paste it in the web UI.",
 "inputSchema": {"type": "object", "required": ["webhook_id", "signing_secret"], "properties": {
   "webhook_id": {"type": "string"},
   "signing_secret": {"type": "string", "minLength": 16, "maxLength": 512},
   "keep_previous_for": {"type": "string", "description": "Go duration, at most 24h"}}}}
```

`list_webhooks` rows gain `secret_origin`, `secret_set`, `secret_fingerprint`, `secret_set_at`,
`handshake_at`.

## Verification Flow

```mermaid
sequenceDiagram
  autonumber
  participant P as Provider
  participant R as SelfManaged
  participant DB as Postgres
  P->>R: POST /webhooks/w/token (raw body)
  R->>DB: webhook + current/previous secret (decrypted)
  alt no secret (awaiting_secret)
    R-->>P: 503, nothing written
  end
  R->>R: HMAC per kind over raw body (current, then previous)
  R->>R: signed timestamp inside the kind's window
  alt slack url_verification
    R->>DB: UPDATE handshake_at
    R-->>P: 200 {"challenge": …}
  else ordinary delivery
    R->>R: delivery id from the verified body (or Linear-Delivery)
    R->>DB: BEGIN; INSERT signature seen ON CONFLICT DO NOTHING
    alt already seen
      R->>DB: ROLLBACK
      R-->>P: 202 duplicate
    else first time
      R->>DB: event + routed todos; COMMIT
      R-->>P: 202 todos
    end
  end
```

## Linear and Plain Details

| | Linear | Plain |
|---|---|---|
| Secret | per webhook, shown on the webhook's page | one per workspace, Settings → Request signing |
| Signature | `Linear-Signature`, hex HMAC-SHA256 of raw body | `Plain-Request-Signature`, hex HMAC-SHA256 of raw body |
| Signed freshness | body `webhookTimestamp` (ms), 60 s | body `webhookMetadata.webhookDeliveryAttemptTimestamp` (ISO 8601), 5 min |
| Unsigned headers ignored for trust | `Linear-Timestamp` | `Plain-Webhook-Delivery-Attempt-Timestamp` |
| Dedup | `Linear-Delivery` header, plus the replay guard | body `id` |
| Event kind | body `type` + `.` + `action` (`Issue.create`) | body `type` (`thread.thread_created`) |
| Provider retries | 3 (about 1 min, 1 h, 6 h) | 12 over about 5 days |
| Expects | `200` within 5 s | `2xx`; redirects are failures |

Plain's documentation computes its example signature over a re-serialized body; Switchboard verifies
the raw bytes, which is what Plain signs. A contract test with a recorded Plain delivery guards this.

## Risks / Trade-offs

- **Slack events lost while awaiting a secret** (Slack retries for minutes, not days). → Docs and the
  create result say: set Slack's secret at creation. The handshake also fails without it, so Slack will
  not send events to an unverified URL anyway.
- **Vendor docs change.** → Each verifier cites its vendor page in a comment and has a recorded-fixture
  test; a scheme change fails that test rather than silently accepting.
- **Plain's workspace-wide secret** means one leaked secret affects every Plain webhook of that
  workspace. → Inherent in Plain; rotation with overlap makes rolling it cheap.
- **Existing broken Stripe and Slack webhooks change state** from "401 on every delivery" to
  "awaiting secret". → Called out in the release notes; it is strictly more honest.

## Migration Plan

1. Ship the docs interim now: mark `stripe` and `slack` as "awaiting provider-issued secret support" in
   `docs/getting-started/04-first-webhook.md` so no one wires them against `main` today.
2. Land secret origin, `set_webhook_secret`, the UI field, the Stripe and Slack delivery ids, the replay
   guard and the handshake (P1).
3. Land `linear` and `plain` (P2).

Rollback: the new columns are nullable or defaulted; removing the verb and the UI field returns to
minted-only behaviour, with provider-origin webhooks refusing deliveries as they effectively do today.

## Open Questions

- Should `slack` accept either a provider-issued secret or, for a Slack app the owner builds against a
  test harness, a minted one? Proposed: no; Slack always issues.
- Should the replay guard also apply to `cairn`, which has a signed timestamp and a signed event id?
  Proposed: not needed; its dedup key is already signed.
