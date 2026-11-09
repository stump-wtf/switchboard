---
status: proposed
date: 2026-10-25
decision-makers: Proposed by joestump-agent, pending Joe
extends: [ADR-0012]
related: [ADR-0002, ADR-0003, ADR-0022, ADR-0031, ADR-0037, ADR-0038, ADR-0051]
---

# ADR-0052: Per-Webhook Provider Read Connection — Who May Point a Read at a Provider, and What It May Re-Drive

**Status note:** this ADR is **proposed**. Decision 1 ships with working code in the same pull request so
the choice can be argued against an implementation rather than prose; Decision 2 is a recommendation with
no code behind it yet, because the reconcile pass that would exercise it is #616. Nothing here is settled
until Joe accepts it, and the requirement text it implies is listed under Consequences rather than applied
to SPEC-0026 or SPEC-0003.

## Context and Problem Statement

Part A of this epic (#612, [ADR-0051](ADR-0051-per-source-pre-intake-reject-counts.md)) gave Switchboard a
per-source count of deliveries that were refused before intake. Counting is not recovering. Recovering
means asking the provider what it has and re-driving whatever never arrived, and that needs three things
this repository does not yet hold in one place (#614, #615, #616): where to ask (a provider base URL and a
repository), how to authenticate (a read-only credential), and who is allowed to decide either.

The parts exist separately. `internal/provider` has GitHub and Gitea read clients behind one interface,
exercised so far only against `httptest` (#619). `internal/cred` has an envelope cipher already trusted with
provider-issued signing secrets (ADR-0037, SPEC-0032). `endpoint_webhooks` knows its source type and its
trust gate (SPEC-0026 REQ-5) and nothing at all about a read address.

Two questions are actually open, and only the first is a schema question.

**Where the credential points.** A signing secret proves who is talking to *us*. A provider read token lets
*us* talk to a provider as one of our users, and a reconcile read is the first time Switchboard dials out
with a credential of its own. Whoever can name an address and attach a token can, in principle, aim that
token at a host they control and read the response they want. A prefix test on an allowlist is not a guard:
`https://gitea.stump.rocks.evil.test/` is a well-formed host that satisfies it.

**What a read may re-drive.** A provider read that finds an issue nobody delivered produces, in effect, a
delivery with no signature on it — its only authentication is the token we chose to hold and the address we
chose to aim it at. SPEC-0003 makes an accepted todo durable, and SPEC-0026 puts a fail-closed gate between
a delivery and a todo: an actor outside the webhook's `trusted_actors` is quarantined, not routed. If a read
result skips that gate, then the trust boundary of the whole intake path is "whoever can edit this field",
which is a much smaller set of people than the gate was built to screen. [ADR-0038](ADR-0038-teams-and-tenancy.md)
gives every resource a user or team owner but defines no provider read credential, so tenancy tells us whose
webhook this is and not what its token may do; the ticket that scoped this work cites ADR-0038 as if it did,
and that citation is recorded here as a mismatch rather than quietly resolved.

## Decision

**Decision 1 (implemented in this pull request): the provider connection is per-webhook configuration that
a human writes and an agent reads.**

- It lives on the webhook row: `provider_base_url`, `provider_repo`, a sealed `provider_read_token`, and
  `provider_connection_set_at`. The address half is a pair — base URL and repo are set or cleared together,
  enforced by a `CHECK` — because a read with one half is a read that cannot be aimed.
- The address is canonicalized and validated on write: `https` origin only (loopback `http` excepted for
  local development), no userinfo, query or fragment, and when `SWITCHBOARD_PROVIDER_READ_ALLOWED_ORIGINS`
  is set, **exact origin equality** — never a prefix. A malformed list fails startup rather than silently
  admitting everything, the same way `SWITCHBOARD_NOTIFY_HOOK_ALLOW_CIDRS` behaves.
- The token is sealed with the envelope cipher that already protects signing secrets, is never selected into
  a response body, is added to the redaction field list, and can only be stored when an encryption key is
  configured. Reads report a boolean plus a derived state (`unset`, `awaiting_credential`, `ready`) so an
  operator can see a webhook that has an address but no credential yet, which is the shape #615 hands over.
- Writes are human-only: `PUT /api/v1/webhooks/{webhook_id}/provider-connection` and
  `switchboard webhook connection set`, both reach-guarded like every other SPEC-0035 surface, with the
  three-state token rule the rules editor already uses (omitted key keeps, `""` clears, a value replaces).
  MCP gets the connection on `list_webhooks` **read-only**; there is no new MCP verb. The reasoning is
  Decision 2's: the agent that would consume a reconcile's output is the party a misaimed read can serve, so
  the agent that runs the work does not get to point the credential.

**Decision 2 (recommended, not implemented): a provider-read result is a read of the tracker, not a new
trust root. It may re-drive intake only through the gate intake already has.**

Concretely: when #616 dials, whatever it synthesizes MUST be evaluated by the same webhook's current
`trusted_actors` (SPEC-0026 REQ-5) and quarantined when it fails, MUST NOT be marked `signed`, and MUST NOT
be inserted as a todo directly (SPEC-0003). A read that finds an issue opened by an actor the gate would
hold lands in quarantine with the same reason as the delivery that never arrived — which is exactly the
outcome the operator wants, since a delivery that arrived would have been held too.

## Considered Options

- **Let an agent set the connection over MCP.** Rejected: an agent already chooses what to act on. Letting
  it also choose which host a stored credential is sent to removes the only check on that credential's
  destination. Read visibility costs nothing and keeps the field debuggable from the same tool that needs it.
- **Instance-wide provider configuration.** Rejected: webhooks on one instance point at different repos and
  orgs, one credential would enumerate everything the instance can reach, and the per-webhook row is already
  where trust state lives.
- **Prefix or substring matching on the origin allowlist.** Rejected: it is the classic failure above. Origin
  equality is one line and has no bypass.
- **Store the token in a plain column and redact it on the way out.** Rejected: signing secrets are sealed at
  rest (ADR-0037), and a token that reads a private tracker is at least as sensitive as one that signs a
  delivery.
- **A separate `provider_connections` table.** Rejected for now: the relationship is 1:1 with a webhook, and
  the columns ride the row every webhook read already loads. Revisit if a webhook ever gets more than one
  read target or a read target with its own lifecycle.
- **Trust the read because we chose the address.** Rejected as a design: it makes the intake trust boundary
  rest on nobody ever editing a field, which is not a property this codebase has ever relied on.

## Consequences

**Gains.** #616 has somewhere to read from and one function that opens the credential, so the reconcile pass
is wiring rather than design work; #615 has a storage path with an "address set, credential missing" state it
can finish. The endpoint card and `switchboard webhook list` show whether a webhook could be reconciled at
all, which turns "did we lose this issue?" into a question with an answer before anyone dials.

**Costs and limits.** Switchboard cannot verify that the token it is given is read-only; that stays the
operator's side of the bargain, and the allowlist plus human-only writes plus the docs are what we have for
it. Nothing here dials out: with this PR alone, the new columns are inert. The address is stored per webhook
and is not rewritten when the allowlist narrows, so a webhook configured against an origin later removed from
the list fails at write time on its next edit and at read time in #616, not silently. `provider_connection_set_at`
is when the field last changed, not evidence that the target was ever reachable.

**Implied requirement changes.** SPEC-0035 gains a **Requirement: Provider Connection Config**, and this PR
applies that wording, because the surface it describes ships here. SPEC-0026 REQ-5 and SPEC-0003 gain the
sentences in Decision 2 above — that a provider-read synthesis is evaluated by the gate and cannot bypass it
— and those are **listed here, not applied**, until this ADR is accepted and #616 needs them. This change
persists an address and a sealed credential and grants no source new access.
