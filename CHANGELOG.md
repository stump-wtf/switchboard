# Changelog

All notable changes to Switchboard are documented in this file.

The format is based on [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Every release section is written by a person from the commit history, not generated
from commit subjects. `[Unreleased]` accumulates the entries for the next release and
is emptied at tag time. Pre-1.0, a superseded surface is removed outright rather than
deprecated, so a breaking change is listed under **Breaking** and carries a note in
[Upgrading](https://github.com/stump-wtf/switchboard/blob/main/docs/guides/15-upgrading.md).

## [0.5.0] - 2026-09-28

Friend-vended endpoints stop acting with the approver's authority, and a worker can end
an attempt without a verdict. **Read the
[upgrade note](https://github.com/stump-wtf/switchboard/blob/main/docs/guides/15-upgrading.md#upgrading-to-v050)
before upgrading**: migration `0025_friend_edges_own_authority` narrows existing friend
endpoints in place, and the verbs it removes cannot be restored.

### Breaking

- **A webhook's rules and routes are managed only from the endpoint that owns it.** The
  ten rule and route verbs called from any other endpoint, including another endpoint of
  the same person, now answer `not_found`. Use the owning endpoint's credential. (#420)
- **Existing friend endpoints are narrowed on upgrade.** Migration
  `0025_friend_edges_own_authority` removes every verb except `create_for` and the drain
  verbs from endpoints minted by approving a friend request, and from their recorded
  grant. It cannot be reversed; back up first. (#420)
- **A friend request that asks only for verbs a friend can never be granted is refused**
  with a 400, over A2A and from the Friends page, instead of being stored. (#420)

### Security

- **Friend endpoints act with their own authority.** A friend endpoint runs on the
  approver's agent, and friend intake accepted any requested tools, so a friend could be
  granted webhook, rule and event-history tools and use them with the approver's authority.
  Friend grants are now limited to `create_for` and the drain verbs, at request and at
  approval, and existing friend endpoints are narrowed to that set on upgrade. A route a
  friend endpoint added before the upgrade is left in place and keeps delivering to it;
  the upgrade note has the query that lists such routes for review. (#420)
- **A webhook's routes and rules are managed only from its own endpoint.** The rule and
  route verbs used to accept any endpoint of the webhook owner's human. Another endpoint's
  webhook now answers `not_found`. To edit a webhook's rules, use the endpoint that owns it.
  (#420)
- Friend requests from two different users' same-named personas no longer collide. (#420)

### Added

- **A worker can end an attempt without a verdict: `release`.** When an attempt ends for a
  reason that is not the work's fault -- the daemon is shutting down, an operator stops it,
  a usage limit is hit -- `release {id, summary?, artifact?, lease_token?}` hands the todo
  back to the queue. The todo returns to `pending` with its attempt counter unchanged, so
  no retry backoff is burned and the attempt does not read as a failure; the attempt closes
  as `released` and the todo is requeued. It takes the same lease-token fence as
  `complete` and `fail`, and an endpoint holds it only when its grant names it. (#507)
- **`summary` and `artifact` on a released attempt.** Both are optional and both are kept
  on the attempt for later claimers. `artifact` is an `mcp://cairn/<id>` handle or an
  absolute `https` URL of at most 512 bytes; Switchboard never fetches it. A URL carrying
  userinfo is refused, because the artifact is handed to every later claimer and rendered
  as a link on the Board. (#507)

## [0.4.0] - 2026-09-27

Routing rules now fail closed, replay targets belong to their endpoint and every replay
passes the SSRF guard, event history is scoped to its owner, and workers can fence their
leases and read a todo's attempt history with `get_todo`. **Read the
[upgrade note](https://github.com/stump-wtf/switchboard/blob/main/docs/guides/15-upgrading.md)
before upgrading**: two changes are breaking, and the replay-target migration cannot be
reversed.

### Breaking

- **Routing rules fail closed.** A rule that errors, times out, runs out of memory or
  budget, or no longer compiles no longer counts as a no-match: evaluation stops there,
  and the delivery is recorded as `faulted` (a new `events.disposition` column) and routed
  nowhere. A webhook with rules answers `503 routing unavailable` when the rule sandbox
  cannot run, instead of routing by default. Rule saves that would fault on the webhook's
  recent deliveries are refused, and `params` values must be strings, numbers, booleans or
  homogeneous lists. There is no switch. Run the query in the
  [upgrade note](https://github.com/stump-wtf/switchboard/blob/main/docs/guides/15-upgrading.md#routing-rules-fail-closed)
  before upgrading to find webhooks whose rules fault today. (#212)
- **Replay targets belong to the endpoint, and every replay passes the SSRF guard.** The
  instance settings `replay_default_target` and `replay_allowed_targets` are gone: the
  migration deletes both rows and nothing reads or warns about them. They used to exempt
  their targets from the SSRF checks for every tenant's replay. An endpoint now owns its
  replay targets, named at vend time (`replay_targets` on `POST /api/v1/endpoints`) and
  checked by the shared SSRF guard; the first is the default when `replay_webhook_event`
  names no `target_url`. With neither, the call fails with the new code
  `replay_target_required`. Every target, owned or not, must be `https` on a public
  address, checked when the call is made and again when it connects, so replaying to
  localhost, a private network or plain `http` no longer works. See the upgrade note. (#421)

### Fixed

- **Switchboard now reports its real version over MCP.** `serverInfo.version` was the
  hard-coded `0.1.0` on every build; it now carries the build's own version, and every
  session's instructions open with a `switchboard <version> (built <date>)` line. The
  version, commit and commit date come from one place, `internal/buildinfo`, which also
  fills in the version and commit of a `go install …@vX.Y.Z` build that no release
  pipeline stamped.
- **Release binaries report the full tag.** `switchboard version` on a release prints
  `switchboard v0.3.0` rather than `switchboard 0.3.0`, matching a `go install` build, and
  the CLI's `User-Agent` changes the same way. Release container images report the tag
  too, and images built from `main` report a `git describe` version instead of a bare
  commit hash.

### Security

- **Event history is scoped to its owner.** `list_webhook_events`, `get_webhook_event`,
  `replay_webhook_event` and the `switchboard://events/recent` resource could read and
  replay every tenant's deliveries. Each event now records the endpoint that owns it, and
  every history read and replay returns only the caller's own events; another tenant's
  event id answers `not_found`, like an id that does not exist. Event deduplication is
  keyed per owner as well. Existing events are backfilled from their webhook. Events whose
  owner can no longer be established (their webhook was deleted before this release) stay
  invisible, to the tools and to the board, and age out through retention. (#194)
- **Friend-approved endpoints read no event history.** A friend endpoint is minted on one of
  the approver's agents, so owner scoping alone would have let a friend request that asked for
  the event-history tools read and replay all of the approver's deliveries. A friend endpoint
  now lists nothing and every event id answers `not_found`, whatever it was granted, until
  friend endpoints get their own authority (#420). (#194)
- **The board's dedup markers stay within one owner.** The feed's `deduped` stage, the LIVE
  rate and the todo dedup badge no longer match another owner's delivery that shares an
  external id. (#194)
- Permanently deleting an endpoint now also deletes the deliveries it owns. Todos those
  deliveries created on a friend's endpoint keep their payload but lose the event link, so they
  read as trust `queue` and no longer ring as verified. (#194)

### Added

- **Lease-token fence.** `claim` and `claim_next` take `require_fence`; a fenced claim returns a
  one-time `lease_token` that `heartbeat`, `complete` and `fail` must then present, so another
  worker on the same endpoint, or a stale one, gets `conflict` instead of closing the attempt.
  Only the token's SHA-256 is stored. (#325)
- **`get_todo`.** Reads one todo with its `result`, `next_retry_at`, `dead_letter` and its attempt
  history, newest first (`attempts_limit`, default 20, maximum 50). `list_todos` implies it, so
  existing endpoints get it without a re-vend. A foreign todo, or one outside the granted queues,
  answers `not_found` exactly as an unknown id does. (#326)
- **`release`.** Hands a held todo back to `pending` without a verdict (shutdown, operator stop,
  usage limit): no backoff, attempt counter unchanged, and the attempt closes `released` with an
  optional `summary` (cut to 2048 bytes, `summary_truncated`) and `artifact` (an `mcp://cairn/`
  handle or an absolute `https` URL; anything else is `invalid` and changes nothing). It honours the
  lease-token fence. It is its own grant, listed by the vend wizard and the consent screen, and no
  other verb implies it. (#328)

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
