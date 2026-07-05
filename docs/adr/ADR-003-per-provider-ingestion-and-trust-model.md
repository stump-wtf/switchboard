---
status: proposed
date: 2026-07-05
decision-makers: Joe Stump
related: [ADR-000, ADR-002, ADR-004, ADR-005, ADR-014]
---

# ADR-003: Per-Provider Ingestion and Trust Model (Signed / Generic-Unverified / Redis)

## Context and Problem Statement

`switchboard` ingests events from heterogeneous sources with wildly different security stories: GitHub, Stripe, and Slack sign their webhooks (each with a *different* HMAC scheme); Docker Hub has *no* signing scheme at all; and Redis is not an HTTP webhook at all but a queue the app *consumes*. A single "verify if you can, otherwise trust it" approach would silently launder unverified payloads as if they were authenticated — the exact failure the brief calls out (§8, §13). The system must make each source's trust story **explicit, per-provider, and enforced**, and surface it honestly in the UI and API so a human never has to guess whether an event was actually authenticated. How do we model ingestion so that signed providers are verified-or-rejected, unsigned providers are clearly labeled unverified rather than faked, and the queue consumer's real trust boundary (the Redis connection) is documented rather than pretended away?

## Decision Drivers

* **Honesty over convenience.** The trust level of every event must be visible and accurate. Never present an unverified payload as signed. (Brief §8, §13.)
* **Enforcement, not suggestion.** For a provider *declared* signed, signature verification is mandatory: a failed or missing signature is a 401 and the payload is **not persisted** (only the rejection is logged). Verification is not a best-effort nicety.
* **Docker Hub reality.** Docker Hub ships no webhook signature scheme. Faking a "signed Docker Hub" adapter would be a lie in code. It must be handled as what it is: a generic, unverified sender, mitigated by network isolation + a per-instance shared token.
* **Redis is a different front door.** No HTTP request, no signature header exists to check. The trust boundary is *who can publish to the channel* — i.e. Redis auth/ACL/TLS. That assumption must be documented, not implicit.
* **One pipeline, many front doors.** However an event arrives, it flows through the same `normalize → persist → broadcast → expose` path. The trust model is metadata carried on the event, not a fork in the pipeline.
* **Replay resistance for the schemes that support it.** Stripe and Slack sign a timestamp; we enforce a freshness window to blunt replay. GitHub does not, so we do not pretend to.
* **No secret leakage.** Signature headers and secrets must never be logged in full or persisted (cross-ref [ADR-002](ADR-002-sqlite-persistence-and-retention.md), [ADR-004](ADR-004-secrets-management-openbao-approle.md)).

## Considered Options

* **Trust modeling:** (A) single best-effort `verified` boolean, verify when a secret is configured; (B) **three explicit declared trust modes** (`signed`, `unverified`, `redis`) with per-provider enforcement; (C) treat every source as generic/unverified and push trust entirely to the network layer.
* **Docker Hub:** (A) build a "Docker Hub signed" adapter that fabricates verification; (B) **route Docker Hub through the generic/unverified endpoint** with a per-instance shared-secret path/query token.
* **Redis trust:** (A) require an application-level HMAC envelope on every Redis message; (B) **rely on the Redis connection's auth/ACL (+ TLS where available)** as the trust boundary and document it.
* **Rejected-payload handling on signed providers:** (A) persist the payload flagged `verified=0`; (B) **reject with 401 and do not persist** (log the rejection only).

## Decision Outcome

Chosen options: **B (three explicit declared trust modes, enforced per provider)**, **B (Docker Hub via the generic/unverified endpoint with a shared token)**, **B (Redis trust = connection ACL/TLS, documented)**, and **B (signed-provider verification failures return 401 and are not persisted)**.

Every provider config declares exactly one `trust_mode`, and the mode is stored on every event ([ADR-002](ADR-002-sqlite-persistence-and-retention.md) `trust_mode` + `verified` + `verify_detail`) and shown everywhere in the UI and API:

### Mode 1 — `signed` (verification mandatory)

Signature verification is required, not optional. On a signed provider, a request whose signature is **missing, malformed, or fails** returns **HTTP 401** and the payload is **not written to the database**; only a redacted rejection line is logged (provider, event type if known, reason, source IP — never the secret or the full signature). A verified request is persisted with `verified=1` and `verify_detail` like `hmac-sha256 ok`. All HMAC comparisons use a constant-time compare (`hmac.compare_digest`). The raw request body is read and verified **before** any parsing (a JSON re-serialize would change the bytes and break the HMAC).

MVP signed providers and their schemes:

| Provider | Header(s) | Scheme | Replay window |
|----------|-----------|--------|---------------|
| GitHub | `X-Hub-Signature-256` | HMAC-SHA256 over raw body, `sha256=` prefix | n/a (no signed timestamp) |
| Stripe | `Stripe-Signature` | `t=` timestamp + `v1=` HMAC-SHA256 over `"{t}.{body}"` | reject if `|now − t|` > tolerance (default 300s) |
| Slack | `X-Slack-Signature`, `X-Slack-Request-Timestamp` | `v0=` HMAC-SHA256 over `"v0:{ts}:{body}"` | reject if `|now − ts|` > tolerance (default 300s) |

GitHub is implemented first to prove the signed path; a second signed provider (Stripe or Slack) proves the abstraction generalizes (brief §2, §11.3).

### Mode 2 — `generic` (unverified, by design)

A catch-all endpoint `/webhooks/generic/{name}` for senders with **no signature scheme at all** — self-hosted services, homelab-internal tooling, and **Docker Hub** (which has no native signing). It is:

* **Opt-in per instance and disabled/not-created by default.** An operator must explicitly add a generic provider; the system never auto-creates one.
* **Labeled unverified everywhere.** Events carry `trust_mode='unverified'`, `verified=0`, `verify_detail='unverified by design'`; the UI shows an explicit "unverified" badge, never a lock/"signed" indicator.
* **Guarded by a weak, non-cryptographic per-instance token** — a shared secret in the path (`/webhooks/generic/{name}?token=…` or a path token). This is a *bozo filter* against stray traffic, **not** authentication, and is documented as such. Token comparison is still constant-time.
* **Only appropriate on a trusted network.** This endpoint assumes the network is already isolated by Caddy `forward_auth` / UniFi segmentation. It is a real trust boundary, not a fallback to reach for casually.

Docker Hub is routed here rather than through a fake "signed" adapter, because inventing verification where the provider offers none would be dishonest and would undermine the meaning of the `signed` badge for every other provider.

### Mode 3 — `redis` (queue consumer, not HTTP receiver)

> **Later generalization ([ADR-014](ADR-014-ingestion-adapters-push-pull.md)):** this "redis mode" is reframed as the reference **pull adapter** of the ingestion-adapter model — the `signed`/`unverified` HTTP webhooks are the **push** family; Redis (and later SQS/NATS/AMQP) is the **pull** family. The `redis` *trust* semantics below (trust = the connection) are unchanged; ADR-014 adds the pull-side **store-then-ack** coupling and decides the pub/sub-vs-stream sub-decision below in favor of an ack-capable mode.

An in-process task subscribes to a Redis channel/stream and feeds messages into the same pipeline. There is **no HTTP request and no signature header**, so HTTP-style verification does not apply. The trust boundary is the **Redis connection itself**: authentication (`requirepass`/ACL user), ACLs constraining *who can publish* to the subscribed channel, and TLS if the Redis instance supports it. Events carry `trust_mode='redis'`, `verified=0`, and `verify_detail` naming the ACL user (e.g. `redis acl: deploy-bot`). The security control is "who is allowed to publish to this channel," and this assumption is documented here rather than left implicit. The Redis connection secret (URL/password) is pulled from OpenBao like any other ([ADR-004](ADR-004-secrets-management-openbao-approle.md)).

**pub/sub vs. consumer-group stream — deferred sub-decision.** Whether the Redis path uses plain pub/sub (fire-and-forget, no redelivery) or a consumer-group stream (`XREADGROUP`, acked, redelivery on restart) is left to the code session. Recommendation captured here: **prefer a consumer-group stream** so events survive an app restart and are not silently lost, at the cost of managing consumer-group offsets. If pub/sub is chosen for simplicity, the README must state that events published while the app is down are lost. Either way the client is `redis.asyncio` (`redis-py`).

### Cross-cutting rules (all modes)

* **Header sanitization before persist.** Before writing `headers` to the DB, signature/secret-bearing headers (`X-Hub-Signature-256`, `Stripe-Signature`, `X-Slack-Signature`, `Authorization`, any `?token=`) are redacted to a marker like `«redacted»`. Full signature values and secrets are never logged (brief §8).
* **Disabled providers reject fast.** A provider toggled off in the registry ([ADR-002](ADR-002-sqlite-persistence-and-retention.md) `providers.enabled`) returns 404/403 without processing.
* **Same downstream pipeline.** Regardless of front door, accepted events normalize to the common event shape, persist, broadcast over SSE, and become visible to the MCP tools ([ADR-005](ADR-005-mcp-tool-and-resource-contract.md)).

### Consequences

* Good, because the trust level of every event is explicit, stored, and displayed — no source is ever laundered into looking authenticated.
* Good, because signed providers fail closed (401, no persist), so a forged or misconfigured signature cannot inject a stored event.
* Good, because Docker Hub's real limitation is represented honestly as an unverified generic sender, keeping the `signed` badge meaningful.
* Good, because Redis's trust boundary is documented as connection/ACL, so operators know what actually protects the channel.
* Good, because one pipeline with trust-as-metadata keeps the code, schema, and specs uniform across four front doors.
* Bad, because operators must understand three trust modes rather than "webhooks are secure" — mitigated by prominent UI labeling and this ADR.
* Bad, because the generic endpoint is a genuine foot-gun if exposed off a trusted network — mitigated by default-disabled, explicit opt-in, unverified labeling, and the localhost/Caddy posture in the README ([ADR-001](ADR-001-web-stack-starlette-htmx-pico.md)).
* Bad, because we maintain three distinct HMAC schemes for the signed providers — irreducible; each provider defines its own, and faking a common one would be wrong.

### Confirmation

* Per-provider verification tests: for each signed provider, a valid signature persists with `verified=1`; an invalid/missing signature returns 401 and writes **no** event row (brief §9 positive + negative cases).
* Stripe/Slack tests assert a stale timestamp (outside tolerance) is rejected even with an otherwise-valid HMAC.
* A test asserts persisted `headers` have signature/secret headers redacted, and that logs contain no full signature value.
* A test asserts the generic endpoint is not present until explicitly configured and is labeled `unverified` in both the API and the rendered UI.
* A test asserts Redis-ingested events carry `trust_mode='redis'`, `verified=0`.
* HMAC comparisons use `hmac.compare_digest` (constant-time) — verified by code review and a lint/grep check for `==` on signature bytes.

## Pros and Cons of the Options

### Trust modeling: three explicit modes (chosen) vs. best-effort boolean vs. all-generic

**Three explicit declared modes (B, chosen)**
* Good, because each provider's trust story is declared and enforced, and rendered honestly.
* Good, because "signed" retains a precise, trustworthy meaning.
* Bad, because it is more model than a single flag — worth it for correctness.

**Best-effort `verified` boolean (A, rejected)**
* Good, because simplest to implement.
* Bad, because "verify if a secret happens to be configured" silently downgrades a signed provider to unverified on misconfiguration, and blurs signed vs. unsigned into one ambiguous flag — the exact laundering the brief forbids.

**All-generic / network-only trust (C, rejected)**
* Good, because uniform and simple.
* Bad, because it throws away real cryptographic verification that GitHub/Stripe/Slack *do* provide, weakening security for no benefit.

### Docker Hub: generic endpoint (chosen) vs. fake signed adapter (rejected)

* Good (generic), because it represents Docker Hub's actual capability and keeps the `signed` badge honest.
* Bad (fake signed), because it would fabricate verification that does not exist — a lie encoded in the trust UI. Explicitly rejected in the brief.

### Redis: connection ACL/TLS (chosen) vs. app-level HMAC envelope (rejected)

* Good (ACL/TLS), because it matches how Redis security actually works and needs no schema imposed on publishers.
* Bad (app HMAC), because it invents a message format every publisher must adopt, reimplements what Redis ACLs already provide, and adds a shared-secret distribution problem — overkill for a queue whose access is already gated by connection auth.

### Rejected payloads on signed providers: 401 + no persist (chosen) vs. persist flagged unverified (rejected)

* Good (401 + no persist), because it fails closed; forged payloads never enter the store.
* Bad (persist flagged), because it lets an attacker fill the event log with unauthenticated rows and muddies the audit trail; a signed provider that fails verification is, by definition, not a trustworthy event to keep.

## Architecture Diagram

```mermaid
flowchart TD
  subgraph front[Front doors]
    gh[/webhooks/github/]:::signed
    st[/webhooks/stripe/]:::signed
    sl[/webhooks/slack/]:::signed
    gen[/webhooks/generic/{name}<br/>Docker Hub + homelab/]:::unver
    rc[[Redis consumer task]]:::redis
  end

  gh --> V{signature valid?}
  st --> V
  sl --> V
  V -- no --> reject[[401 · log redacted · DO NOT persist]]:::bad
  V -- yes --> norm

  gen --> tok{shared token ok?<br/>bozo-filter, not auth}
  tok -- no --> reject
  tok -- yes --> norm

  rc -->|trust = Redis ACL/TLS| norm

  norm[normalize → set trust_mode/verified] --> db[(SQLite events)]
  db --> sse[SSE broadcast]
  db --> mcp[MCP tools]

  classDef signed fill:#dfe,stroke:#090
  classDef unver fill:#ffe8c2,stroke:#c80
  classDef redis fill:#dde4ff,stroke:#33f
  classDef bad fill:#fdd,stroke:#b00
```

## More Information

* Provider signature references: GitHub <https://docs.github.com/en/webhooks/using-webhooks/validating-webhook-deliveries>, Stripe <https://docs.stripe.com/webhooks#verify-events>, Slack <https://api.slack.com/authentication/verifying-requests-from-slack>, Docker Hub (no native signing) <https://docs.docker.com/docker-hub/webhooks/>.
* Redis trust references: `redis-py` asyncio <https://redis-py.readthedocs.io/en/stable/examples/asyncio_examples.html>, Redis ACL <https://redis.io/docs/latest/operate/oss_and_stack/management/security/acl/>.
* Secret sourcing for all provider secrets and the Redis URL: [ADR-004](ADR-004-secrets-management-openbao-approle.md).
* Storage of `trust_mode`/`verified`/`verify_detail` and header redaction: [ADR-002](ADR-002-sqlite-persistence-and-retention.md).
* This ADR governs the ingestion half of `docs/specs/openapi.yaml` (webhook endpoints, 401 responses) and is realized by the provider adapters (`app/providers/{github,stripe,slack,generic,redis}.py`) in the code session.
