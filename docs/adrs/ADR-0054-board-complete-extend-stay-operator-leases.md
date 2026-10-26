---
status: proposed
date: 2026-10-26
decision-makers: Proposed by joestump-agent, pending Joe
extends: [ADR-0022]
related: [ADR-0039]
---

# ADR-0054: Board Complete and Extend Stay on Operator-Taken Leases

**Status note:** this ADR is **proposed**. It answers the open question left by issue #513 (the
SPEC-0034 REQ-6 Board exemption for Release and Fail, landed in the PR that references this ADR);
nothing here is settled until Joe accepts it.

## Context and Problem Statement

On `main`, every Board action passed `owner = "op:"+human.ID` to the `…OperatorOwned` store calls,
which match `owner=$2`. A todo an agent claimed (`owner = agent:<id>`) therefore answered every
Board action with a conflict, and the owning human could not recover a stuck attempt before its
lease lapsed. SPEC-0034 REQ-6 promises that the Board's actions are exempt from the fence, "so a
human can always release or fail a stuck attempt."

The fix (issue #513) makes the Release and Fail handlers read the todo through
`GetTodoOperatorOwned` and act on its current owner, so they reach a lease the human's agent
holds. REQ-6 names only release and fail. The issue left open whether **Complete** and **Extend**
also apply to an agent's lease. This ADR proposes the answer: they do not.

## Decision

Board **Complete** and **Extend** keep acting only on a lease the human took from the Board
(`owner = op:<human_id>`). A lease an agent holds, fenced or not, still answers them with a
conflict. Board **Release** and **Fail** are the only actions that reach an agent-held lease.

## Considered Options

- **Extend the read-the-current-owner shape to Complete and Extend as well** (the same two-line
  change in each handler). Rejected:
  - **Complete asserts the work is done.** The agent holding the lease is the only party that
    knows whether it is; a human completing the agent's attempt from the Board would close it as
    `completed` without the agent's result. REQ-6 promises recovery, not completion by proxy.
  - **Extend is a heartbeat.** Applying it to an agent's lease would let a human keep a crashed
    supervisor's lease alive indefinitely, hiding the stall from the reaper — the opposite of the
    recovery the exemption exists for. A worker that is alive heartbeats itself; one that is not
    should be reaped, not fed.
  - The fence exists to gate `heartbeat`, `complete`, `fail` and `release` away from everyone but
    the attempt's holder. REQ-6's Board exemption narrows that gate for the two actions that
    unstick a todo without asserting its outcome. Widing it to the outcome- and clock-asserting
    actions goes beyond the promise.
- **Leave Complete and Extend as they are (this ADR).** The Board's recovery story stays exactly
  REQ-6's: release the lease back to the queue, or fail the attempt into backoff.

## Consequences

- A Board Complete or Extend on an agent-held lease remains a `409 conflict` until this ADR is
  accepted in a different form or REQ-6 is amended; the guide's fence section documents that.
- If a later requirement needs the Board to complete or extend an agent's lease (for example, an
  operator finishing work an agent abandoned mid-attempt), it lands as a deliberate change to
  this ADR, with the Complete-without-result and Extend-past-crash cases decided on the record.
