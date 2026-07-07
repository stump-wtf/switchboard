# Design: Webhook Ingestion (Push Adapters)

## Context

Switchboard ingests external deliveries and turns them into durable todos that human-owned agents
drain. This design realizes the **push** half of that pipeline — HTTP webhook receivers — for
SPEC-0001. It is governed by two ADRs:

- [ADR-0003](../../../adrs/ADR-0003-per-provider-ingestion-and-trust-model.md) fixes the trust model:
  the `webhook` provider family and its ordered trust modes `signed` (HMAC verified, `verified=true`),
  `token` (shared-secret, caller authenticated, `verified=false`), and `open` (no check, off by
  default).
- [ADR-0014](../../../adrs/ADR-0014-ingestion-adapters-push-pull.md) frames ingestion as **adapters**
  in two families (push/pull) sharing one back-half contract into the todo queue. Webhooks are the
  push family; queues (SPEC-0002) are pull.

Current state: the signed GitHub receiver is implemented in `internal/ingest/ingest.go` and wired at
`POST /webhooks/github` in `internal/server/server.go`. Events persist to the `events` table and
todos to the `todos` table (`internal/db/migrations/0001_init.sql`), through
`store.InsertEvent` and `store.CreateTodo` (`internal/store/todos.go`). Stripe, Slack, and the
generic (`token`/`open`) endpoint follow the same contract but are not yet all coded; the prose
contract for the full push family lives in `docs/specs/ingestion-adapters.md` and
`docs/specs/openapi.yaml`, whose substance this design folds in.

Constraints: verification runs on the **raw body before parsing** (re-serialization breaks the HMAC);
comparisons are constant-time; secrets are never logged or persisted; and every accepted delivery
flows through the identical `verify → derive idempotency key → normalize → create todo (dedup) →
persist event` back half so it cannot drift from the pull family.

## Goals / Non-Goals

### Goals

- Receive provider webhooks over HTTP and verify each per its declared trust mode.
- Fail closed for `signed`: a bad/missing signature is 401 and nothing is persisted.
- Give unsigned webhooks a real, honest auth story (`token`) that is default-required and clearly
  weaker than `signed`.
- Dedup redeliveries into exactly one todo via an idempotency key.
- Normalize every accepted delivery to the same todo shape the pull family produces.
- Redact all secret-bearing headers before persisting event history.

### Non-Goals

- Pull (queue) ingestion — that is SPEC-0002 (`queue-adapters`).
- The todo lifecycle after creation (claim / lease / complete / fail) — that is the todos capability.
- Agent self-management of webhooks under a ceiling — ADR-0012 / the agent-mcp-tools capability.
- Routing-rule configuration UI — the routing-rule schema is shared and referenced, not owned here.

## Decisions

### Verify the raw body before parsing

**Choice**: Read the body with a bounded limiting reader, then verify the provider signature or token
over those exact bytes *before* JSON parsing.
**Rationale**: HMAC is computed over the raw bytes the sender signed; any parse-and-re-serialize
changes whitespace/ordering and breaks the signature. Verifying first also means a forged/oversized
delivery never reaches the parser or the store.
**Alternatives considered**:
- Parse then verify: rejected — re-serialization invalidates the HMAC and wastes work on forged input.

### Fail closed on signed rejection (401, no persist)

**Choice**: A missing/malformed/failing signature returns 401 and writes no event and no todo; only a
redacted line is logged.
**Rationale**: Persisting flagged-unverified rows would let an attacker fill the audit log with
unauthenticated payloads and muddy the trust surface. Failing closed keeps forged deliveries out of
the store entirely.
**Alternatives considered**:
- Persist with `verified=false`: rejected by ADR-0003 — pollutes history, invites log-flooding.

### `token` as a distinct, weaker tier for unsigned providers

**Choice**: Unsigned providers (Docker Hub, homelab) use a configured shared-secret token, header
preferred with a `?token=`/path fallback, default-required, provider disabled until set; persisted as
`trust_mode='token'`, `verified=false`.
**Rationale**: It is the honest answer to "can they set a password?" — the token authenticates the
caller but not the body and has no replay resistance, so it must never render as `signed`. Making it
default-required keeps homelab endpoints from being silently world-writable.
**Alternatives considered**:
- Open by default: rejected — every unsigned endpoint would be world-writable.
- Fabricate an HMAC scheme the provider lacks: rejected — a lie encoded in the trust UI.

### Idempotency key from provider delivery id, body-hash fallback

**Choice**: Derive the key from a provider delivery id where present (GitHub `X-GitHub-Delivery`,
Stripe event `id`) and fall back to `sha256(body)` where absent (Slack, generic). Dedup todos on
`(queue, idempotency_key)` among non-terminal rows; dedup events on `(source, external_id)`.
**Rationale**: Provider ids are the strongest dedup signal; a body hash is the best available where no
id exists. This collapses at-least-once sender retries into one todo.
**Alternatives considered**:
- Always body-hash: rejected — misses semantically-distinct redeliveries a provider id would separate.

## Architecture

A webhook receiver runs the **front half** (transport verification) unique to its trust mode, then
the **shared back half** identical to every adapter. The front half diverges by trust mode; the back
half — `InsertEvent` then `CreateTodo` (dedup) then hub publish — is common.

```mermaid
sequenceDiagram
    participant P as Provider (GitHub/Stripe/Slack/Docker Hub)
    participant R as Receiver (internal/ingest)
    participant S as Store (PostgreSQL)
    participant H as Hub (SSE / agent sessions)

    P->>R: POST /webhooks/... (raw body + signature|token)
    R->>R: bounded read (<= 5 MiB), else 413
    alt signed provider
        R->>R: HMAC over raw body, constant-time compare (+ timestamp window)
        R-->>P: 401 & no persist (on failure)
    else token (generic) provider
        R->>R: constant-time compare shared secret
        R-->>P: 403 & no persist (missing/wrong token, or disabled)
    else open provider (explicit opt-in)
        R->>R: no check
    end
    R->>R: sanitize headers (redact signature/token/secret)
    R->>R: derive idempotency key (delivery id or sha256(body))
    R->>S: InsertEvent (dedup on source+external_id)
    R->>S: CreateTodo (dedup on queue+idempotency_key, non-terminal)
    S-->>R: todo (created? bool)
    opt newly created
        R->>H: Publish(todo)
    end
    R-->>P: 202 {id, queue, verified}
```

Trust-mode outcomes map directly to the `events` columns (`family`, `trust_mode`, `verified`,
`verify_detail`) and drive labeling everywhere:

| Trust mode | Verified by | `verified` | `verify_detail` | Failure |
|------------|-------------|-----------|-----------------|---------|
| `signed` | per-provider HMAC over raw body (+ ts window for Stripe/Slack) | `true` | `hmac-sha256 ok` | 401, no persist |
| `token` | shared-secret constant-time compare | `false` | `token ok (caller authenticated; body not verified)` | 403, no persist |
| `open` | none (explicit opt-in) | `false` | `open — no verification` | n/a (off by default) |

## Risks / Trade-offs

- **`open` mode is a genuine foot-gun off a trusted network** → default-off, explicit operator opt-in,
  loudest label, documented localhost/Caddy posture.
- **`token` has no replay protection (same secret every request)** → labeled distinctly from `signed`,
  `verify_detail` states body-not-verified; prefer signed providers where available.
- **Body-hash idempotency keys collide across semantically-distinct identical bodies** → accepted for
  providers with no delivery id; the `(source, external_id)` event dedup and per-queue scoping bound
  the blast radius.
- **Operators must understand four trust values, not "webhooks are secure"** → prominent UI labeling
  and ADR-0003 on the record.

## Migration Plan

The signed GitHub receiver is already implemented and merged; this spec is a redo capturing it plus
the Stripe/Slack/generic contract. Remaining providers (Stripe/Slack signed adapters, the generic
`token`/`open` endpoint) are additive on the existing `events`/`todos`/`adapters` schema — no schema
migration is required beyond what `0001_init.sql` already provides (the `adapters` table already
carries `family`/`trust_mode`/`enabled`/`config`). New receivers reuse `InsertEvent` and `CreateTodo`
unchanged.

## Open Questions

- The MVP `internal/ingest/ingest.go` uses `io.LimitReader` (silently truncates at 5 MiB) rather than
  `http.MaxBytesReader` (returns an explicit 413). SPEC-0001 requires the 413 semantics; the code
  SHOULD move to `http.MaxBytesReader` for a clean oversize rejection.
- Per-provider signing secrets and generic tokens are read from environment/config today. Whether
  operator-managed provider config (including rotation) moves into the `adapters` table `config`
  column is deferred.
- Slack `url_verification` handshake (echo the `challenge`, 200, persist nothing) is specified in
  `openapi.yaml` but not yet implemented in the reference receiver.
