---
status: proposed
date: 2026-10-22
decision-makers: Proposed by joestump-agent, pending Joe
extends: [ADR-0023, ADR-0047]
related: [ADR-0003, ADR-0012, ADR-0022, ADR-0032, ADR-0043, ADR-0048]
---

# ADR-0049: Scripted Vend Scope and Webhook Lifecycle From the CLI — One Rule Set, One Place for a Secret

**Status note:** this ADR is **proposed**. It ships with an implementation so the choice can be argued
against working code rather than prose; nothing here is settled until Joe accepts it. The requirement
text it implies for SPEC-0006, SPEC-0007 and SPEC-0016 is listed under Consequences rather than applied.

## Context and Problem Statement

Three operator gaps share one root, and issue #584 reported them together:

1. `switchboard endpoint vend` could only vend the basics scope: one queue, the generic source type, one
   webhook. The web wizard asks for a source-type list, a webhook-queue list, a webhook count and a
   credential lifetime, so an operator who wanted a *router* endpoint (webhooks from Gitea and Stripe,
   delivered to several queues) had to click through the wizard. A deployment that must be reproducible
   cannot be scripted at all.
2. SPEC-0035 already specifies `POST /api/v1/endpoints/{ref}/webhooks`,
   `POST /api/v1/webhooks/{webhook_id}/rotate` and `DELETE /api/v1/webhooks/{webhook_id}`, and the CLI
   verbs beside them. None of the three exists: creating, rotating or deleting a webhook needs the
   owning endpoint's MCP credential, which an operator does not have after vend printed it once, and
   `webhook rules` and `webhook route` reached the same objects from the human side.
3. Both reveals put the secret on stdout. A scripted vend writes it into a terminal, a scrollback buffer,
   a CI transcript and anything that ships terminal output, and the `.mcp.json` stanza the vend prints
   embeds the same bearer token a second time.

The root is that the same decision exists twice: once inside the wizard's steps and once inside the MCP
verb bodies. Two implementations of one rule drift, and scope is immutable after vend (SPEC-0007), so
the drifted copy is not a bug report but a permanent endpoint carrying the looser rule.

## Decision

**One. Vend scope lives in `internal/vend`, reachable by both surfaces.** `ParseLifetime`,
`CheckQueues` and `CheckCeiling` are the wizard's rules moved to a package the API can import.
`POST /api/v1/endpoints` gains `source_types`, `allowed_queues`, `webhook_max` and `ttl`, and the CLI
gains `--source-types`, `--allowed-queues`, `--webhook-max`, `--ttl` and `--from-file scope.json`.
Naming none of them keeps today's behaviour byte for byte, including the auto-minted generic webhook;
naming any of them switches the request to a named ceiling whose omitted fields keep the basics
defaults, so `--source-types gitea` widens what the agent may create instead of silently dropping the
webhook it would otherwise have had. `webhook_max: 0` is the only way to say "no webhook at all", and
it means the endpoint's own too. Unknown keys in a scope file are refused by name: a typo that silently
dropped a ceiling field would mint an endpoint whose scope is not the scope the file appears to say,
and there is no way to edit a vended scope afterwards.

**Two. Webhook lifecycle policy lives in `internal/manage`, not in a second handler.**
`internal/manage/webhooks.go` holds `Create`, `Rotate` and `Delete` parameterized by a `Principal` and a
`Ceiling`, following the precedent `manage/routes.go` set: the human API runs the MCP verbs' checks
rather than a copy of them. The routes in SPEC-0035 mount that implementation, and the CLI verbs are thin
clients of the routes. `internal/mcp/webhooks.go` is not rewritten in this change: its create body grows
the same checks from the same package one step at a time, and that consolidation is the named follow-up.

**Three. A revealed secret goes to a 0600 file when the caller names one.** `--secret-file PATH` writes
the bare value; `--env-file PATH` writes shell-sourceable `KEY=VALUE` lines
(`SWITCHBOARD_WEBHOOK_URL`/`SWITCHBOARD_WEBHOOK_SECRET` for a webhook,
`SWITCHBOARD_MCP_URL`/`SWITCHBOARD_ENDPOINT_TOKEN` for a vended endpoint). The two are mutually
exclusive. The file is created 0600 and `chmod`ed again after the write, so a loose umask cannot decide
who reads a credential and a pre-existing world-readable file cannot be quietly reused. When a
destination is named, nothing prints the secret: not the text reveal, not the `.mcp.json` stanza that
embeds the bearer token, not the `--json` body, which is scrubbed of the token wherever it appears. With
neither flag the reveal goes to stdout exactly as it does today, because a human at a keyboard has to see
it once in order to paste it into the producer.

## Options Considered

- **Duplicate the wizard's checks in the API handler.** Rejected: it is the second implementation this
  ADR exists to avoid, and the failure is invisible until an operator compares two endpoints.
- **Move the wizard onto `internal/vend` in the same change.** Deferred: the wizard's step form, drafts
  and reveal are entangled with its handlers, and the parity claim is testable now because the scripted
  path enforces the wizard's rules, not because the wizard was rewired under it.
- **Accept a supplied `signing_secret` from the human API by default.** Rejected: SPEC-0032 REQ-1 gates
  that on an operator setting, and the shared implementation reads the same switch, so the human route
  cannot be a way to get around it.
- **Write the secret to a temp file and print the path.** Rejected: the caller chose the path so the file
  could be `chmod`ed and cleaned up by the thing that made it; a CLI-owned temp file is a credential left
  in `/tmp` for someone else to reap.
- **A single `--out` flag instead of two.** Rejected: the two destinations hold different documents (a
  bare value versus a sourceable env file), and a flag whose meaning depends on the caller's content is
  a flag people get wrong in a script at 3 a.m.

## Consequences

- A scripted operator can build the same endpoint the wizard builds, and CI can assert it.
- Two surfaces, one rule set: a lifetime, queue name or source type that the wizard refuses is refused
  by `endpoint vend` and by `POST /api/v1/endpoints`, in their own error shapes.
- `webhook_max: 0` is a documented meaning for a zero value, not an accident of the field being absent.
- No credential needs to appear in a transcript, a log or a shell history for a script to use it.
- The human webhook routes reach the same store guards as the MCP verbs, so the ceiling and the trust
  rules cannot answer differently per surface.
- **Follow-ups:** consolidate `internal/mcp/webhooks.go` onto `internal/manage`, move
  `internal/web.parseLifetime` to `vend.ParseLifetime` (the two accept and refuse the same vocabulary
  today, which `endpoints_lifetime_test.go` pins), and cover the parity claim in the spec's own wording
  with a table-driven test that runs one scope through both surfaces.
- Implied requirement text, not applied: SPEC-0006 gains "a vend names its ceiling, and every surface
  validates it alike"; SPEC-0007 gains "a revealed credential may be written to a caller-named file the
  CLI creates for the owner only, in which case no surface prints it"; SPEC-0016 gains "a scripted vend
  sets a lifetime in the wizard's vocabulary".

## References

- Issue: stump.wtf/switchboard#584. Spec: SPEC-0035 (the routes and verbs), SPEC-0006 (the ceiling),
  SPEC-0007 (immutable scope, one-time reveal), SPEC-0016 (lifetime), SPEC-0032 (secret origin),
  SPEC-0026 REQ-6 (the reserved queue). ADRs: ADR-0023 (the operator API), ADR-0047 (verb-scoped vend),
  ADR-0012 (webhook self-management), ADR-0032 (secret origins), ADR-0048 (add-only scope widening).
- Code: `internal/vend/scope.go`, `internal/server/api_vend_scope.go`,
  `internal/server/api_webhook_admin.go`, `internal/manage/webhooks.go`,
  `cmd/switchboard/webhook_admin.go`, `cmd/switchboard/vend_scope.go`.
