---
status: proposed
date: 2026-10-21
decision-makers: Proposed by joestump-agent, pending Joe
extends: [ADR-0008, ADR-0023]
related: [ADR-0012, ADR-0022, ADR-0029, ADR-0042, ADR-0043]
---

# ADR-0047: Verb-Scoped Endpoint Vend — A Lane Credential Holds Only What Its Lane Does

**Status note:** this ADR is **proposed**. It ships with an implementation so the choice can be argued
against working code rather than prose; nothing here is settled until Joe accepts it. The requirement
text it implies for SPEC-0006 and SPEC-0007 is listed under Consequences rather than applied: accepted
records stay as accepted until this ADR is accepted.

## Context and Problem Statement

`switchboard endpoint vend NAME --queue Q` — and the operator API behind it — handed every vended
endpoint `mcp.AllVerbs()`: the eight todo verbs, and also the webhook lifecycle, routing, rule,
trusted-actor and event-replay verbs. The only flags were `--queue` and `--json`, so an operator
scripting a deployment could not ask for less, and 29 verbs is what a lane credential got for a job
that uses eight of them (issue #589).

The blast radius is the problem, not the count. A worker or merge agent that is prompt-injected, or
whose env file leaks, currently holds the ability to rotate or delete the webhook that feeds every
stage, rewrite the routing rules that decide which lane a delivery lands in, and replay events. None
of that is part of draining a queue.

The capability to vend narrowly already exists: the web wizard ticks verbs per endpoint, and the store
already carries `verbs` and a webhook ceiling per endpoint. What was missing was the CLI surface, the
default posture, and any way to see at a glance what a lane holds.

## Decision Drivers

* Least privilege for a credential that runs unattended code written by other people.
* A scripted vend must stay one command; a new mandatory flag would be answered by pasting the flag
  into every script and change nothing.
* No silent narrowing: an operator who means to grant admin verbs must be able to say so, and a
  mistake must fail loudly rather than mint a credential quieter than they asked for.
* One source of truth for the verb families, so the CLI, the API, the wizard and the consent screen
  cannot drift (the pattern ADR-0023 and `internal/mcp/verbs.go` already establish).

## Considered Options

* **(A) Warn-and-opt-in.** Keep the broad default; print the verb count and a warning, and require
  `--allow-webhook-admin` for the admin verbs.
* **(B) Narrow by default, explicit grant.** Default to the worker surface (the todo verbs and the
  endpoint's own lease verbs); `--verbs` takes verb names and/or presets (`todo`, `webhook-admin`,
  `all`); `--allow-webhook-admin` is sugar for adding the admin preset; `endpoint list` shows what each
  endpoint holds.
* **(C) Force a choice.** Refuse to vend until the operator names a verb set, on every call.

## Decision Outcome

Chosen: **(B)**, narrow by default.

1. `endpoint vend` with no verb flags sends no `verbs` field, and the server applies its default: the
   eight todo verbs plus the two verbs that read and set that endpoint's own default lease. The
   default lives in one function (`mcp.DefaultVendVerbs`) that the CLI does not duplicate, so the two
   cannot disagree about what "nothing named" means.
2. `--verbs` takes verb names and presets together, comma-separated and case-insensitive. A name that
   is neither a verb nor a preset is refused **before the deployment mints anything**, because a typo
   that silently grants less than the operator meant is the one failure this surface must not have.
   The server resolves the same way, so a scripted `POST /api/v1/endpoints` behaves identically.
3. `--allow-webhook-admin` adds the admin preset. Naming admin verbs in `--verbs` is itself the opt-in;
   either way the reveal then prints the count and one line on what those verbs can do, so the broad
   grant stays a decision somebody made rather than a default that slipped through.
4. `endpoint list` gains a VERBS column: a count, with the admin verbs called out — `29 (19
   webhook-admin)` — and the verb names under `-v`.

## Mechanics

* `internal/mcp/verbs.go` owns the families, the presets, the canonical order, the resolver and the
  admin-family test used by both the reveal warning and the audit column. `WebhookAdminVerbs()` is a
  contiguous slice of `AllVerbs()`, so a new verb cannot fall outside the audit flag by accident.
* `POST /api/v1/endpoints` gained an optional `verbs` array. Empty or absent is the default grant.
  The webhook ceiling still comes from `mcp.BasicWebhookCeiling(verbs, queues)`, so a narrow grant
  carries `webhook_max = 0`: self-managed webhooks are off, exactly as when the wizard's webhook step
  is left blank.
* **The vended ingestion webhook is unaffected.** It is minted by the API on the operator's behalf
  with its own ceiling of one, not through the endpoint's self-management allowance, so a narrow lane
  still receives deliveries and the webhook→todo→doorbell loop still closes. What a narrow lane loses
  is the ability to create, rotate or delete webhooks and to rewrite routing.
* The web wizard is unchanged: a human ticking chips there has already made the choice, and its verbs
  travel as they always did.

### Consequences

* **Breaking for scripts.** A deployment that vended a router or a webhook manager with a bare
  `switchboard endpoint vend` now gets a worker credential, and the verbs it wanted surface later as
  refused MCP calls. The fix is one flag: `--allow-webhook-admin`, or `--verbs` naming what that lane
  actually needs. The `Changed` entry in the CHANGELOG says so.
* `get_default_lease` / `set_default_lease` (ADR-0043) **stay in the default grant**, as SPEC-0006
  requires, and they widen nothing: they act only on the calling endpoint, and a claim can already ask
  for the same lease through `lease_ttl_seconds`, under the same ceiling. The verbs the default drops
  are the ones that reach past one endpoint — webhooks, routing, rules, trusted actors, event replay.
* Notify-hook verbs (ADR-0029) stay outside every preset, including `all`: they make Switchboard dial
  URLs a tenant chose, so they remain name-only.
* Implied requirement text, for SPEC-0006 "Todo Drain Verbs" and SPEC-0007 once this ADR is accepted:
  a vend that names no verbs MUST grant the drain verbs and the endpoint's own lease verbs, and no
  others; a vend that names an unknown verb or preset MUST mint nothing; and the endpoint listing MUST
  reveal each endpoint's grant without revealing its credential.

### Confirmation

`internal/mcp/verbs_vend_test.go` pins the default grant, preset resolution and the admin partition;
`internal/server/api_vend_verbs_test.go` pins the API default, the resolution, and that a refused verb
mints no agent and no endpoint; `cmd/switchboard/endpoint_verbs_test.go` pins that the default vend
sends no `verbs` field, that an unknown verb exits before the deployment is called, that the admin
grant prints the warning, and that `endpoint list` counts verbs and names them under `-v`.
