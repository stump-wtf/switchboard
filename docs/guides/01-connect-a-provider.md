---
title: Connect a provider
---

# Connect a provider

> **Using the hosted service?** Providers are instance-wide and only the instance's operators can
> connect them, so this page is operator reference. As a user, your agent creates its own webhooks
> for GitHub, Gitea, Cairn, and your own scripts — see
> [Receive your first webhook](/getting-started/first-webhook).

A **provider** is an inbound line into switchboard. Every event enters through one, and every
provider carries an **enforced trust mode** so an event's provenance is never in question. There are
two families.

## Webhook providers (push)

The sender POSTs to switchboard. The receiver reads the **raw body and verifies the caller before
any parsing**, then stamps the event with one of three ordered trust modes.

### `signed` — cryptographic verification (the strongest tier)

For providers that sign their payloads. Verification is **mandatory**: a missing, malformed, or
failing signature returns **HTTP 401 and the payload is not persisted** — only a redacted rejection
line is logged. A verified request persists with `verified=true`.

| Provider | Header | Scheme | Replay window |
|----------|--------|--------|---------------|
| GitHub | `X-Hub-Signature-256` | `sha256=` + HMAC-SHA256 over the raw body | n/a |
| Gitea | `X-Hub-Signature-256` | `sha256=` + HMAC-SHA256 over the raw body | n/a |
| Stripe | `Stripe-Signature` | timestamp + HMAC-SHA256 over `"{t}.{body}"` | reject if more than ~300s off |
| Slack | `X-Slack-Signature` | HMAC-SHA256 over `"v0:{ts}:{body}"` | reject if more than ~300s off |

Webhooks an agent creates for itself (`create_webhook`) accept the same schemes, and a `gitea` one
also accepts Gitea's bare-hex `X-Gitea-Signature`. Those, and the `cairn` source, are covered in
[Receive your first webhook](/getting-started/first-webhook).

**To connect one:** enable the provider in the Providers view and set its signing secret (injected
via environment/config, never committed). Point the sender at `POST /webhooks/{provider}` — see the
[API reference](/api).

### `token` — shared-secret authentication (for unsigned webhooks)

For webhook sources with **no signing scheme** — Docker Hub, homelab and self-hosted tooling. The
caller presents a **shared secret** on every request, preferably as `Authorization: Bearer <token>`,
or — for senders that can only be configured with a URL (Docker Hub is exactly this) — as a URL
token: `POST /webhooks/generic/{name}?token=…`.

A token authenticates the **caller** but does **not** verify the body: it can't detect a tampered
payload and offers no replay protection. It is an honest, weaker tier than `signed`, and labeled as
such. An unsigned provider **requires a token by default and stays disabled until one is set**, so a
homelab endpoint is never silently world-writable. Rotating the token is a config change.

### `open` — no verification (explicit, discouraged)

For senders that genuinely cannot present any secret. It is **off by default**, must be turned on
explicitly, is the loudest-labeled tier, and is appropriate **only** on an already-isolated network.
Prefer `token`; reach for `open` only when a token is impossible.

## Queue providers (pull)

A queue provider **consumes** from a broker and feeds messages into the same pipeline. There is no
HTTP request and no signature, so the trust boundary is **who is allowed to publish** to the queue —
enforced by the broker's ACL, not by switchboard.

| Queue type | Status | Trust boundary |
|------------|--------|----------------|
| **Redis** (streams / lists / pub-sub) | reference | connection auth (`requirepass` / ACL user) + TLS |
| SQS / NATS / AMQP | later | the broker's IAM/auth + TLS |

Events carry `trust_mode=queue` and a `verify_detail` naming the broker identity (e.g.
`redis acl: deploy-bot`). Crucially, a pull adapter **acks the source only after the todo is durably
stored** — so a crash between receive and store loses nothing; the message is redelivered.

**To connect one:** provide the connection URL/DSN (via environment/config) and enable the provider.

## What every provider shares

- **Secrets are redacted before persist.** Signature and token values are never written to the event
  record or logs.
- **Disabled providers reject fast** — a toggled-off provider returns 404/403 without processing.
- **One pipeline.** However an event arrives, once accepted it normalizes to a common shape,
  persists, broadcasts over SSE, and becomes eligible to fan out into todos.

## Next

Events are now flowing and landing as todos. Give an agent a scoped way to drain them:
[Vend an endpoint](/guides/vend-an-endpoint).

> Deeper detail: [ADR-0003 — Ingestion & trust model](/decisions/ADR-0003-per-provider-ingestion-and-trust-model)
> and [ADR-0014 — Ingestion adapters](/decisions/ADR-0014-ingestion-adapters-push-pull).
