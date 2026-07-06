---
status: proposed
date: 2026-07-05
decision-makers: Joe Stump
related: [ADR-0001, ADR-0002, ADR-0003, ADR-0005, ADR-0010, ADR-0013, ADR-0014]
---

# ADR-0015: Implementation Language & Runtime — Go

## Context and Problem Statement

The implementation language was, until now, only *implicit* — the web-stack ADR ([ADR-0001](ADR-0001-web-stack-go-htmx-pico.md)) originally assumed Python/Starlette from the earliest single-node cut of switchboard. Now that switchboard is a **self-hosted, multi-tenant service**, the language should be an explicit decision and should match the rest of the fleet. **Every other self-hosted StumpCloud service is Go + HTMX + SSE**, and switchboard is not special: MCP, A2A, and Claude Code Channels are **ordinary wire protocols over HTTP and stdio** — MCP is JSON-RPC (Streamable HTTP or stdio), A2A is HTTP + JSON with SSE streaming, and a Channel is just MCP notifications. A Go/HTMX/SSE service serves all of that natively. Since **no application code exists yet** (docs-first), this is simply the moment to write the house choice down — a language *selection*, not a migration.

## Decision Drivers

* **Fleet consistency.** The self-hosted StumpCloud services are Go + HTMX + SSE. One stack means shared ops, deploy patterns, CI, and idioms. Deviating for one service needs a *reason*, and there isn't one.
* **The agent protocols are just HTTP/stdio.** MCP (JSON-RPC over Streamable HTTP or stdio), A2A (HTTP + JSON + SSE), and Channels (MCP notifications) are wire formats, not a runtime requirement. Go's stdlib `net/http` + JSON + SSE serve them directly; the official Go MCP SDK and A2A Go support exist, but the surface is buildable from the wire spec regardless.
* **Concurrency fit reinforces it.** The hot path is many long-lived workers draining PostgreSQL queues via `FOR UPDATE SKIP LOCKED` ([ADR-0002](ADR-0002-postgres-persistence-and-retention.md)), lease/retention timers, pull-adapter consumers ([ADR-0014](ADR-0014-ingestion-adapters-push-pull.md)), and streaming connections (SSE, Channels). Goroutines map onto that directly — no async coloring, no GIL.
* **Operational simplicity.** A single statically-linked binary with embedded assets is a tiny container, low memory, fast start, trivially run as several instances against one PostgreSQL — the same ops story as the rest of the fleet.
* **Type safety across many moving parts.** Scopes, verb allowlists, the todo state machine, trust modes, and adapter families are invariants a compiler should enforce.
* **No sunk cost.** With zero code written, "we already have Python" is not a reason; the switching cost is docs, which this ADR pays down.

## Considered Options

* **Go** — the house stack for self-hosted StumpCloud services (Go + HTMX + SSE); statically typed, goroutine concurrency, single-binary deploys.
* **Python** — Starlette/`asyncpg`/the `mcp` Python SDK; a mature MCP ecosystem and the language of the existing Channels prototype, but inconsistent with the fleet.
* **TypeScript / Node** — where the reference channels ship (TS/Bun); also inconsistent with the fleet and buys nothing the wire protocols don't already give Go.

## Decision Outcome

Chosen option: **Go.** It is the established stack for self-hosted StumpCloud services, and nothing about MCP/A2A/Channels — all HTTP/stdio wire protocols — argues for treating switchboard differently. Concurrency, single-binary ops, and static typing all reinforce the choice.

### What this pins (details in the cited ADRs)

| Concern | Go choice | ADR |
|---------|-----------|-----|
| HTTP + web UI | `net/http` + a light router (chi), `html/template`, SSE via `http.Flusher`, assets via `embed.FS` | [ADR-0001](ADR-0001-web-stack-go-htmx-pico.md) |
| PostgreSQL | `pgx` (no heavy ORM); `SKIP LOCKED` claims, `ON CONFLICT` dedup | [ADR-0002](ADR-0002-postgres-persistence-and-retention.md) |
| Signature verification | `crypto/hmac` + `crypto/subtle` (`hmac.Equal` / constant-time compare) | [ADR-0003](ADR-0003-per-provider-ingestion-and-trust-model.md) |
| Pull-adapter queue client | a Go Redis client (`redis/go-redis`), streams + consumer groups | [ADR-0014](ADR-0014-ingestion-adapters-push-pull.md) |
| MCP server + tools/resources | the official **Go MCP SDK** (`github.com/modelcontextprotocol/go-sdk`), or the wire protocol directly | [ADR-0005](ADR-0005-mcp-tool-and-resource-contract.md) |
| A2A discovery / agent cards | `net/http` + JSON (+ SSE for streaming) | [ADR-0010](ADR-0010-a2a-discovery-human-vended-friending.md) |
| Secrets | env/config-injected; switchboard-minted credentials hashed in Postgres | [ADR-0002](ADR-0002-postgres-persistence-and-retention.md) |
| Build & CI | `go build` / `go test` / `go vet` + `golangci-lint`; a single static binary | — |

A pinned modern Go toolchain (Go 1.23+); the exact minor version and dependency set are the code session's to lock in `go.mod`.

### Channels is HTTP/stdio too — a transport detail, not a language one

A Channel is an MCP server ([ADR-0013](ADR-0013-channels-push-delivery.md)); Go serves it like any other MCP surface. The only open detail is **transport**, and it is language-independent:

* If Claude Code Channels can use the **Streamable HTTP MCP transport**, switchboard serves channels **directly over HTTP** alongside its vended MCP endpoints and the web UI's SSE — no separate process, maximally consistent with the rest of the fleet.
* If Channels remains a **local stdio subprocess** of the harness, a small **Go** stdio binary bridges to central switchboard with a vended credential ([channel-delivery spec](../openspec/specs/channels/spec.md)) — still Go, still the same binary/toolchain.

Either way it is Go, and the durable todo queue remains the ledger. The Python prototype under `~/src/switchboard` is a **behavioral reference** for what a channel emits, not a codebase to carry forward.

### Consequences

* Good, because switchboard is one more Go + HTMX + SSE service — shared ops, deploys, CI, and idioms across the fleet, no special-casing.
* Good, because goroutines fit the worker/queue/timer/stream workload directly — no event-loop gymnastics, no GIL.
* Good, because a single static binary with embedded assets is the simplest thing to deploy and scale horizontally.
* Good, because compile-time types catch whole classes of scope/state-machine bugs before runtime.
* Good, because the Postgres queue, HMAC verification, and the MCP/A2A/Channels surfaces are all ordinary Go over `net/http`/stdio + JSON, with official SDKs available where useful.
* Neutral, because contributors fluent only in Python must switch — expected, since this is the house stack.

### Confirmation

* The repo is a Go module (`go.mod`), not a Python package; there is no `pyproject.toml`/`requirements` for the service.
* CI runs the Go toolchain (`build`/`test`/`vet` + `golangci-lint`) and produces a static binary.
* The MCP surface is served over Go `net/http`/stdio (via the Go MCP SDK or directly); a smoke test exercises a tool call and the recent-events resource ([ADR-0005](ADR-0005-mcp-tool-and-resource-contract.md)).
* A concurrency test exercises N goroutine workers claiming one Postgres queue with `SKIP LOCKED` and no double-claim ([ADR-0002](ADR-0002-postgres-persistence-and-retention.md)).

## Pros and Cons of the Options

### Go (chosen)

* Good, because it *is* the centrally-hosted house stack (Go + HTMX + SSE) — consistency, not novelty.
* Good, because goroutine concurrency, single-binary ops, and compile-time types fit a concurrent central queue service, and the agent protocols are plain HTTP/stdio wire formats Go serves natively (official Go MCP + A2A SDKs available).
* Bad, because if a bleeding-edge Channels feature ships first in the TS/Python SDK, Go may trail briefly — bounded, and irrelevant to the durable-queue ledger.

### Python (rejected)

* Good, because a mature MCP ecosystem and the language of the existing Channels prototype.
* Bad, because it is inconsistent with the Go fleet with no offsetting advantage (the protocols are just HTTP/stdio), and the GIL/async-coloring are a poorer fit for a worker/queue-heavy service. With no code yet, familiarity is the only pull, and it does not outweigh fleet consistency.

### TypeScript / Node (rejected)

* Good, because the reference channels ship there.
* Bad, because it is equally inconsistent with the fleet, single-threaded for the worker fan-out, and buys nothing over Go for surfaces that are all HTTP/stdio anyway.

## More Information

* Web/UI stack this decides the language for: [ADR-0001](ADR-0001-web-stack-go-htmx-pico.md).
* Persistence & queue mechanics that are idiomatic in Go: [ADR-0002](ADR-0002-postgres-persistence-and-retention.md).
* MCP surface (Go SDK or the wire protocol directly): [ADR-0005](ADR-0005-mcp-tool-and-resource-contract.md). Go MCP SDK: <https://github.com/modelcontextprotocol/go-sdk>.
* Signature verification primitives: [ADR-0003](ADR-0003-per-provider-ingestion-and-trust-model.md).
* Channels transport (HTTP vs. stdio) — language-independent: [ADR-0013](ADR-0013-channels-push-delivery.md), [channel-delivery spec](../openspec/specs/channels/spec.md).
