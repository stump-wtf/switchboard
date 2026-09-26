# Changelog

All notable changes to Switchboard are documented in this file.

The format is based on [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Every release section is written by a person from the commit history, not generated
from commit subjects. `[Unreleased]` accumulates the entries for the next release and
is emptied at tag time. Pre-1.0, a superseded surface is removed outright rather than
deprecated, so a breaking change is listed under **Breaking** and carries a note in
[Upgrading](https://github.com/stump-wtf/switchboard/blob/main/docs/guides/15-upgrading.md).

## [Unreleased]

### Breaking

- **Routing rules fail closed.** A rule that errors, times out, runs out of memory or
  budget, or no longer compiles no longer counts as a no-match: evaluation stops there,
  and the delivery is recorded as `faulted` (a new `events.disposition` column) and routed
  nowhere. A webhook with rules answers `503 routing unavailable` when the rule sandbox
  cannot run, instead of routing by default. Rule saves that would fault on the webhook's
  recent deliveries are refused, and `params` values must be strings, numbers, booleans or
  homogeneous lists. There is no switch. Run the query in the
  [upgrade note](https://github.com/stump-wtf/switchboard/blob/main/docs/guides/15-upgrading.md)
  before upgrading to find webhooks whose rules fault today. (#212)

## [0.3.0] - 2026-09-22

The first release since `v0.2.0`, and the first release under
[ADR-0032](https://github.com/stump-wtf/switchboard/blob/main/docs/adrs/ADR-0032-release-version-reporting-and-upgrade-contract.md).
It carries the webhook-ingestion rework, the operator board fixes and a new Prometheus
metrics surface. **Read the upgrade note before upgrading** — the change to webhook
secrets is breaking and one migration cannot be reversed.

### Breaking

- **Instance-wide webhook receivers are gone. Every sender needs its own webhook.** The
  environment-seeded receivers (`/webhooks/{github,gitea,stripe,slack}`, configured with
  `SWITCHBOARD_{GITHUB,GITEA,STRIPE,SLACK}_SECRET` and
  `SWITCHBOARD_LEGACY_RECEIVER_ENDPOINT_ID`) have been removed, along with the provider
  registry they read from. Each sender now moves to its own webhook, created with
  `create_webhook` or the vend wizard, which returns a unique ingest URL and signing
  secret and binds the webhook to the endpoint that owns the todos it mints. This
  resolves the long-standing "receiver not configured" 503 and the empty providers page.
  Those five variables are now ignored with no warning at startup, and the migration that
  drops the old provider rows cannot be reversed. See the upgrade note for the backup
  step, the full variable list and the per-sender move.

### Added

- **A Prometheus `/metrics` endpoint**, with scrape authentication and a metric registry,
  leading with queue liveness computed at scrape time, and carrying todo lifecycle and
  lease-expiry counters plus webhook ingest and routing-decision counters.
- **Doorbells for work that is already waiting.** A session that opens its notification
  stream is now rung immediately for todos already routed to it, instead of waiting for
  the next delivery.
- **Per-endpoint outbound webhooks**, so a consumer with no session can be notified of
  todos addressed to it.
- **Inbound webhooks are idempotent on the sender's own delivery id**, so a redelivery
  from a generic sender is recognised rather than ingested twice.
- Per-endpoint presence (clock in / clock out), and a **GitHub login provider** with
  session provenance.

### Fixed

- **The operator board no longer spills raw activity text down the page.** Live toasts,
  lane cards and todo rows now survive the `htmx` out-of-band swaps that update them,
  instead of being replaced wholesale.
- **Quick vend now grants a usable webhook scope** when it grants `create_webhook`, so
  the first webhook created from the wizard works instead of being unusable.
- **Webhook rules can route to any queue the owner's endpoints drain**, where they were
  previously limited to the queues of the endpoint that created the rule.
- A routing rule that faults is reported rather than being silently treated as no-match.
- A provider secret or webhook signing secret stored in plaintext now warns, so a
  deployment with the encryption key unset is visible rather than assumed safe.

### Changed

- `golangci-lint` findings are fixed and CI is gated on `make lint`, so the lint gate
  that branch protection names actually runs.
- The `aibot` reviewer no longer takes the merge inputs the action stopped reading.
- Documentation: ADR-0027 / SPEC-0022 (endpoint presence), ADR-0028 / SPEC-0023
  (Prometheus metrics), ADR-0029 (outbound webhooks), ADR-0036 / SPEC-0031 (rule packs)
  and ADR-0038 / SPEC-0033 (teams and tenancy) are recorded, and the Claude Code
  doorbell setup is documented.

## [0.2.0] - 2026-09-15

### Added

- **Per-endpoint ingestion.** A webhook belongs to an endpoint, and the todos it mints
  belong to that endpoint's owner, replacing the operator-owned registries the operator
  board could not attribute to anyone.
- Vend wizard and endpoint cards, so an endpoint, its queues and its webhooks can be set
  up from the web UI.
- Handoff lanes: `handoff`, `lane:*`, `size:*` and `repo:` routing for work orders
  arriving from Cairn.
- A `switchboard` operator CLI for draining, inspecting and repairing the queue.
- Friendly endpoints (grants between endpoints), with a passkey-issuer consent gate on
  the issuer.
- Operator OAuth, and a dev-login path for local development.
- A `docs` companion service alongside the application.

### Changed

- The queue is the record: reading a queue, its rules and its todos is owner-scoped, and
  the operator sees queue names and counts rather than contents.
- The self-hosting guide gained the Docker path and a source-build section.

## [0.1.0] - 2026-08-25

The first release: the durable webhook-to-todo queue, its jq routing rules, and the MCP
server that agents drain it through.

### Added

- **Webhooks become durable todos.** A verified inbound webhook delivery is stored, the
  routing rules are evaluated against it, and it lands on a queue as a todo that
  survives an agent restart.
- **jq routing rules**, ordered, first match wins, with `set_webhook_rules` and
  `test_webhook_rules` so a rule can be dry-run against real stored deliveries before it
  is saved.
- **The MCP server**, with the claim / heartbeat / complete / fail lifecycle, leases with
  a reaper, `claim_next` for competing consumers, and doorbell events that wake a live
  session.
- **Dead-lettering**, so a todo that exhausts its attempts stops consuming a worker and
  stays inspectable.
- **Self-managed webhooks** with a per-webhook signing secret, so a sender can be
  authenticated without a shared instance-wide credential.
- **Web UI**: the operator board, listening history for a queue's deliveries, and the
  wave-1 importers (GitHub, Gitea).
