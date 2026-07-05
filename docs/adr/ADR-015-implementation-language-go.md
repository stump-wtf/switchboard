---
status: proposed
date: 2026-07-05
decision-makers: Joe Stump
related: [ADR-001, ADR-002, ADR-003, ADR-005, ADR-006, ADR-014]
---

# ADR-015: Implementation Language & Runtime — Go

## Context and Problem Statement

The implementation language was, until now, only *implicit* — the web-stack ADR ([ADR-001](ADR-001-web-stack-go-htmx-pico.md)) originally assumed Python/Starlette because the first cut of switchboard was a single-node homelab **event store**. What the design has since become is a different animal: a **centrally hosted, multi-tenant service** whose hot path is a **concurrent todo work-queue** — many agent-worker loops draining PostgreSQL via `FOR UPDATE SKIP LOCKED` ([ADR-002](ADR-002-postgres-persistence-and-retention.md)), a lease reaper, pull-adapter queue consumers ([ADR-014](ADR-014-ingestion-adapters-push-pull.md)), webhook ingestion, SSE + Channels push ([ADR-013](ADR-013-channels-push-delivery.md)), and a fleet of vended MCP endpoints ([ADR-008](ADR-008-human-principal-vended-endpoints.md)). That is a concurrency- and operations-heavy profile, and it deserves an explicit language decision. Because **no application code exists yet** (docs-first), this is the cheapest possible moment to choose — a language *selection*, not a migration.

## Decision Drivers

* **Concurrency fit.** The core workload is many long-lived workers draining queues, timers (leases/retention), fan-out consumers, and streaming connections. A goroutine-per-worker model maps directly onto that with no async/await coloring and no GIL serializing CPU-bound bits (signature verification, JSON handling) under load.
* **Operational simplicity for a hosted service.** A single statically-linked binary with embedded assets deploys as one container with a tiny image, low memory, and fast start — easy to run several instances against one PostgreSQL. This matches the StumpCloud central-service posture better than a Python app + ASGI worker manager + interpreter/venv.
* **Type safety across many moving parts.** Scopes, verb allowlists, the todo state machine, trust modes, and adapter families are exactly the kind of invariants a compiler should enforce.
* **Protocol SDKs now exist in Go.** The historical reason to pick Python here — MCP/A2A maturity — has largely closed: the **official Go MCP SDK** and A2A Go support both landed in 2025, so the whole agent-protocol surface is buildable in Go.
* **Idiomatic Postgres queue.** `pgx` + `FOR UPDATE SKIP LOCKED` is a well-trodden Go pattern; the queue mechanics in [ADR-002](ADR-002-postgres-persistence-and-retention.md) are native, not bolted on.
* **No sunk cost.** With zero code written, "we already have Python" is not a reason — the switching cost is docs, which this ADR pays down.

## Considered Options

* **Go** — statically-typed, goroutine concurrency, single-binary deploys; official Go MCP SDK + A2A Go.
* **Python** — Starlette/`asyncpg`/the reference `mcp` Python SDK; the richest, fastest-moving MCP/A2A ecosystem and Joe's house language (the existing Channels prototype is Python).
* **TypeScript / Node** — the most MCP/Channels-native runtime (the reference channels are TS/Bun), strong for the agent-protocol surface.

## Decision Outcome

Chosen option: **Go.** The service is now a long-lived, concurrent, multi-worker queue service, which is Go's strongest domain; the SDK gap that once favored Python has closed; and with no code written the choice is free.

### What this pins (details in the cited ADRs)

| Concern | Go choice | ADR |
|---------|-----------|-----|
| HTTP + web UI | `net/http` + a light router (chi), `html/template`, SSE via `http.Flusher`, assets via `embed.FS` | [ADR-001](ADR-001-web-stack-go-htmx-pico.md) |
| PostgreSQL | `pgx` (no heavy ORM); `SKIP LOCKED` claims, `ON CONFLICT` dedup | [ADR-002](ADR-002-postgres-persistence-and-retention.md) |
| Signature verification | `crypto/hmac` + `crypto/subtle` (`hmac.Equal` / constant-time compare) | [ADR-003](ADR-003-per-provider-ingestion-and-trust-model.md) |
| Pull-adapter queue client | a Go Redis client (`redis/go-redis`), streams + consumer groups | [ADR-014](ADR-014-ingestion-adapters-push-pull.md) |
| MCP server + tools/resources | the official **Go MCP SDK** (`github.com/modelcontextprotocol/go-sdk`) | [ADR-005](ADR-005-mcp-tool-and-resource-contract.md) |
| Secrets (OpenBao AppRole) | the Vault/OpenBao Go HTTP API client | [ADR-004](ADR-004-secrets-management-openbao-approle.md) |
| Build & CI | `go build` / `go test` / `go vet` + `golangci-lint`; a single static binary | [ADR-006](ADR-006-gitea-primary-github-mirror-and-ci.md) |

A pinned modern Go toolchain (Go 1.23+); the exact minor version and dependency set are the code session's to lock in `go.mod`.

### The Channels nuance

A Claude Code **channel** is an MCP server the harness spawns over stdio ([ADR-013](ADR-013-channels-push-delivery.md)); the reference channels are TS/Bun and the switchboard prototype is Python. In Go this is the one component slightly off the busiest path: it is served by the Go MCP SDK (which supports the stdio transport and server-initiated notifications), or, if a specific Channels feature lags in the Go SDK, kept as a **thin separate channel-adapter process** in whatever language has the best channel support, bridging to central switchboard with a vended credential ([channel-delivery spec](../specs/channel-delivery.md)). Either way the durable todo queue remains the ledger, so the adapter is replaceable.

### Consequences

* Good, because the concurrency model fits the worker/queue/timer/stream workload directly — no event-loop gymnastics, no GIL.
* Good, because a single static binary with embedded assets is the simplest thing to deploy and scale horizontally for a central service.
* Good, because compile-time types catch whole classes of scope/state-machine bugs before runtime.
* Good, because the Postgres queue, HMAC verification, and MCP surface are all idiomatic Go with first-party or official libraries.
* Bad, because Go is off the *busiest* MCP/A2A/Channels ecosystem path (Python/TS) — mitigated by the now-official Go SDKs and the replaceable channel-adapter boundary.
* Bad, because it discards the working Python Channels prototype as a starting point — accepted; it remains a reference for behavior, and there is no other code to carry over.
* Neutral, because contributors fluent in Python must switch — acceptable for a single-maintainer service where the runtime fit wins.

### Confirmation

* The repo is a Go module (`go.mod`), not a Python package; there is no `pyproject.toml`/`requirements` for the service.
* CI runs the Go toolchain (`build`/`test`/`vet` + `golangci-lint`) and produces a static binary ([ADR-006](ADR-006-gitea-primary-github-mirror-and-ci.md)).
* The MCP surface is served via the Go MCP SDK; a smoke test exercises a tool call and the recent-events resource ([ADR-005](ADR-005-mcp-tool-and-resource-contract.md)).
* A concurrency test exercises N goroutine workers claiming one Postgres queue with `SKIP LOCKED` and no double-claim ([ADR-002](ADR-002-postgres-persistence-and-retention.md)).

## Pros and Cons of the Options

### Go (chosen)

* Good, because goroutine concurrency, single-binary ops, and compile-time types fit a concurrent central queue service.
* Good, because official Go MCP + A2A SDKs and idiomatic `pgx`/`SKIP LOCKED`.
* Bad, because the Channels/MCP reference ecosystem is Python/TS — a manageable, bounded gap.

### Python (rejected)

* Good, because the richest MCP/A2A/Channels ecosystem, Joe's house language, and a working Channels prototype.
* Bad, because the GIL and async-coloring are a poorer fit for a worker/queue-heavy long-lived service, and at this scale the language — not Postgres — would be the thing you tune around; with no code yet, ecosystem familiarity is the only real pull, and it does not outweigh the runtime fit.

### TypeScript / Node (rejected)

* Good, because it is the most MCP/Channels-native runtime.
* Bad, because it is the largest departure from the Python-adjacent tooling around this project, single-threaded for the worker fan-out (worker threads/cluster to compensate), and buys little over Go for the non-Channels surface.

## More Information

* Web/UI stack this decides the language for: [ADR-001](ADR-001-web-stack-go-htmx-pico.md).
* Persistence & queue mechanics that are idiomatic in Go: [ADR-002](ADR-002-postgres-persistence-and-retention.md).
* MCP surface via the Go SDK: [ADR-005](ADR-005-mcp-tool-and-resource-contract.md). Go MCP SDK: <https://github.com/modelcontextprotocol/go-sdk>.
* Signature verification primitives: [ADR-003](ADR-003-per-provider-ingestion-and-trust-model.md). CI & build: [ADR-006](ADR-006-gitea-primary-github-mirror-and-ci.md).
* The Channels adapter boundary: [ADR-013](ADR-013-channels-push-delivery.md), [channel-delivery spec](../specs/channel-delivery.md).
