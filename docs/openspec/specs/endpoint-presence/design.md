# Design: Endpoint Presence

## Context

A doorbell becomes a model turn ([ADR-0013](../../../adrs/ADR-0013-channels-push-delivery.md)), and
Switchboard has no idea whether an endpoint's agent is at work. The result is re-rings spent on
agents that are gone, doorbells for agents that are winding down, and nothing on their return.
[ADR-0027](../../../adrs/ADR-0027-endpoint-presence-clock-in-clock-out.md) adds per-endpoint presence
(in or out), set by the agent, by its human, or by a shift. It also adds a digest doorbell on return,
and a delivery-accounting fix in the heartbeat sweep. Governing spec: SPEC-0022. Amended: SPEC-0011,
SPEC-0006, SPEC-0007.

## Goals / Non-Goals

### Goals

- A disconnected or clocked-out agent spends no ring budget and receives no pushes.
- A returning agent learns its backlog in one doorbell.
- Every existing endpoint works unchanged and gains the verbs without a re-vend.
- Correct across instances and restarts without in-memory coordination.

### Non-Goals

- Changing what an agent may pull or claim.
- Routing work to whichever endpoint is awake (deferred in ADR-0027).
- Knowing whether an agent is mid-turn.
- Coupling to Harness or any other supervisor.

## Decisions

### Columns on `endpoints`, no new table

Migration `0021_endpoint_presence.sql`:

```sql
ALTER TABLE endpoints
  ADD COLUMN shift               text,
  ADD COLUMN presence_override   text CHECK (presence_override IN ('in','out')),
  ADD COLUMN override_until      timestamptz,
  ADD COLUMN override_set_at     timestamptz,
  ADD COLUMN override_by         text,
  ADD COLUMN presence_seen       text NOT NULL DEFAULT 'in' CHECK (presence_seen IN ('in','out')),
  ADD COLUMN presence_changed_at timestamptz,
  ADD COLUMN presence_changed_by text;
```

`override_set_at` is what "ended by the next boundary" is computed from: an override is live only if
no shift boundary lies between `override_set_at` and now. Presence is one row per endpoint, read
wherever the endpoint is already read, so a separate table would only add a join.

### A pure `internal/presence` package

```go
type Shift struct { /* zone + windows */ }
func ParseShift(s string) (Shift, error)
func (s Shift) In(t time.Time) (in bool, next time.Time, ok bool)

type Inputs struct {
    Shift         *Shift
    Override      string    // "", "in", "out"
    OverrideUntil *time.Time
    OverrideSetAt time.Time
}
// Effective applies SPEC-0022 REQ "Presence Model".
func Effective(in Inputs, now time.Time) (presence string, source string, until *time.Time)
```

Everything that decides presence, including the doorbell filter, the sweep, the verbs, the API and
the evaluator, calls `Effective`, so the precedence rule exists in one function with a table test.
The grammar mirrors Harness's `internal/hours` parser. The shared contract is the grammar string, not
the code: the two projects do not share a module.

### Filtering the doorbell

`Handler` already caches each session's endpoint auth snapshot. It gains a small presence cache,
`map[endpointID]presence.Inputs`, loaded on session creation and refreshed from a
`LISTEN endpoint_presence` notification that every presence write sends. `PublishTodoReady` computes
`presence.Effective(inputs, now)` right after its tenant check and returns early when the result is
`out`. The 5-second cross-instance freshness bound comes from the NOTIFY. The 30-second evaluator
re-reads the cache as a safety net for a missed notification.

### The heartbeat sweep takes the instance's receivers

`RingUnclaimed(ctx)` becomes `RingUnclaimed(ctx, receivers []string)`. `receivers` holds the endpoint
IDs with at least one open-stream session on this instance whose effective presence is `in`, and the
`Handler` builds that list. The SQL adds `AND t.endpoint_id = ANY($8)`. An instance with no receivers
skips the query. Rings are still counted in the same statement that picks them, but now only for an
endpoint that could receive one. A buffer-full drop can still waste a ring, which the existing lossy
contract accepts.

### Transitions: claim in SQL, deliver locally

A presence evaluator runs every 30 seconds on each instance and immediately after any presence write:

```sql
UPDATE endpoints SET presence_seen = $new, presence_changed_at = now(), presence_changed_by = $src
WHERE id = $id AND presence_seen <> $new
RETURNING id;
```

Only the instance whose update returns a row broadcasts `NOTIFY endpoint_presence '<id>:<new>'`. Every
instance that receives an `in` broadcast and hosts an open-stream session for that endpoint sends it
one digest. That means one digest per hosting instance. Sessions of one endpoint are competing
consumers of one agent, so two instances each waking one worker is the same outcome as two todos
arriving.

### The digest

```go
type Digest struct {
    Pending int
    Queues  map[string]int
    Oldest  time.Time
    Reason  string // clock_in, operator, shift_start, override_end, reconnect
}
```

It is built from a `PendingDoorbellTodosSummary(endpointID)` query using the push-eligibility
predicate already in `RingUnclaimed`, grouped by queue. It is encoded as a
`notifications/claude/channel` with `content` as one line, and `meta` = `kind=digest`, `pending`,
`queues` (comma-joined, sorted), `reason`, all snake_case. After a successful write,
`MarkDigestRung(endpointID, ids, at)` sets `last_ringed_at` on exactly the counted rows.

### Reconnect digests

`mcpSession` already tracks `inflight` streams. The first 0→1 transition for an endpoint on an
instance, with no other open-stream session for it on that instance, triggers a digest check through
the existing `doorbellGate` keyed `digest:<endpointID>`, with a 10-minute TTL.

### Self verbs

`internal/mcp/verbs.go` gains `SelfVerbs() []string { return {"clock_in","clock_out","presence"} }`.
`tools/list` appends them after the scope-derived set, and `tools/call` checks
`hasScope(ep.ScopeVerbs, v) || isSelfVerb(v)`. The vend wizard and OAuth consent screen do not list
them, because they are not grantable.

## Architecture

```mermaid
sequenceDiagram
  participant A as agent session (instance 1)
  participant H1 as Handler (instance 1)
  participant DB as Postgres
  participant H2 as Handler (instance 2)

  A->>H1: clock_out
  H1->>DB: UPDATE override=out · NOTIFY endpoint_presence
  DB-->>H2: presence cache refresh
  Note over H1,H2: new todos: PublishTodoReady sees out → no ring; sweep receivers exclude the endpoint

  A->>H1: clock_in
  H1->>DB: UPDATE override cleared
  H1->>DB: UPDATE presence_seen out→in RETURNING id
  DB-->>H1: 1 row (claimed)
  H1->>DB: NOTIFY endpoint_presence 'id:in'
  H1->>DB: PendingDoorbellTodosSummary
  H1->>A: notifications/claude/channel kind=digest pending=7
  H1->>DB: MarkDigestRung(ids, now)
```

## Risks / Trade-offs

- **Exempt verbs.** The allowlist stops being the complete list of callable verbs. Mitigated by
  keeping the exempt set to three verbs with no arguments that could address anything, and by a test
  that fails if any self verb accepts an identifier.
- **Two clocks.** Harness `operating_hours` and the endpoint `shift` can disagree. Mitigated by using
  the same grammar, and by the reconnect digest, which makes a missing shift degrade to "it works, a
  little less efficiently."
- **Missed NOTIFY.** An instance could keep a stale presence for up to 30 seconds. The cost is at
  most one extra ring or one late digest.
- **Consumers assuming `todo_id`.** Agents told by the server instructions to claim `meta.todo_id`
  must learn the digest. The `instructions` text on `initialize` changes to describe both doorbells.

## Migration Plan

Additive columns with defaults that preserve today's behavior (`presence_seen = 'in'`, no shift, no
override). The sweep signature change is internal. Roll back by dropping the columns after reverting
the code. No data depends on them.

## Open Questions

- Should the digest name the oldest todo's id, so an agent that wants FIFO can claim it directly, or
  stay count-only so it never carries anything todo-derived?
- Should the 8-hour cap on an agent's `clock_in` be configurable per endpoint by its human?
