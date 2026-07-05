# switchboard — Design Docs Index

Switchboard is **docs-first**: these architecture decision records (`docs/adr/`) and specs
(`docs/specs/`) are the canonical design record. Application code is written *fresh from these
documents*. The published site renders this tree via `docs-site/scripts/build-docs.mjs`.

Two layers:

- **Event-store core (ADR-000–006)** — receive, verify, persist, and expose inbound webhooks/queue
  events to MCP clients and a local web UI.
- **Agent layer (ADR-007–014)** — inbound events become durable **todos**; humans register agents and
  are vended scoped MCP endpoints; personas are A2A Agent Cards; cross-agent work is granted by
  human-approved friending; and todos are pushed into live harness sessions over Claude Code Channels,
  with the durable queue staying the ledger.

## Architecture Decision Records (MADR)

| ADR | Title | One-line |
|-----|-------|----------|
| [ADR-000](adr/ADR-000-project-naming-and-scope.md) | Project naming & scope | The project is `switchboard`; MVP scope ratified. |
| [ADR-001](adr/ADR-001-web-stack-starlette-htmx-pico.md) | Web stack | Starlette + HTMX + Pico.css over FastAPI/SPA/Tailwind. |
| [ADR-002](adr/ADR-002-postgres-persistence-and-retention.md) | PostgreSQL persistence & retention | Postgres queue store: SKIP LOCKED claims, ON CONFLICT dedup, partial pending index, age + row-cap pruning. |
| [ADR-003](adr/ADR-003-per-provider-ingestion-and-trust-model.md) | Per-provider trust model | Three explicit trust modes (`signed`/`unverified`/`redis`), enforced. |
| [ADR-004](adr/ADR-004-secrets-management-openbao-approle.md) | Secrets via OpenBao AppRole | Machine identity; `secret/switchboard/*`; nothing on disk. |
| [ADR-005](adr/ADR-005-mcp-tool-and-resource-contract.md) | MCP tool/resource contract | `list`/`get`/`replay`/`list_providers` + recent-events resource. |
| [ADR-006](adr/ADR-006-gitea-primary-github-mirror-and-ci.md) | Gitea primary + GitHub mirror + CI | Gitea is canonical; GitHub mirrors; CI on the act_runner. |
| [ADR-007](adr/ADR-007-todos-as-core-primitive.md) | **Todos as the core primitive** | Durable work-items (lease/ack/idempotency), not a message inbox. |
| [ADR-008](adr/ADR-008-human-principal-vended-endpoints.md) | **Human principal + vended endpoints** | Humans authenticate; agents get vended scoped MCP endpoints; IdP holds humans only. |
| [ADR-009](adr/ADR-009-personas-as-scoped-agent-cards.md) | **Personas as scoped Agent Cards** | One agent → many personas; a persona is a verb-subset, advertised as an A2A card. |
| [ADR-010](adr/ADR-010-a2a-discovery-human-vended-friending.md) | **A2A discovery + human-vended friending** | A2A discovers; MCP tools; todo-queue transports; approval-is-vend. |
| [ADR-011](adr/ADR-011-identity-assurance-oidc-passkey-deferred.md) | **Identity & assurance** | Simple OIDC (Pocket ID) now; passkey `amr`/`acr` step-up deferred. |
| [ADR-012](adr/ADR-012-agents-self-manage-webhooks.md) | **Agents self-manage webhooks** | Webhook CRUD within a human-vended ceiling; switchboard owns verification. |
| [ADR-013](adr/ADR-013-channels-push-delivery.md) | **Channels push-delivery** | Claude Code Channels pushes into a live session as a notify layer; the durable todo queue stays the ledger. |
| [ADR-014](adr/ADR-014-ingestion-adapters-push-pull.md) | **Ingestion adapters (push/pull)** | Push (webhook) + pull (queue) families → todos; Redis is the reference pull adapter; store-then-ack couples the source ack to the todo. |

## Specifications

| Spec | Covers |
|------|--------|
| [openapi.yaml](specs/openapi.yaml) | HTTP surface: webhook ingestion endpoints + web-UI routes. |
| [asyncapi.yaml](specs/asyncapi.yaml) | SSE stream for the live web UI. |
| [mcp-tools.md](specs/mcp-tools.md) | Event-history MCP contract (`list`/`get`/`replay`/`list_providers`). |
| [todos.md](specs/todos.md) | Todo object schema + state machine (ADR-007). |
| [agent-mcp-tools.md](specs/agent-mcp-tools.md) | Vended-endpoint MCP tools: todos, webhook CRUD, friending verbs (ADR-008/010/012). |
| [ingestion-adapters.md](specs/ingestion-adapters.md) | Push (webhook) + pull (queue) adapters, shared verification/idempotency/normalization → todos, pull store-then-ack coupling, routing rules (ADR-014/003/007). |
| [personas-and-agent-cards.md](specs/personas-and-agent-cards.md) | Persona record, verb→skill derivation, A2A Agent Card + well-known (ADR-009). |
| [friend-requests.md](specs/friend-requests.md) | Discover → request → approval-todo → approve(=vend) flow (ADR-010). |
| [accounts-and-endpoints.md](specs/accounts-and-endpoints.md) | Human OIDC account, agent registration, vend/scope/ceiling (ADR-008/011/012). |
| [channel-delivery.md](specs/channel-delivery.md) | Channels push: todo→notification mapping, lossy/degrade-to-pull semantics, two-way reply/permission relay (ADR-013). |

## Open questions / to confirm

Decisions recorded as *proposed* that Joe should confirm before the code session locks them in:

1. **Immutable vs. mutable vended scope** *(proposed: immutable)* —
   [ADR-008](adr/ADR-008-human-principal-vended-endpoints.md), [accounts-and-endpoints spec](specs/accounts-and-endpoints.md).
   A vended endpoint's scope is proposed **immutable**: change access by revoke + re-vend, not in-place
   edit. Simpler to audit (a URL+credential always means one fixed power set); the alternative (mutable
   scope) is more convenient but reintroduces "when/who changed this scope" ambiguity. **Confirm.**

2. **Passkey `amr`/`acr` step-up — deferred hardening (must not be crossed silently)** —
   [ADR-011](adr/ADR-011-identity-assurance-oidc-passkey-deferred.md), [friend-requests spec](specs/friend-requests.md).
   Today switchboard trusts the passkey-only Pocket ID issuer and does **not** enforce a
   phishing-resistant `amr`/`acr` claim on consent actions. **Before federating to / accepting
   provenance from any non-passkey IdP**, it MUST require such a claim on friend approvals. Note there
   is **no standard `amr` value for "passkey"** (RFC 8176: `hwk`/`swk`/`pop`/`mfa`; passkey ≈ `hwk`+`pop`,
   a `phr` marker, or an agreed `acr`) — the exact value/level is TBD at that time.

3. **Event/todo seam** —
   [ADR-007](adr/ADR-007-todos-as-core-primitive.md).
   The ADR-005 event tools (history/audit) and the todo tools (work) are currently two surfaces sharing
   one PostgreSQL layer, with events as todo producers. Whether they remain two surfaces indefinitely, or
   whether events collapse into a pure sub-record of todos, is left to confirm.

4. **Channels push: notification-only vs. inline payload** *(proposed: notification-only)* —
   [ADR-013](adr/ADR-013-channels-push-delivery.md), [channel-delivery spec](specs/channel-delivery.md).
   A Channels wake carries the todo id + summary and the agent claims via the queue; whether to also
   inline a small payload in the push is left to confirm.

5. **Channels two-way (reply tool + permission relay): MVP or defer** *(proposed: defer)* —
   [ADR-013](adr/ADR-013-channels-push-delivery.md).
   One-way notify first; the reply tool and human-consent permission relay (mapping onto todo
   completion and friend approvals) are recommended for a later phase. **Confirm.**

6. **Local channel-adapter ↔ central switchboard handshake** —
   [ADR-013](adr/ADR-013-channels-push-delivery.md), [channel-delivery spec](specs/channel-delivery.md).
   Channels needs a local stdio subprocess but switchboard is a central service; the thin local adapter
   authenticates with the vended credential and bridges pull + push. The exact adapter/auth shape is a
   code-session detail to confirm.

7. **Pull-adapter ack timing: ack-on-store vs. ack-on-complete** *(proposed: ack-on-store)* —
   [ADR-014](adr/ADR-014-ingestion-adapters-push-pull.md), [ingestion-adapters spec](specs/ingestion-adapters.md).
   A pull adapter acks the source message once the todo is **durably stored** (the durability boundary);
   optionally it could defer the ack until the todo is **completed** for stronger end-to-end coupling at
   the cost of holding redelivery state longer. **Confirm.**

## Conventions

- **ADRs:** MADR format, `docs/adr/ADR-NNN-kebab-title.md`, front-matter `status`/`date`/
  `decision-makers`/`related`. Numbering is monotonic; new ADRs append.
- **Specs:** `docs/specs/*.md` (+ the two OpenAPI/AsyncAPI YAML files). Cross-reference the ADR(s) they
  implement; ADRs cross-reference the spec(s) that realize them.
- **Rendering:** `docs-site/scripts/build-docs.mjs` auto-discovers all `ADR-*.md` and all `*.md` specs;
  new files appear on the site without further wiring.
