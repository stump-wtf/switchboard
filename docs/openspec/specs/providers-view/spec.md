---
status: deprecated
date: 2026-07-18
implements: [ADR-0020]
requires: [SPEC-0001, SPEC-0002, SPEC-0015]
---

# SPEC-0017: Providers View, Runtime Registry, and Connect Wizard

> **Retired 2026-09-21, with the shared-receiver removal.** The provider registry was removed whole: the Providers view, the
> connect wizard and catalog, provider lifecycle, env seeding, `SWITCHBOARD_OPERATOR_SUBJECTS`, the
> `list_providers` MCP tool, and the `adapters` table (dropped by migration 0021). No code implements
> these requirements. Self-managed webhooks
> ([ADR-0012](../../../adrs/ADR-0012-agents-self-manage-webhooks.md), `POST /webhooks/w/{token}`,
> [SPEC-0001](../webhook-ingestion/spec.md)) are the only ingestion surface: instance-wide ingestion
> belongs to no tenant, so it cannot name the endpoint that owns the todos it mints (ADR-0022). Kept
> as history.

## Overview

Providers become first-class, runtime-configurable objects (ADR-0020): a DB-backed registry that
ingestion resolves live, a Providers view grouping every line by family (webhook · push / queue ·
pull) with trust and health, a connect-provider wizard, and an honest catalog of
available-but-unimplemented sources. Verification semantics themselves (SPEC-0001) and adapter
mechanics (SPEC-0002) are unchanged — this spec governs where provider configuration lives and how
operators manage it.

## Requirements

### Requirement: Runtime Provider Registry

Provider configuration SHALL live in a DB-backed registry: name, family (`webhook|queue`), kind,
trust mode, enabled flag, encrypted secret, kind-specific config, and health/rate fields. Webhook
routing and the pull-adapter runner SHALL resolve providers from the registry at request/poll time,
so registry changes take effect without restart. Provider secrets SHALL be stored encrypted using
the existing envelope — never plaintext.

#### Scenario: Wizard-created provider is live immediately

- **WHEN** a new generic webhook provider is created through the UI
- **THEN** its ingestion URL accepts (and trust-checks) calls without a process restart

### Requirement: Environment Config Import

Providers configured via environment/deployment config SHALL be imported into the registry at boot
as an idempotent seed (create-if-absent, never clobber operator edits). After import the registry
is authoritative; env changes to an existing provider SHALL NOT silently override registry state.

#### Scenario: Boot with existing registry

- **WHEN** the service boots with env config for a provider the registry already holds
- **THEN** the registry row wins and no duplicate provider appears

### Requirement: Providers View

The Providers view SHALL group connected providers by family with per-provider glyph, kind, trust
chip (SPEC-0001 vocabulary — `signed`/`token`/`open`/`queue`), in-rate, last-seen, enabled state,
and a configure affordance. Secrets SHALL never render — only `configured`/`missing` status.

#### Scenario: Trust at a glance

- **WHEN** the operator opens the Providers view
- **THEN** every connected line shows its enforced trust mode as a chip, and no secret material
  appears anywhere in the page

### Requirement: Connect Provider Wizard

Connecting SHALL be a wizard (SPEC-0015 wizard pattern). Webhook path: choose source → choose trust
mode (`signed` only where a real scheme exists; `token` as the default for generic senders; `open`
only behind an explicit warning) → provide/generate the secret → receive the copyable ingestion
URL. Queue path: choose an implemented adapter and provide connection settings (e.g. Redis
stream/list/pubsub). The wizard SHALL end with the provider registered, enabled, and visible on the
view.

#### Scenario: Homelab sender lands on token

- **WHEN** the operator connects a generic homelab sender and accepts defaults
- **THEN** the provider is created with `token` trust and the final step shows the URL and the
  token exactly once in the standard reveal pattern

#### Scenario: Open requires intent

- **WHEN** the operator selects `open`
- **THEN** the wizard requires an explicit acknowledgement of the risk before proceeding

### Requirement: Provider Catalog

The view SHALL show implemented-but-unconnected kinds as connectable and not-yet-implemented kinds
(e.g. SQS, NATS, AMQP) as **available** catalog cards that describe what connecting will mean but
expose no functional connect path. Catalog entries SHALL be visually distinct from connected and
connectable states — the UI never fakes a backend that does not exist.

#### Scenario: Catalog honesty

- **WHEN** the operator opens the SQS catalog card
- **THEN** it reads as planned/available with no wizard entry point that would dead-end

### Requirement: Provider Lifecycle

Operators SHALL be able to disable (stop accepting/polling, keep history), re-enable, rotate the
secret (token/signed webhook kinds), and remove a provider. Removal SHALL require confirmation and
SHALL NOT delete previously ingested events or todos.

#### Scenario: Disable stops the line

- **WHEN** a webhook provider is disabled
- **THEN** its ingestion URL rejects new calls while existing events and todos remain queryable
