# Design: Providers View, Runtime Registry, and Connect Wizard

> **Retired 2026-09-21 (#181).** The provider registry was removed; see the note on
> [SPEC-0017](spec.md). Kept as history.

## Context

Providers today are boot-time plumbing: signed adapters compiled in and configured by env, generic
providers parsed from env (`config.ParseGenericProviders`), pull adapters half-modeled in the
`adapters` table (family column, health fields) with no UI. The only surfaces are the MCP
`list_providers` tool and `providerStatuses()` in `internal/server/server.go`. The redesign adds
the sixth view + wizard + catalog. Governing: ADR-0020. Related: ADR-0003 (trust doctrine),
ADR-0014 (push/pull families), SPEC-0015 (view chrome + wizard pattern).

## Goals / Non-Goals

### Goals

- One registry both families resolve from at runtime; env becomes a seed, not the authority.
- The trust decision (ADR-0003) becomes an explicit, well-defaulted step an operator takes in the
  UI, with `token` as the pit of success for unsigned senders.
- Catalog honesty: available ≠ connectable.

### Non-Goals

- No new verification schemes and no changes to SPEC-0001 semantics (rejected payloads still never
  persist).
- No SQS/NATS/AMQP implementations in this spec — catalog cards only (each future adapter is its
  own spec/ADR moment under ADR-0014's contract).
- No multi-tenant provider ownership; providers are workspace-global like today.

## Decisions

### Converge on the adapters table, extended — not a third model

**Choice**: Grow the existing `adapters` registry (family already present) into the provider
registry — add kind, trust mode, enabled, encrypted secret, config JSON; webhook providers join
pull adapters in it.
**Rationale**: ADR-0014 already put pull adapters there with health fields; two registries would
force the view to merge them forever. Migration freedom is explicit (nobody uses this yet).

### Resolve-at-request, cache-lightly

**Choice**: Webhook routes (`/webhooks/*`, `generic/<name>`) and the runner look providers up from
the registry per request/poll (with a short in-process cache invalidated on registry writes) rather
than materializing routes at boot.
**Rationale**: This is what makes the wizard real — no restart. The generic mux already dispatches
by path segment; it starts consulting the registry instead of a boot-time map.

### Secrets through the envelope, revealed once

**Choice**: Wizard-generated tokens/secrets use the existing `internal/cred` envelope encryption
and the established one-time reveal pattern; the view only ever shows `configured`/`missing`.
**Rationale**: Matches SPEC-0007's credential discipline; the web tier gains no new plaintext
secret surface.

## Architecture

```mermaid
flowchart TB
    ENV[env/deploy config] -->|idempotent seed at boot| REG[(provider registry\nextended adapters table)]
    WIZ[connect wizard\nSPEC-0015 pattern] -->|create/rotate/disable| REG
    REG --> WH["/webhooks/{provider}\ningest verification (SPEC-0001)"]
    REG --> RUN[pull-adapter runner\n(SPEC-0002)]
    REG --> VIEW[Providers view\nfamilies · trust chips · health]
    REG --> MCP[list_providers tool\nproviderStatuses]
    WH --> EV[(events → todos)]
    RUN --> EV
```

## Key files

Touched: `internal/store/adapters.go` (registry CRUD), one migration (extend `adapters`),
`internal/ingest` (registry-resolving dispatch for generic/token/open; signed adapters read their
secret from registry-or-env), `internal/adapter/runner` (registry-driven), `internal/config`
(seed import), `internal/server/server.go` (routes, `providerStatuses` reads registry), new
`internal/web/providers.go` + templates. The MCP `list_providers` tool output shape is unchanged.
