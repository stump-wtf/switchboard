---
status: proposed
date: 2026-10-30
decision-makers: Proposed by joestump-agent, pending Joe
extends: [ADR-0003]
related: [ADR-0012, ADR-0022, ADR-0037, ADR-0048, ADR-0050, ADR-0052, ADR-0053]
---

# ADR-0055: Issue-Dependency Holds — A Todo Whose Issue Is Blocked Is Visible But Unclaimable

**Status note:** this ADR is **proposed**. Decisions 1-4 and 6-7 ship with working code in the same pull
request (stump.wtf/switchboard#583) so the choices can be argued against an implementation; Decision 5
deliberately deviates from what the originating report asked for, and that deviation is stated in the open
below rather than buried in a call site. Nothing here is settled until Joe accepts it, and the requirement
text it implies is listed under Consequences rather than applied to SPEC-0003 or SPEC-0013.

## Context and Problem Statement

Work that is not yet doable still arrives. A release-handoff todo for `stump.wtf/switchboard#583` is minted
the moment an event touches that issue, and if the issue says "blocked by #601 and #612", the todo is
nonetheless handed to the first agent that asks for work. The agent burns a lease and an attempt on it,
fails or reports that it cannot proceed, and the todo burns the rest of its retry budget — while the
operator sees a queue full of work that was never startable. Upstream reports this as
[stump-wtf/switchboard#34](https://github.com/stump-wtf/switchboard/issues/34): the tracker already knows
what blocks what, and Switchboard ignores it.

The facts that constrain any answer, verified against Gitea 1.27.3 in that report:

- Gitea has first-class issue dependencies (`GET /repos/{o}/{r}/issues/{n}/dependencies` returns the
  blockers, `/blocks` the dependents), returning full issue objects with `state` and the blocker's own
  `repository.full_name`, so blockers are routinely cross-repository, and the list is paginated.
- **Adding or removing a dependency link fires no webhook.** The only event in the life of a dependency is
  the close of the blocker itself.
- The delivery payload carries no dependency information, so answering "is this blocked?" costs one
  authenticated provider read — the per-webhook connection of
  [ADR-0052](ADR-0052-per-webhook-provider-read-config.md) read through the clients of
  [ADR-0053](ADR-0053-provider-read-clients-for-reconcile.md). This is the first consumer of that config.
- GitHub has no issue-dependency API at all. Silence must mean "say nothing", not "block forever".
- A blocker closed by any route (merge, manual close, a later duplicate) is a closed blocker, and a reopen
  must re-hold.

What is *not* in question: the tracker stays the place dependencies are declared. Switchboard consuming
that read beats a second dependency graph that drifts from the one humans edit.

## Decision

**Decision 1: `blocked` is a real `todos.state` value, not a side column.**

Migration 0036 rebuilds the `todos_state_check` constraint to admit it, following the NOT VALID / VALIDATE
pattern migration 0012 used, and adds `todos.issue_ref` plus the `todo_blockers` table that records *which*
issues a hold waits on. Every existing claim path selects `state = 'pending'` (or a lapsed `claimed` /
parked `failed` row), so an added state is unclaimable by construction — no claim SQL changes, and the
state cannot be forgotten at a future call site the way a boolean column would be. It also makes the hold
honest on every surface that already reads `state`: `list_todos`, the Todos table, `queue_stats`, and
Prometheus, all of which now distinguish it instead of reporting held work as waiting-to-be-taken.

The two announce paths gained the same short-circuit quarantine already has (`fireDoorbell`, `fireReady` in
`internal/store/store.go`): a held todo rings no doorbell and notifies no hook **at the store**, so a
caller that forgets the rule cannot leak a claim prompt. The todo is still created, still idempotent, and
fully visible — holding is not hiding.

**Decision 2: a hold is keyed by the tracker issue, and events while held coalesce into it.**

`uq_todos_held_per_issue` is a partial unique index on `(endpoint_id, queue, issue_ref)` for rows in state
`blocked`. A delivery for an issue that is already held folds into the existing row
(`coalesced_count = coalesced_count + 1`) instead of minting a second copy. Without it, ten comment edits
on a blocked issue would produce ten todos that all release at once — the duplicate-delivery problem
idempotency keys already solved, arriving from the other direction. One issue, one held todo, one release.

**Decision 3: the release signal is a recorded blocker closing, resolved from stored refs.**

`todo_blockers` stores each blocker as `owner/repo#number` with its state at last sight. When an
`issues/closed` delivery arrives, `MarkBlockerState` stamps that ref closed and releases every blocked row
whose remaining refs are all closed — one indexed read, no provider call, no credential on the release
path. This matters: the close of the blocker is the one event in a dependency's life, so the common case
must not depend on outbound HTTP being reachable at that instant.

**Decision 4: two paths re-check, because links can be edited in silence.**

The close path above, plus a periodic sweep (`internal/server/dependencies.go`, one pass every
`deps.DefaultRecheckInterval`, 50 candidate todos per pass, immediate first pass) that re-reads dependencies
for held todos and for todos whose hold has gone stale. It also releases **orphan** holds — a blocked row
whose recorded refs are all closed but that never saw the close event, which is what a missed webhook
leaves behind — and counts them separately (`RecheckReport.Orphans`) so a sweep that is quietly doing
nothing is distinguishable from one that is quietly cleaning up. A reopen re-holds: a `pending` todo whose
issue now has open blockers is moved to `blocked` (`HoldTodo`, `ErrConflict` if it has since been claimed).

**Decision 5 (deviation from the report): nothing dials in the claim path.**

The report's fifth point asked for a dependency check when an agent claims. This does not do that. The
claim path is a transaction on the hot path of every agent in the system; putting an outbound HTTPS call
inside it would make claim latency someone else's API availability, and would need a credential resolved
per claim. The check happens instead where the todo is born (intake) and on the two re-check paths above,
which bounds staleness to one sweep interval — a blocked issue becomes claimable at most five minutes
after its last blocker closes, instead of at the instant an agent asks. If that bound is unacceptable for
some queue, the right fix is an event-driven re-check on that provider, not HTTP in the claim transaction.

**Decision 6: every unknown fails open.**

No connection configured, the provider says it has no dependency model (GitHub), the read errors, times
out, returns a page that cannot be parsed, or names a blocker whose repo cannot be resolved — all of these
mint an ordinary `pending` todo and log at debug. A hold is asserted only on positive evidence that a
blocker is open. Switchboard blocking work it cannot verify is the failure mode this feature could
worst introduce: an unreachable tracker would otherwise freeze every queue behind an outage.

**Decision 7: the interval is a constant, not an environment knob.**

`deps.Config` exists so a test or a future deployment can set read timeout, cache TTL, sweep interval and
sweep limit, and nothing reads an environment variable for them. This repository's config surface is
`SWITCHBOARD_*` and each knob is a support commitment; a hold's staleness bound is not yet a promise anyone
needs to tune.

## Considered Options

- **A `blocked_at`/`blocked_reason` column on a still-`pending` row.** Rejected: the row stays claimable
  until every claim site remembers the column, and every listing and count reports held work as available.
  That is a rule enforced by memory, in the one place this repository has repeatedly stopped trusting
  memory (quarantine, ADR-0048 per-queue pause, and the announce gating in `store.go` all chose a state
  or a queue instead).
- **A dedicated `blocked` queue, like quarantine.** Rejected: quarantine is a queue because its rows are
  *undelivered* — they have no todo yet. A held todo is a real todo on a real endpoint that must keep its
  queue, lease semantics, dedup key and tenancy, and moving it between queues would rewrite the endpoint
  routing the whole model rests on.
- **Re-reading dependencies on every claim** (the report's point 5). Rejected: Decision 5.
- **A dependency graph inside Switchboard** (`depends_on` todos, MCP verbs to edit it). Rejected: the
  tracker already has this, humans and agents already edit it, and a copy would drift the first time
  someone edited the real one.
- **Holding on `blocked_by` text or labels.** Rejected: labels and prose are not a relationship, they have
  no cross-repository meaning, and "needs rebase" would silently become a hold.
- **Fail closed when the provider is unreachable.** Rejected: Decision 6; an outage would stop every
  queue the instance serves.

## Consequences

- **Positive:** work that cannot proceed is not handed out; the retry budget is spent on real failures;
  the operator sees *why* (`waiting on stump.wtf/cairn#41`) on the row and in `list_todos`, and the blocked
  pill counts the population. Releasing is automatic and needs no operator.
- **Negative:** the feature is only as good as the tracker data. A team that never declares dependencies
  sees no change — which is the correct silence, not a malfunction, and the reason `list_todos` reports
  blocker refs rather than leaving the state bare. A held todo whose blocker is closed outside the tracker
  (a force-push that merged it, a repo rename) waits for the sweep.
- **Costs:** one indexed provider read per todo born on a repository that has the dependencies API, with a
  30-second per-issue cache and a per-endpoint read timeout of 3s; one sweep pass per interval bounded at
  50 todos. Cross-repo blockers cost no extra reads (the dependency page already carries each blocker's
  repository).
- **Surfaces:** `todos.state` gains `blocked` (SPEC-0003's state machine, SPEC-0018's audit of every
  state-consuming site); `list_todos` gains `issue_ref` and `blockers` and accepts `state: "blocked"`;
  `queue_stats` and `switchboard_queue_todos` gain `blocked`; the Todos view gains a seventh pill and a
  row sub-line. The MCP doorbell never announces a held todo, so no agent prompt changes shape.
- **Requirements implied, not yet written:** in SPEC-0003, that a todo MAY be minted in `blocked` and SHALL
  NOT be returned by any claim path while any recorded blocker is open, and SHALL move to `pending`
  automatically when the last one closes; in SPEC-0013, that a held todo SHALL appear in the operator's
  listings with its blocker refs; in SPEC-0035's provider-read scope, that a dependency read is an allowed
  use of the per-webhook read-only credential.
- **Open, for Joe:** provisioning a live read-only tracker credential per webhook (#615) is still the
  operator's step — until then the intake path fails open and this code is exercised only by tests, which
  is the same shipping posture ADR-0052 and ADR-0053 chose. Whether Decision 5's five-minute bound is
  acceptable, or whether the sweep should also be nudged by any delivery to the blocking repository, is
  the one call worth arguing about.
