---
status: accepted
date: 2026-10-01
decision-makers: [joestump]
governs: [SPEC-0006, SPEC-0007, SPEC-0011, SPEC-0015, SPEC-0035]
related: [ADR-0002, ADR-0008, ADR-0012, ADR-0022, ADR-0023, ADR-0027, ADR-0038, ADR-0039]
---

# ADR-0043: A Per-Endpoint Default Claim Lease, Set by Its Human

> **Implementation status: shipped.** `endpoints.default_lease_ttl_seconds` (migration 0029),
> resolved by `internal/lease`; editable on `POST/PATCH /api/v1/endpoints`, the Endpoints view, and
> `switchboard endpoint vend|edit --lease-ttl`; read and set by the endpoint itself through
> `get_default_lease` and `set_default_lease`.

## Context and Problem Statement

A claim holds a lease ([ADR-0002](ADR-0002-postgres-persistence-and-retention.md), SPEC-0003): while
it is live the todo is invisible to every other worker, and when it lapses the reaper requeues the
todo for another worker. The lease was 300 seconds unless every `claim`, `claim_next` and `heartbeat`
call passed `lease_ttl_seconds` (capped at 86400). There was no other way to say how long a claim on
an endpoint should last.

Workers whose jobs take 15 to 60 minutes, such as local-model pull-request reviewers, lost their
lease mid-run whenever they forgot the parameter. The todo was reaped and requeued while they were
still working, and a second worker could take it. On 2026-10-01 a reviewer claimed with the default,
was reaped 23 seconds after the lease lapsed, then approved and merged a pull request for a todo it no
longer held. PR #552 made the session instructions state the lease contract, which helps a worker
that reads them. It does not help a worker that forgets, and a second trap remained: a bare
`heartbeat` reset a long lease to 300 seconds, so a worker that claimed for an hour and heartbeated
without the parameter shortened its own lease.

The person who knows how long an endpoint's jobs take is the human who vended it. **How should that
human set how long claims on an endpoint last, without depending on every worker passing a
parameter on every call?**

## Decision Drivers

* **The owner decides, server-side.** A setting that only works when each worker remembers it is the
  status quo. The value has to live on the endpoint and apply when the call says nothing.
* **Per-call intent still wins.** A worker that knows a job is short or long keeps that control, and
  the 86400-second cap stays the hard ceiling for both.
* **Not scope** ([ADR-0008](ADR-0008-human-principal-vended-endpoints.md), SPEC-0007). Scope decides
  *what* an endpoint may do and is immutable. A default lease decides only how long a claim the
  endpoint is already allowed to make is held, and grants nothing a per-call `lease_ttl_seconds` could
  not already ask for. Editing it must not require revoke-and-re-vend, as presence and shift do not
  ([ADR-0027](ADR-0027-endpoint-presence-clock-in-clock-out.md)).
* **The text agents read has to match.** Since #552 the instructions and the claim, claim_next and
  heartbeat descriptions state the lease numbers. With a per-endpoint default they must state that
  endpoint's number, or they teach the wrong contract.
* **Every surface, one rule.** The API, the web UI, the CLI and MCP all set or show the same field
  under the same name, and every agent-facing claim path resolves the lease the same way.
* **Isolation** ([ADR-0022](ADR-0022-endpoint-scoped-todo-ownership.md),
  [ADR-0038](ADR-0038-teams-and-tenancy.md)). Another human's endpoint stays indistinguishable from a
  missing one, and an endpoint can never change another endpoint's setting.

## Considered Options

* **(A) A server-wide configurable default.** One knob for every endpoint. *Rejected:* a review
  worker and a triage worker on the same instance need different leases, and the instance operator
  is not the endpoint's human.
* **(B) A per-queue default.** *Rejected:* queues are a shared namespace (SPEC-0003), so two humans'
  endpoints draining the same queue name would fight over one setting, and a worker's job length
  belongs to the worker, not the queue.
* **(C) Leave it to the instructions (#552).** *Rejected as sufficient:* it depends on every worker
  reading and obeying text, which is the failure being fixed.
* **(D) A per-endpoint default, set by its human and optionally by the endpoint itself.** *(chosen)*

For the endpoint's own access (D), two shapes were weighed:

* **(D1) Self verbs outside the scope allowlist**, as ADR-0027 chose for presence. *Rejected:* a
  human who wants the lease to be theirs alone could not withhold the write.
* **(D2) Allowlisted verbs, granted by default like webhook self-management**
  ([ADR-0012](ADR-0012-agents-self-manage-webhooks.md)). *(chosen)*

## Decision Outcome

Chosen option: **(D) with (D2)**.

* **Field.** `endpoints.default_lease_ttl_seconds`, nullable; NULL means the server default (300).
  The name is the same on every surface. A CHECK constraint holds it to 60..86400, the bounds
  `internal/lease` defines; a test pins the constraint to the constants.
* **Bounds.** Whole seconds from 60 to 86400 inclusive. Out of range is refused (400 /
  `invalid_argument`), never clamped. Null, or `default` in the CLI and the web form, resets it. The
  60-second floor applies to the endpoint's default only: a worker may still pass a shorter
  `lease_ttl_seconds` on one call.
* **Precedence on every agent-facing claim path.** An explicit per-call `lease_ttl_seconds`, still
  clamped to 86400, wins. Otherwise the endpoint's default applies, otherwise the server default.
  This holds for `claim`, `claim_next` and `heartbeat`. A heartbeat without `lease_ttl_seconds`
  extends by the endpoint's default, which removes the bare-heartbeat trap. Those three MCP verbs are
  the only paths where an endpoint takes or extends a lease: the A2A task methods are stubs, the
  human API has no claim route, and a friend-vended endpoint uses the same MCP verbs. Operator Board
  claims are a human's, keyed to no endpoint credential, and keep their own fixed 5-minute lease.
* **Timing.** The default is read on each call that needs it, so an edit applies to the next claim or
  heartbeat on every live session. Leases already granted keep their expiry.
* **What agents read.** A new session's instructions and the `claim`, `claim_next` and `heartbeat`
  descriptions state the endpoint's effective default and whose it is ("this endpoint's default" or
  "the server default"). An edit re-registers those three tools on the endpoint's live sessions, so
  the client gets `tools/list_changed`. MCP fixes instructions at `initialize`, so a live session
  keeps the number it started with there. The `lease_ttl_seconds` schema text is a struct tag, so it
  states only the cap and points at the description.
* **Human surfaces.** `POST /api/v1/endpoints` accepts the field; vend and `GET /api/v1/endpoints`
  return it with `effective_lease_ttl_seconds`; `PATCH /api/v1/endpoints/{ref}` edits it. PATCH
  follows SPEC-0035: reach by slug or id, another human's endpoint is the same 404 as an unknown one,
  a revoked endpoint is **409** `conflict` with its state (as every write to a revoked endpoint
  answers), the per-human write bucket, 64 KiB body cap. The body's key is required and any other key
  is refused, so a caller cannot believe it edited scope. The web UI takes it at quick vend and on the
  wizard's lifetime step and edits it on each active card (`POST /endpoints/{id}/lease`). The CLI has
  `endpoint vend --lease-ttl`, `endpoint edit SLUG|ID --lease-ttl DUR|default` and a LEASE column.
* **The endpoint's own access.** `get_default_lease` reads the calling endpoint's effective default
  and bounds; `set_default_lease {default_lease_ttl_seconds: int|null}` sets it. Neither takes an
  endpoint argument, and the schema refuses extra keys. Both are in `AllVerbs` beside the webhook
  self-management verbs, so the basics vend paths grant them, and the vend wizard offers them
  unchecked like every grant beyond the drain verbs. They are **not friend-grantable**: a friend
  endpoint is vended on the approver's agent ([ADR-0038](ADR-0038-teams-and-tenancy.md) F3), so its
  settings are the approver's, who can change them on the API, the web UI or the CLI. Endpoints vended
  before this change do not hold the verbs (scope is immutable), but their human can set the default
  for them on every human surface, and the default applies to them all the same.

### Consequences

* Good, because a long-running worker keeps its todo when it forgets `lease_ttl_seconds`, and a bare
  heartbeat no longer cuts a long lease short.
* Good, because the human who knows the job length owns the setting, and changing it is an edit, not
  a re-vend.
* Good, because the numbers an agent reads are the numbers the server applies, for configured and
  unconfigured endpoints alike.
* Bad, because a long default makes a crashed worker's todo wait longer before another worker gets
  it: the reaper requeues on lapse, and a 24-hour default means up to 24 hours. That is the trade the
  owner chooses; the per-call value and `release` still let a worker hand work back sooner.
* Bad, because claims and heartbeats without `lease_ttl_seconds` now read the endpoint row once per
  call. It is a primary-key read; a failure refuses the claim rather than guessing a lease.
* Bad, because an endpoint is no longer fully frozen at vend: one non-scope setting changes in place.
  SPEC-0007 now names it, beside presence and shift, as not scope.

### Confirmation

Tests pin: the precedence for `claim`, `claim_next` and `heartbeat` (per-call > endpoint > server,
the cap); an edit applying to the next call on a live session; the bounds (59, 60, 86400, 86401) on
every write path and in the CHECK constraint; null resets; an edit leaving a granted lease's expiry
alone; the instructions and descriptions for a configured and an unconfigured endpoint; the
re-registration on another live session; `set_default_lease` touching only the caller and refusing an
endpoint argument; the verbs being allowlisted, default-granted and not friend-grantable; PATCH reach
(another human's 404 identical to an unknown id), 409 on revoked, required key, unknown keys refused,
rate limit; the web form's CSRF, ownership and inline refusals; and the CLI's conversion, reset and
usage errors.
