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

### Added

- **Add-only endpoint scope widening.** Giving an existing lane one more route no longer costs a re-vend.
  `switchboard endpoint scope widen SLUG --add-queue merge` (or `--add-source-type`, `--add-verb`,
  `--webhook-max`) adds to an endpoint's grant instead of replacing it, and
  `switchboard endpoint scope add-queue SLUG QUEUE` is that command with one flag. It prints the old and
  the new grant and asks before writing; the endpoint's credential is untouched, so the ingest URL and
  signing secret the producer already points at keep working — which was the whole problem, since a
  webhook belongs to the endpoint that created it. The command cannot remove anything: arrays are unioned,
  a `--webhook-max` below the current ceiling is refused rather than clamped, and any key that would
  replace a list is rejected before it is read. Every field that moves is recorded with the value it
  replaced, in the same transaction as the change, and `GET /api/v1/endpoints/{ref}/scope/widenings` reads
  that trail back. Narrowing a grant is still a revoke and a re-vend. Proposed as ADR-0048. (#590)

- **Verb-scoped endpoint vend.** `switchboard endpoint vend` takes `--verbs`, accepting verb names
  and/or the presets `todo` (the eight todo verbs), `webhook-admin` (the webhook, routing, rule,
  trusted-actor and event verbs) and `all`, plus `--allow-webhook-admin` as the one-flag opt-in for a
  lane that administers its own webhooks; an unknown name is refused before the deployment mints
  anything, so a typo cannot leave behind a quieter credential than you asked for. `endpoint list`
  gained a VERBS column — a verb count with the administration verbs called out (`29 (19
  webhook-admin)`), or the names under `-v` — so auditing which lanes can rewrite routing is one
  command. Proposed as ADR-0047. (#589)

- **A todo interrupted on a client no longer parks forever when its holder dies.** A row in
  `input-required`/`auth-required` keeps its owner and lease on purpose so the same worker can resume it,
  which also made it invisible to the lease reaper and unclaimable — so a holder that crashed mid-wait
  left the todo stranded until someone called `resume`. The reaper now ages that wait too: past 24 hours
  it returns the row to `pending` (dead-lettering at the attempt cap, with the cause in `result`), and
  `switchboard_interrupt_leases_reclaimed_total{queue}` says how much of the lease-expiry signal came
  from a wait nobody answered. Set `interrupt_lease_ttl_seconds` in `settings` to tune the deadline, or to
  `0` to leave interrupted todos interrupted indefinitely as before. Proposed as ADR-0046. (#594)

- `/healthz` reports the build version as the `X-Switchboard-Version` response header (the body
  stays bare `ok`, so existing probes are untouched). The Upgrading guide's verify step — confirm
  the new build actually took — is now executable over HTTP, without a shell in the container or
  a vended endpoint. (#25)
- **Token-trusted runtime telemetry ingestion.** Self-managed webhooks can use source
  type `telemetry`, with readable severity-aware todo titles and stable sender
  delivery IDs. The operator's source-type ceiling explicitly grants telemetry;
  oversized delivery IDs fall back to body-hash deduplication. A standard-library
  [OTLP JSONL relay and validator](examples/telemetry-relay/README.md) handle compressed
  rotations, redaction, retry checkpoints, and severity filtering. The
  [deployment guide](docs/guides/18-telemetry-relay.md) covers grants, worker triage,
  scheduling, and validation through a worker acknowledgement.
- **Queue-stall nudge: a queue with unclaimed work wakes its consumer again.** A queue could only wake
  a consumer when something arrived upstream, so a consumer that claims one todo per invocation and is
  started by an outbound notify hook could leave real, claimable work sitting `pending` indefinitely —
  every such consumer had to reimplement "is my queue drained" itself. A sweep on the existing reaper
  tick now asks the database that question: a queue whose oldest gate-passing pending todo is older than
  two minutes, on an active endpoint with an enabled notify hook covering it, is woken once, naming its
  oldest pending todo, as an ordinary `todo.ready` whose `reason` is now `stalled`. Nothing on the wire
  changes and no consumer change is needed — an unknown `reason` is just a wake. Pacing lives in a small
  per-`(endpoint, queue)` ledger: 5 minutes, then 20, 1 hour, 6 hours; a claim in that queue resets it,
  a drained queue forgets it, and a queue that never gets claimed stops being woken after five tries
  rather than paying a model turn forever. No new configuration, and no wake for a queue with no
  receiver. Design decision recorded as proposed ADR-0045 with draft SPEC-0038. (#566)

- **Worker verbs on the endpoint CLI.** A vended endpoint's worker verbs are now scriptable without
  carrying an MCP client: `switchboard todo list|next|claim|get|heartbeat|complete|fail|release`
  authenticate with the endpoint credential (`SWITCHBOARD_URL` + `SWITCHBOARD_API_KEY`, or
  `--url`/`--key`/`--slug`) and act as that endpoint with exactly its verb scope, over the endpoint's
  own MCP session. They never fall back to the operator login: with no endpoint credential they
  refuse (`todo next` on an empty queue prints `{"empty": true}` and exits 3, so a one-shot branches
  without reading a body), and `next`/`claim`/`get` keep the webhook payload off the output unless
  `--payload` is passed. `complete` and `fail` take `--result` as a JSON object or `@file`; every
  verb takes `--json`. (#556)

- **Webhook routes from the CLI and the human API.** The human who owns a webhook can now list, add
  and remove the endpoints it delivers to without the owning endpoint's MCP credential:
  `switchboard webhook route list|add|remove WEBHOOK_ID [ENDPOINT]` (ENDPOINT by slug or id, each
  with `--json`), over `GET /api/v1/webhooks/{id}/routes`, `PUT` and `DELETE
  /api/v1/webhooks/{id}/routes/{endpoint_id}`, and `POST /api/v1/webhooks/{id}/routes` with
  `{"target_endpoint_id"}`. They run the MCP routing verbs' own checks: any webhook of your
  endpoints is yours to route, another human's is `404`, and a target that is unknown, revoked or
  another human's without an approved friend request is one `403 forbidden`. A route is what lets a
  rule's `endpoints` name an endpoint, so `webhook rules set` now works for targets that were never
  routed while someone held the owner's key. Adding a route to a revoked endpoint's webhook is
  `409`; listing and removing still work. The MCP verbs are unchanged.

- **A per-endpoint default claim lease.** An endpoint's human can now say how long its claims last
  when a call passes no `lease_ttl_seconds`: `default_lease_ttl_seconds`, 60 to 86400 seconds, or
  unset for the server's 300. `claim`, `claim_next` and `heartbeat` apply per-call >
  endpoint default > 300, so a worker that forgets the parameter keeps its todo, and a bare
  `heartbeat` no longer cuts a long lease back to 300 seconds. Set it at vend (`POST
  /api/v1/endpoints`, quick vend, the wizard's lifetime step, `switchboard endpoint vend
  --lease-ttl 1h`) and change it any time without a re-vend (`PATCH /api/v1/endpoints/{ref}`, the
  form on each endpoint card, `switchboard endpoint edit SLUG --lease-ttl 45m|default`). Out of
  range is refused, never clamped; a change applies to the next claim, and granted leases keep their
  expiry. `GET /api/v1/endpoints` and `endpoint list` show it. Session instructions and the claim,
  claim_next and heartbeat descriptions state the endpoint's own default. New MCP verbs
  `get_default_lease` and `set_default_lease` let an endpoint read and set its own default; they
  are in the default grant beside webhook self-management, never friend-grantable, and an endpoint
  vended before this release needs a re-vend to hold them (its human can still set its default).
  Migration `0029_endpoint_default_lease` adds the nullable column. See
  [ADR-0043](https://github.com/stump-wtf/switchboard/blob/main/docs/adrs/ADR-0043-per-endpoint-default-claim-lease.md).

- **A CLI reference page and a safe-routing how-to on the docs site.** The
  [CLI reference](https://github.com/stump-wtf/switchboard/blob/main/docs/guides/16-cli-reference.md)
  lists every `switchboard` operator subcommand and flag as the binary prints it; the
  [safe-routing how-to](https://github.com/stump-wtf/switchboard/blob/main/docs/guides/17-webhook-routing-safely.md)
  walks the backup → dry-run → apply → verify → roll-back loop for a webhook's routing rules, and
  the vend guide gained a section on storing the vended credential without ever printing it.

- **An `llms.txt` for agents.** The docs site publishes
  [`/docs/llms.txt`](https://switchboard.stump.wtf/docs/llms.txt), an llmstxt.org index of
  the pages an agent needs to connect (connect, security model, self-hosting, operator
  CLI, routing), and the connect page gains a **Paste this to your agent** block that
  points the agent at it. (#340)

### Changed

- **`endpoint vend` defaults to a worker's verbs, not every verb.** A vend that names no verbs now
  grants the eight todo verbs plus the two that read and set that endpoint's own default lease, and
  nothing else. It used to grant all 29, including the ability to rotate or delete webhooks, rewrite
  the routing rules every lane depends on, and replay events — so a lane credential that leaked, or a
  lane agent that was prompt-injected, could reach the whole routing setup. This is the one behavior
  change to expect when upgrading a script: a `switchboard endpoint vend router --queue q` that was
  counting on webhook administration now needs `--allow-webhook-admin`, or `--verbs` naming exactly
  what that lane needs. The ingestion webhook the command mints is unchanged, so a narrowed lane still
  receives deliveries and its queue still drains. The web wizard and quick vend are unchanged: a human
  ticking verbs there has already made the choice. Proposed as ADR-0047. (#589)

### Fixed

- **Idle notification streams stay open behind a proxy.** The MCP standalone GET stream wrote no
  bytes at all while its queues were quiet, and a proxy with an idle timeout reads silence as a dead
  connection (Cloudflare cancels about 125 s after connect). So a connected consumer was dropped on
  a schedule and heard nothing during the down window: the queue kept filling, and the work only
  surfaced when the agent pulled. The stream now emits an SSE comment every 15 s, the keep-alive the
  web UI stream already uses, so a stream stays usable for the session's lifetime. Comments carry no
  protocol meaning, so doorbell delivery, its catch-up ring and the web UI stream are unchanged. (#44)

- **Landing-page docs links resolve on self-hosted instances.** The docs image ships only to a
  registry self-hosters cannot reach, so an instance from the published image served no `/docs`
  and every docs and ADR link on the landing page was a dead relative link. The links are now
  absolute under `SWITCHBOARD_DOCS_BASE_URL`, which defaults to the hosted docs site; operators
  who serve the docs themselves point it at their own base (#563).

- **Rings into an empty room no longer spend a todo's re-rings.** The doorbell heartbeat marked a
  row as rung whether or not anything could hear it, so an agent down for a few sweep cycles came
  back to a spent 5-ring budget and silence: the re-rings had gone to no session at all, and the
  catch-up ring that serves a reconnecting consumer shares that same budget. The sweep now asks
  which endpoints have a session attached to the instance running it, and selects and charges only
  those rows. A todo whose consumer is away keeps its full budget for its return, and an instance
  with nothing attached spends nothing. Ringing of attached endpoints, claimed rows and the pacing
  between rings are unchanged, and the queue remains the ledger: pull still drains anything a sweep
  skipped.

- **Connecting agents are told how leases work.** The MCP session instructions said only
  that a claim "sets a lease", so a worker could claim, never heartbeat, and lose the todo
  to the reaper mid-task. They now say: take work with `claim_next`; a claim holds a
  300-second lease unless you pass `lease_ttl_seconds` (at most 86400); `heartbeat` before
  it runs out, before slow steps and before irreversible actions; a lapsed lease is
  requeued for another worker; `conflict` means stop; end every claim with `complete`,
  `fail` or `release`; and workers sharing a credential claim with `require_fence`. The
  `claim`, `claim_next` and `heartbeat` descriptions state the same bounds, and the
  instructions no longer point every endpoint at `list_todos`.

- **Migration `0028_friend_release_verb` re-broadens the `release` verb that migration
  `0025_friend_edges_own_authority` stripped.** 0025's narrowing list predates the
  `release` verb (#507), so upgrading silently removed it from friend endpoints that held
  it. Every approved edge that requested `release` and lost it gets it back, along with
  its endpoint's scope; the upgrade note's manual re-grant advice is superseded. (#538)

- **`set_webhook_rules` no longer wipes a webhook's params when the call omits them.**
  Omitting `params` now keeps the saved ones; pass `params: {}` to clear them. An
  explicit `params: null` is rejected by input validation rather than treated as either.
  `list_webhook_rules`, `set_webhook_rules`, `add_webhook_rule`, `update_webhook_rule`,
  `move_webhook_rule` and `remove_webhook_rule` now always return a `params` object,
  `{}` when none are set (SPEC-0026 REQ-4, #213).

- **Signing in no longer drops a pending `/oauth/authorize`.** A signed-out browser
  that opened an authorize URL (`switchboard login --no-browser`, a URL-only MCP
  client config) was bounced to a bare login page and then always landed on the
  board after sign-in, losing the client's PKCE request and leaving the CLI to time
  out. The login bounce now carries the original URI as a validated return path, the
  login page forwards it through every login entry, and the session-establishing
  redirects send the browser back to it. The path is re-validated at every hop and
  accepts only rooted same-origin paths, so it can never become an open redirect.
  (#199)

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
  reason that is not the work's fault — the daemon is shutting down, an operator stops it,
  a usage limit is hit — `release {id, summary?, artifact?, lease_token?}` hands the todo
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
- **`quarantine` is a reserved queue name.** Held deliveries (from an untrusted actor, a
  faulting rule, or a rule's new `{"quarantine": true}` action) wait there on the webhook
  owner's endpoint until the owner releases or discards them, or they expire after 30
  days; no agent can list, claim or be rung for them. A faulted delivery is held there
  rather than routed nowhere. Migration `0028` aborts if a todo, endpoint or webhook
  already uses a queue with that name; the
  [upgrade note](https://github.com/stump-wtf/switchboard/blob/main/docs/guides/15-upgrading.md)
  has the query to check first. (#386)
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
- **Attempt history on the Board.** The todo drawer lists each attempt, newest first: who claimed
  it, when, how it ended, and a "died" marker with the last heartbeat when the lease lapsed with
  no report. It updates live while open. The A2UI todo detail lists the same history. (#329)

### Fixed

- **`set_webhook_rules` no longer wipes a webhook's params when the call omits them.**
  Omitting `params` now keeps the saved ones; pass `params: {}` to clear them. An
  explicit `params: null` is rejected by input validation rather than treated as either.
  `list_webhook_rules`, `set_webhook_rules`, `add_webhook_rule`, `update_webhook_rule`,
  `move_webhook_rule` and `remove_webhook_rule` now always return a `params` object,
  `{}` when none are set (SPEC-0026 REQ-4, #213).

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
