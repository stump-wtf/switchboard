---
status: proposed
date: 2026-10-21
decision-makers: Proposed by joestump-agent, pending Joe
extends: [ADR-0029]
related: [ADR-0034, ADR-0039, ADR-0046]
---

# ADR-0049: Announce Dead Letters On The Hook, List Them For The Operator — Silence That Costs A Whole Queue

> **Renumbered 2026-10-23.** This record was filed as ADR-0048, a number the per-queue pause record
> [ADR-0048](ADR-0048-per-queue-pause.md) took on `main` while this PR was open, so it moved to the
> next free number.

**Status note:** this ADR is **proposed**. It ships with an implementation so the choice can be argued
against working code rather than prose; nothing here is settled until Joe accepts it. The requirement
text it implies for SPEC-0024 and SPEC-0013 is listed under Consequences rather than applied: approved
records stay as approved until this ADR is accepted.

## Context and Problem Statement

A todo that stops being retried is the loudest failure mode in the queue and the quietest thing in the
product. It reaches `failed` with `next_retry_at IS NULL` — attempts exhausted at the cap, a lease that
lapsed at the cap, an interrupted wait whose holder died (ADR-0046) — and then says nothing. Every
outbound signal Switchboard has (SPEC-0024) fires on the *opposite* transition: `todo.ready` rings when
work becomes claimable. A todo that dies mid-retry produces a `todo.completed`-shaped silence: no
delivery, no doorbell, no hook. The work simply stops appearing anywhere a consumer or an operator looks
until someone notices a queue that is not moving (issue #588).

Three facts make that worse than a missing notification:

* dead letters are **terminal**. Nothing re-rings them; the only recovery is a human, so the delay
  between "this broke" and "someone knows" is unbounded;
* they are **invisible to the surface agents use**. `list_todos` is endpoint-scoped and returns
  drainable work; the Board's Failed pill shows rows that still have a retry scheduled. A dead letter is
  neither, and an agent that is asked "is anything stuck?" honestly answers "no";
* the attempt history that explains **why** (ADR-0039) already exists on the row, and is already
  attached as `final_attempt` on a terminal close. The information is written and never delivered.

ADR-0034 already reserves an event named `todo.dead_lettered`, but for owner-scope **sinks** (Gotify,
Apprise), which are unimplemented — there are no sink tables or migrations. That leaves the name claimed
by an unbuilt design while the built delivery channel, per-endpoint hooks, stays silent.

## Decision Drivers

* A dead letter must be announced where the consumer already listens. Consumers are wired to hooks; a
  channel they have to poll is a channel nobody polls.
* The hook's existing contract is the whole reason it is safe: per-endpoint scope, queue filter, rate
  limit, Standard Webhooks dual signing, per-attempt SSRF re-validation, identifiers only, no payload,
  no persistence. A second event type must inherit all of it, not negotiate a new one.
* Nothing may leak cross-tenant. An announcement goes only to hooks already allowed to see that todo —
  the same match that delivers `todo.ready` — and only for a todo whose intake passed the SPEC-0011
  sender gate, because the closing attempt's summary is sender-influenced text.
* The operator needs a *listing*, not a firehose: dead letters across every endpoint they own, in one
  place, on demand.
* ADR-0034 must stay buildable. Riding hooks cannot pre-empt the sink design or redefine the event's
  eventual owner-scope meaning.

## Considered Options

* **(A) Sink event only.** Wait for ADR-0034 and announce dead letters through sinks.
* **(B) Ride the existing hooks.** Add `todo.dead_lettered` to the SPEC-0024 channel — same endpoints,
  same matching, signing and guards — fired at the four transitions that actually dead-letter a todo,
  plus one operator surface that lists dead letters across an owner's endpoints.
* **(C) Per-transition events.** A family of new events (`todo.lease_expired`, `todo.attempts_exhausted`,
  `todo.interrupted_reaped`, …) so receivers can branch on the cause.

## Decision Outcome

Chosen: **(B)**.

1. **One new event type on the existing channel.** `todo.dead_lettered` is delivered by the same
   dispatcher that delivers `todo.ready`: the same endpoint liveness and scope check, the same
   `hook.Queues` filter, the same per-hook rate limit, the same dual signing and per-attempt SSRF
   re-validation, the same three attempts and no persistence. A receiver sees one more `type`, one more
   body shape, and changes nothing about how it authenticates or routes.
2. **Identifiers plus the closing attempt; never the payload.** The body carries `todo_id`, `source`,
   `kind`, `queue`, `endpoint`, `created_at`, `reason`, `attempts_total`, and a nested `final_attempt`
   (`seq`, `outcome`, `summary`, `artifact`, `at`). The summary and artifact come from ADR-0039's closed
   attempt and are neutralized and length-bound like every other sender-written field. No todo payload,
   no result blob, no credential.
3. **Fire where the row actually stops retrying.** `FailTodoWith` (attempts exhausted),
   `FailTodoOperatorOwned` (the same, operator path), the lease reaper at the cap, and the
   interrupt-lease reaper (ADR-0046). Each names its own reason — `attempts_exhausted`, `lease_expired`,
   `interrupt_expired` — rather than deriving one from the attempt's disposition, because the interrupt
   path can close an attempt as `reaped` for reasons other than a dead letter and would be mislabelled.
4. **A revocation cascade stays silent.** Killing an endpoint dead-letters everything it still held, in
   one transaction; those todos are undrainable by construction and the count is a batch, not a signal.
   One webhook per row would turn a single operator action into a delivery storm against a receiver that
   is by definition no longer being served. Those rows appear in the operator listing, which is where a
   cascade is reviewed.
5. **The sender gate holds.** An intake that never passed per-source verification announces nothing, on
   either event, and so does a todo created without a delivery at all. That gap is exactly what the
   operator listing covers.
6. **The operator surface is a Board filter.** A sixth pill, **Dead**, over
   `store.ListDeadLetters(owner, query, limit)`: rows in `failed` with no scheduled retry, across every
   endpoint owned by the signed-in human, quarantine excluded, newest first. `list_todos` stays
   endpoint-scoped and stays about drainable work — an agent's view of its own lane should not widen into
   a fleet-wide failure report.

## Consequences

* **Silence stops being the default at the worst moment.** A consumer wired to a hook learns that its
  work died while it was asleep; the operator learns where to look without a query.
* **Additive, not a pre-emption of ADR-0034.** Hooks are per-endpoint and agent-facing; sinks are
  owner-scope and human-facing. The hook event is the agent's copy of the same underlying fact. When
  sinks land, `todo.dead_lettered` fires once at the transition and is delivered to both audiences;
  nothing here defines sink routing, severities or subscriptions, and no sink tables are added.
* **A new kind of outbound request.** Dead letters now cause HTTP calls to tenant-configured URLs.
  Everything that makes `todo.ready` safe (SSRF re-validation per attempt, no redirects to private
  ranges, rate limits, the kill switch `notify_hooks_enabled`) now also governs them, and the delivery
  metrics gain a third `type` value, `todo.dead_lettered`.
* **A receiver may see an event it did not ask for.** A hook subscribed to a queue receives dead letters
  for that queue without opting in — the same posture `todo.ready` already has. A receiver that must not
  see them filters on `type`, or turns hooks off.
* **Revocation stays quiet on purpose**, and that is a decision, not an oversight: a cascade can dead
  letter thousands of rows at once (#169 is the reason they are dead-lettered at all).
* **Per-lapse `todo.lease_expired` is deferred.** Below the cap, a lapsed lease is a requeue with a
  `todo.ready` re-ring, which is already the correct signal; announcing both the lapse and the re-ring
  would double the noise for the most common transient in the system. Only the at-cap lapse terminates,
  and only that one announces.
* **No requeue-from-dead-letter action, no endpoint-card badge, no "max total claim age"**, all named in
  the thread and none of them needed for the loop to close. Revocation-cascade announcements are
  deferred with the same reasoning as decision 4.
* Implied requirement text, for SPEC-0024 REQ-5/REQ-6 and SPEC-0013 "Todos View" once this ADR is
  accepted: the channel MUST deliver `todo.dead_lettered` with the field set above to exactly the hooks
  that can deliver `todo.ready` for that todo, MUST apply the SPEC-0011 sender gate to it, MUST fire once
  per terminal transition and MUST NOT fire for a revocation cascade; and the Todos view MUST offer a
  Dead filter whose count is the number of terminal failures with no scheduled retry across every
  endpoint the signed-in human owns.

### Confirmation

`internal/notifyhook/deadletter_test.go` pins the signed body against the reference Standard Webhooks
verifier, the exact field set, the no-payload rule and the 4 KiB bound, that a dead letter and a ready
travel through the same hook matching (and that a hook on another queue stays silent), that a rebinding
attempt is refused without retry, that sender text is neutralized, and that a dead letter with no closed
attempt still announces. `internal/store/todos_dead_letter_hook_test.go` pins firing at the attempt-cap
fail, the at-cap lease reap, the interrupt reclaim and the operator fail — and silence below the cap,
for an unverified sender, and across a revocation. `internal/server/notify_dispatcher_wiring_test.go`
pins that the store's dead-letter hook is actually wired to the dispatcher at startup.
