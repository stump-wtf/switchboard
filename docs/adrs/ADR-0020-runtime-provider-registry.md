---
status: superseded
date: 2026-07-18
decision-makers: Joe Stump
extends: [ADR-0003, ADR-0014]
governs: [SPEC-0017]
---

# ADR-0020: Providers Become Runtime-Configurable First-Class Objects with a Connect Wizard and Catalog

> **Superseded 2026-09-21, with the shared-receiver removal.** The provider registry was removed whole: the Providers view, the
> connect wizard and catalog, provider lifecycle, env seeding, `SWITCHBOARD_OPERATOR_SUBJECTS`, the
> `list_providers` MCP tool, and the `adapters` table (dropped by migration 0021). Self-managed
> webhooks ([ADR-0012](ADR-0012-agents-self-manage-webhooks.md), `POST /webhooks/w/{token}`) are the
> only ingestion surface: instance-wide ingestion belongs to no tenant, so it cannot name the endpoint
> that owns the todos it mints ([ADR-0022](ADR-0022-endpoint-scoped-todo-ownership.md)).

## Context and Problem Statement

Providers today are invisible plumbing: signed webhook adapters are compiled in, generic providers
are parsed from environment config at boot (`config.ParseGenericProviders`), and pull adapters live
in the `adapters` table with health fields but no management UI. The only surfaces are the MCP
`list_providers` tool and `providerStatuses()`. The redesign introduces a **Providers view** — the
sixth view in the IA — showing every line into the system by family (**webhook · push**, **queue ·
pull**) with trust chips, rates, and health, plus a **connect-provider wizard** (pick source → pick
trust mode → secret → endpoint URL) and a **catalog** of available-but-unconnected sources (SQS,
NATS, AMQP). Boot-time env config cannot back a connect wizard. Where does provider configuration
live, and what is honestly connectable versus catalog-only?

## Decision Drivers

* A wizard that creates providers at runtime requires a **DB-backed registry**; env-only config
  can't mint a new `generic/<name>` route while running.
* ADR-0003's trust doctrine is non-negotiable: trust mode is declared per provider, enforced, and
  displayed — the wizard must make choosing `token` over `open` the easy path.
* ADR-0014 already split families (push/pull) and gave pull adapters a table; the registry should
  converge both families into one model rather than invent a third.
* Honesty over chrome: only Redis pull is implemented. Catalog entries must be visibly
  "available", never fake-connectable.
* Secrets discipline: provider HMAC/shared secrets are operator-supplied; switchboard already has an
  encryption envelope (`internal/cred/envelope.go`) and a rule against plaintext secrets at rest.

## Considered Options

* **(A) Status quo** — env config, no view.
* **(B) DB-backed provider registry** — one `providers` registry (converging with/extending the
  `adapters` table) as source of truth; env config becomes a one-time import/seed at boot;
  view + wizard + catalog on top.
* **(C) View-only** — read-only Providers view over current config; no wizard.

## Decision Outcome

Chosen option: **"(B) DB-backed registry."**

* **Registry**: provider records carry name, family (`webhook|queue`), kind (github/stripe/slack/
  generic/redis/…), trust mode, enabled, secret (encrypted via the envelope), config (JSON: paths,
  channels, modes), and the existing health/rate fields. Boot imports env-configured providers into
  the registry (idempotent seed) so deployment config still works; the registry wins thereafter.
* **Ingestion binds to the registry**: webhook routes (`/webhooks/*`, `generic/<name>`) and the
  pull-adapter runner resolve providers from the registry at request/poll time, so wizard-created
  providers are live without restart.
* **View**: the Providers view groups by family, shows trust chips (ADR-0003 vocabulary), in-rates,
  last-seen, and connect state.
* **Wizard**: webhook path — source → trust mode (`signed` where a scheme exists; `token` default
  for generic; `open` behind explicit warning) → secret → copyable endpoint URL. Queue path —
  connection settings for implemented adapters (Redis stream/list/pubsub).
* **Catalog**: SQS/NATS/AMQP render as **available** cards that explain what connecting will mean;
  selecting one files intent (disabled CTA / "planned") — no stub backends ship until a real
  adapter lands (per ADR-0014's adapter contract).

### Consequences

* Good: providers stop being deploy-time trivia; the trust model becomes something operators *do*
  in the UI; wizard-created homelab senders get the token tier by default.
* Bad: config→DB authority handoff needs care (import semantics, drift between env and registry);
  a security-sensitive secret-handling surface moves into the web tier.
* Neutral: adapter implementations themselves (ingest verification, Redis modes) are untouched.
