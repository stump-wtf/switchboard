# switchboard — Design Docs Index

Switchboard's design record is **SDD-canonical**: [Architecture Decision Records](adrs/) in
`docs/adrs/` capture the *decisions*; [OpenSpec specifications](openspec/specs/) in
`docs/openspec/specs/` formalize the *requirements* (RFC 2119 + WHEN/THEN scenarios) that realize
those decisions; and machine-readable [reference contracts](reference/) live in `docs/reference/`.
The published site renders this tree via `docs-site/scripts/build-docs.mjs`.

New here? Start with the **[PRFAQ](prfaq.md)** (working-backwards press release + FAQ) for what
Switchboard is and why, then read the decisions and specs below.

Two layers:

- **Event-store core** — receive, verify, persist, and expose inbound webhooks to MCP
  clients and a local web UI.
- **Agent layer** — inbound events become durable **todos**; humans register agents and are vended
  scoped MCP endpoints; personas are A2A Agent Cards; cross-agent work is granted by human-approved
  friending; and todos are pushed into live harness sessions over Claude Code Channels, with the
  durable queue staying the ledger.
- **Cross-cutting** — [ADR-0015](adrs/ADR-0015-implementation-language-go.md) pins the
  implementation language (**Go**).

## Architecture Decision Records (MADR)

| ADR | Title | One-line |
|-----|-------|----------|
| [ADR-0000](adrs/ADR-0000-project-naming-and-scope.md) | Project naming & scope | The project is `switchboard`; MVP scope ratified. |
| [ADR-0001](adrs/ADR-0001-web-stack-go-htmx-pico.md) | Web stack | Go `net/http` + chi, `html/template`, HTMX; assets embedded via `embed.FS` (CSS layer amended by ADR-0016). |
| [ADR-0002](adrs/ADR-0002-postgres-persistence-and-retention.md) | PostgreSQL persistence & retention | Postgres queue store: SKIP LOCKED claims, ON CONFLICT dedup, partial pending index, age + row-cap pruning. |
| [ADR-0003](adrs/ADR-0003-per-provider-ingestion-and-trust-model.md) | Ingestion provider types & trust | Per-provider verification and the trust model. *Amended (shared receivers removed):* reached only through self-managed webhooks (`signed`/`token`); the `queue` family and operator-configured receivers are gone. |
| [ADR-0005](adrs/ADR-0005-mcp-tool-and-resource-contract.md) | MCP tool/resource contract | `list`/`get`/`replay` + recent-events resource. *Amended (shared receivers removed):* `list_providers` removed. |
| [ADR-0007](adrs/ADR-0007-todos-as-core-primitive.md) | **Todos as the core primitive** | Durable work-items (lease/ack/idempotency), not a message inbox. |
| [ADR-0008](adrs/ADR-0008-human-principal-vended-endpoints.md) | **Human principal + vended endpoints** | Humans authenticate; agents get vended scoped MCP endpoints; IdP holds humans only. |
| [ADR-0009](adrs/ADR-0009-personas-as-scoped-agent-cards.md) | **Personas as scoped Agent Cards** | One agent → many personas; a persona is a verb-subset, advertised as an A2A card. |
| [ADR-0010](adrs/ADR-0010-a2a-discovery-human-vended-friending.md) | **A2A discovery + human-vended friending** | A2A discovers; MCP tools; todo-queue transports; approval-is-vend. |
| [ADR-0011](adrs/ADR-0011-identity-assurance-oidc-passkey-deferred.md) | **Identity & assurance** | Simple OIDC (Pocket ID) now; passkey `amr`/`acr` step-up deferred. |
| [ADR-0012](adrs/ADR-0012-agents-self-manage-webhooks.md) | **Agents self-manage webhooks** | Webhook CRUD within a human-vended ceiling; switchboard owns verification. |
| [ADR-0013](adrs/ADR-0013-channels-push-delivery.md) | **Channels push-delivery** | Claude Code Channels pushes into a live session as a notify layer; the durable todo queue stays the ledger. |
| [ADR-0014](adrs/ADR-0014-ingestion-adapters-push-pull.md) | ~~Ingestion adapters (push/pull)~~ | **Superseded (shared receivers removed).** Push + pull adapter families; the pull family and the operator-configured receivers were removed — self-managed webhooks (ADR-0012) are the only ingestion surface. |
| [ADR-0015](adrs/ADR-0015-implementation-language-go.md) | **Implementation language = Go** | Go for the concurrent queue service: goroutine workers, single static binary, official Go MCP/A2A SDKs, `pgx` + `SKIP LOCKED`. |
| [ADR-0016](adrs/ADR-0016-operator-design-language.md) | **Operator design language** | Direction 1a "Operator" (brass & bakelite) is canonical; owned `tokens.css` + `.sb-*` layer replaces the never-shipped Pico.css; fonts vendored. |
| [ADR-0017](adrs/ADR-0017-mcp-streamable-http-only.md) | **MCP over Streamable HTTP only** | Vended endpoints served HTTP/S-direct from the central service; URL + bearer credential is the whole client; stdio adapter retired. |

### Operation Stumply (accepted 2026-09-22)

ADR-0018 to ADR-0029 are not indexed here yet; browse [`adrs/`](adrs/) for them.

| ADR | Title | One-line |
|-----|-------|----------|
| [ADR-0030](adrs/ADR-0030-doorbell-acknowledgement-and-self-test.md) | **Doorbell acknowledgement + self-test** | A claim acknowledges a ring, unheard rings are counted, and `test_doorbell` / `switchboard doctor` prove the loop. |
| [ADR-0031](adrs/ADR-0031-fail-closed-trusted-actor-intake-and-quarantine.md) | **Fail-closed trusted intake** | Rule faults stop evaluation; `trusted_actors` is a first-class field; everything else is quarantined. |
| [ADR-0032](adrs/ADR-0032-release-version-reporting-and-upgrade-contract.md) | **Release and upgrade contract** | One stamped version on every surface, a CI-enforced CHANGELOG, upgrade notes, and superseded surfaces removed outright pre-1.0. |
| [ADR-0033](adrs/ADR-0033-reply-to-source.md) | **Reply to source** | Todos carry a verified reply address; the opt-in `reply` verb posts back with an owner-scoped connection. |
| [ADR-0034](adrs/ADR-0034-notification-sinks-and-queue-digests.md) | **Notification sinks + digests** | Gotify and Apprise sinks through a durable outbox, plus queue digests. Notifications, not ticketing. |
| [ADR-0035](adrs/ADR-0035-per-queue-admission-control.md) | **Per-queue admission control** | In-flight and per-window budgets, enforced inside the claim transaction; every claim is charged. |
| [ADR-0036](adrs/ADR-0036-rule-packs-as-installable-presets.md) | **Rule packs** | Versioned, tested presets that must replay green against the webhook's own deliveries before they save. |
| [ADR-0037](adrs/ADR-0037-provider-issued-signing-secrets.md) | **Provider-issued signing secrets** | `awaiting_secret`, the write-only `set_webhook_secret` verb, Slack URL verification, Linear and Plain kinds. |
| [ADR-0038](adrs/ADR-0038-teams-and-tenancy.md) | **Teams and tenancy** | Every resource has one user or team owner; the operator bounds tenant data and never reads it. |
| [ADR-0039](adrs/ADR-0039-attempt-history-on-todos.md) | **Attempt history on todos** | Every committed claim opens an attempt record; `get_todo`, `release` and an opt-in lease-token fence. |

## OpenSpec Specifications

Each capability is a paired artifact: `spec.md` (requirements) + `design.md` (architecture &
rationale). Grouped by layer, in dependency order.

| SPEC | Capability | Realizes | Covers |
|------|-----------|----------|--------|
| [SPEC-0004](openspec/specs/persistence/spec.md) | Persistence | ADR-0002 | PostgreSQL as sole ledger, pgx pool lifecycle, migrations, schema/indexes, LISTEN/NOTIFY, retention. |
| [SPEC-0003](openspec/specs/todo-queue/spec.md) | Todo work-queue | ADR-0007 | Four-state lifecycle, SKIP LOCKED claim, visibility window + heartbeat, lease reaper, idempotent enqueue. |
| [SPEC-0001](openspec/specs/webhook-ingestion/spec.md) | Webhook ingestion | ADR-0003, 0012 | Signed/token verification on self-managed webhooks, replay window, idempotency-key extraction, enqueue-as-todo. |
| [SPEC-0002](openspec/specs/queue-adapters/spec.md) | ~~Queue adapters (pull)~~ | ADR-0014, 0003 | **Retired (shared receivers removed).** Pull adapters were removed with ADR-0014; kept as history. |
| [SPEC-0005](openspec/specs/mcp-tools/spec.md) | MCP tool contract | ADR-0005 | Tool/resource shape, JSON schemas, transport, event-history surface. |
| [SPEC-0006](openspec/specs/agent-tools/spec.md) | Agent-facing MCP tools | ADR-0012, 0005 | Todo drain (claim/complete/fail/heartbeat) + webhook self-management within a vended ceiling. |
| [SPEC-0007](openspec/specs/vended-endpoints/spec.md) | Vended MCP endpoints | ADR-0008 | (URL + credential) = scoped capability, hashed at rest, immutable scope, revoke = kill. |
| [SPEC-0008](openspec/specs/identity/spec.md) | Human identity & assurance | ADR-0011 | OIDC (Pocket ID) login, session establishment, deferred passkey step-up. |
| [SPEC-0009](openspec/specs/personas/spec.md) | Personas as Agent Cards | ADR-0009 | Persona record, verb→skill derivation, A2A Agent Card + well-known endpoint. |
| [SPEC-0010](openspec/specs/friending/spec.md) | A2A friending | ADR-0010 | Discover → request → approval-todo → approve(=vend); per-direction, revocable, non-transitive. |
| [SPEC-0011](openspec/specs/channels/spec.md) | Channels push delivery | ADR-0013 | Push semantics over the vended MCP session: notify shape, sender gate, best-effort/lossy, degrade-to-pull. |
| [SPEC-0012](openspec/specs/web-ui/spec.md) | Web UI | ADR-0001 | Baseline web surface: embedded templates, sessions, SSE, security & a11y (screen set refined by SPEC-0013). |
| [SPEC-0013](openspec/specs/operator-board/spec.md) | Operator board | ADR-0016, 0001 | Five-view operator UI (Board/Todos/Endpoints/Personas/Friends), drawer + modals, live SSE, design-language conformance. |
| [SPEC-0014](openspec/specs/mcp-transport/spec.md) | MCP Streamable HTTP transport | ADR-0017 | `/mcp/{endpoint}` HTTP-only MCP: bearer auth, scoped tools incl. heartbeat, channels doorbells, stdio retirement. |

### Operation Stumply specifications (approved 2026-09-22)

SPEC-0015 to SPEC-0023 are not indexed here yet; browse [`openspec/specs/`](openspec/specs/) for them.

| SPEC | Capability | Realizes | Covers |
|------|-----------|----------|--------|
| [SPEC-0024](openspec/specs/notify-hooks/spec.md) | Outbound notify hooks | ADR-0029 | Endpoint-owned, signed, payload-free HTTPS wake-ups for consumers with no live session; presence-aware. |
| [SPEC-0025](openspec/specs/doorbell-acknowledgement/spec.md) | Doorbell acknowledgement + self-test | ADR-0030 | Ring records, claim-as-ack, unheard sweep and metrics, `test_doorbell`, `switchboard doctor`, synthetic todos. |
| [SPEC-0026](openspec/specs/trusted-intake/spec.md) | Fail-closed trusted intake | ADR-0031 | Fault-stops-evaluation, `trusted_actors`, quarantine, classifier endpoints, the public-mirror recipe. |
| [SPEC-0027](openspec/specs/release-contract/spec.md) | Release and upgrade contract | ADR-0032 | `internal/buildinfo`, `/healthz`, CHANGELOG and upgrade-note checks, release-honest docs, opt-in release check. |
| [SPEC-0028](openspec/specs/reply-to-source/spec.md) | Reply to source | ADR-0033 | Reply addresses, the connection vault, the `reply` verb, echo suppression, content limits. |
| [SPEC-0029](openspec/specs/notification-sinks/spec.md) | Notification sinks + digests | ADR-0034 | Gotify/Apprise sinks, subscriptions, outbox delivery, budgets, the `notify` action, queue digests. |
| [SPEC-0030](openspec/specs/admission-control/spec.md) | Per-queue admission control | ADR-0035 | Policy model, charging rule, atomic enforcement at claim, deferred status, withheld doorbells and hooks. |
| [SPEC-0031](openspec/specs/rule-packs/spec.md) | Rule packs | ADR-0036 | Catalog, replay over stored deliveries, two-phase install, upgrades and removal, the built-in packs. |
| [SPEC-0032](openspec/specs/provider-signing-secrets/spec.md) | Provider-issued signing secrets | ADR-0037 | Secret origin, `awaiting_secret`, `set_webhook_secret`, rotation, replay guard, Slack handshake, Linear and Plain. |
| [SPEC-0033](openspec/specs/teams-tenancy/spec.md) | Teams and tenancy | ADR-0038 | Owner model, reach, team roles and invites, team queues, operator surfaces, enrollment, the audit fixes. |
| [SPEC-0034](openspec/specs/todo-attempts/spec.md) | Attempt history | ADR-0039 | Attempt records, died-versus-failed, prior attempts on claim, `get_todo`, `release`, the lease-token fence. |

## Reference Contracts

Machine-readable interface definitions in `docs/reference/`:

| Contract | Covers |
|----------|--------|
| [openapi.yaml](reference/openapi.yaml) | HTTP surface: webhook ingestion endpoints + web-UI routes. |
| [asyncapi.yaml](reference/asyncapi.yaml) | SSE stream for the live web UI. |

## Open questions / to confirm

Decision points Joe should confirm before the code hardens them in further. The ADRs and specs are
now *accepted*/*implemented* (the code follows the proposed defaults below), but each default remains
open to confirmation and is also tracked in the relevant spec's `design.md` **Open Questions** section.

1. **Immutable vs. mutable vended scope** *(proposed: immutable)* —
   [ADR-0008](adrs/ADR-0008-human-principal-vended-endpoints.md),
   [SPEC-0007](openspec/specs/vended-endpoints/spec.md). A vended endpoint's scope is proposed
   **immutable**: change access by revoke + re-vend, not in-place edit. **Confirm.**

2. **Passkey `amr`/`acr` step-up — deferred hardening (must not be crossed silently)** —
   [ADR-0011](adrs/ADR-0011-identity-assurance-oidc-passkey-deferred.md),
   [SPEC-0008](openspec/specs/identity/spec.md). Before federating to / accepting provenance from any
   non-passkey IdP, switchboard MUST require a phishing-resistant `amr`/`acr` claim on friend
   approvals. Exact value/level TBD (RFC 8176 has no standard "passkey" `amr`). **Confirm.**

3. **Event/todo seam** — [ADR-0007](adrs/ADR-0007-todos-as-core-primitive.md),
   [SPEC-0003](openspec/specs/todo-queue/spec.md). The event tools (history/audit) and the todo tools
   (work) are two surfaces over one PostgreSQL layer, events as todo producers. Whether they stay two
   surfaces or events collapse into a sub-record of todos is left to confirm.

4. **Channels push: notification-only vs. inline payload** *(proposed: notification-only)* —
   [ADR-0013](adrs/ADR-0013-channels-push-delivery.md),
   [SPEC-0011](openspec/specs/channels/spec.md). **Confirm.**

5. **Channels two-way (reply tool + permission relay): MVP or defer** *(proposed: defer)* —
   [ADR-0013](adrs/ADR-0013-channels-push-delivery.md),
   [SPEC-0011](openspec/specs/channels/spec.md). **Confirm.**

6. **Channels transport: HTTP-direct vs. local stdio adapter** — **Resolved: HTTP-direct.**
   [ADR-0017](adrs/ADR-0017-mcp-streamable-http-only.md) serves vended endpoints exclusively over
   Streamable HTTP ([SPEC-0014](openspec/specs/mcp-transport/spec.md)); the stdio adapter is retired.

7. **Pull-adapter ack timing: ack-on-store vs. ack-on-complete** — **Moot: pull adapters were
   removed with the shared receivers.** [ADR-0014](adrs/ADR-0014-ingestion-adapters-push-pull.md) is superseded and
   [SPEC-0002](openspec/specs/queue-adapters/spec.md) retired.

## Conventions

- **ADRs:** MADR format, `docs/adrs/ADR-XXXX-kebab-title.md` (4-digit), front-matter `status`/`date`/
  `decision-makers`/`related`. Numbering is monotonic; new ADRs append (retired numbers keep their gap).
- **Specs:** OpenSpec pairs at `docs/openspec/specs/{capability}/{spec.md,design.md}`, numbered
  `SPEC-XXXX`. `spec.md` uses RFC 2119 + `#### Scenario` WHEN/THEN; `design.md` carries the Mermaid
  architecture. Frontmatter `implements: [ADR-XXXX]` links each spec to the ADR(s) it realizes.
- **Reference:** machine-readable contracts at `docs/reference/*.yaml`.
- **Design:** the charm-web design language (ADR-0018) lives at `docs/design/NN-slug.md`
  ([ADR-0016](adrs/ADR-0016-operator-design-language.md)) and renders at `/design` on the site.
- **Rendering:** `docs-site/scripts/build-docs.mjs` auto-discovers all `ADR-*.md`, every
  `openspec/specs/*/` pair, `design/NN-*.md`, and the reference YAMLs; new files appear on the site
  without further wiring.
- **Tooling:** managed with the [SDD plugin](https://github.com/joestump/claude-plugin-sdd)
  (`/sdd:adr`, `/sdd:spec`, `/sdd:plan`, …). See the repo `CLAUDE.md`.
