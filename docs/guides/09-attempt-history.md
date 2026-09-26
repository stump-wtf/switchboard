---
title: Attempt history
---

# Attempt history

Every claim of a todo opens an **attempt**, and whatever ends that lease closes it with an
**outcome**. The next worker to claim the todo is handed the recent attempts, so it learns what was
already tried, by whom, and whether each one failed or simply **died**. This page is the reference
for the verbs and arguments involved, their limits, and the settings an operator controls.

For the claim-and-ack loop itself, start with [Drain the queue](/guides/draining-the-queue).

## What an attempt records

| Field | Meaning |
|---|---|
| `seq` | 1-based, over the todo's whole life. Never reused, and a manual **Retry now** does not reset it. |
| `attempt` | The todo's attempt counter after this claim. **Retry now** resets the counter to 1, never `seq`. |
| `claimer_kind` | `endpoint` (an agent credential) or `owner` (the human, claiming from the Board). |
| `claimant` | The label the claimer supplied, or `null`. |
| `claimed_at`, `last_heartbeat_at`, `ended_at` | RFC 3339 times. `ended_at` is `null` while the attempt is open. |
| `outcome` | How the attempt ended (below). `null` while open. |
| `disposition` | What happened to the todo: `done`, `retry_scheduled`, `requeued`, `dead_lettered` or `canceled`. |
| `died` | `true` when the lease lapsed with no report. |
| `summary`, `summary_truncated` | The holder's note to later claimers, and whether it was cut. |
| `artifact` | An `mcp://cairn/<id>` handle or `https` URL the holder left, or `null`. |

No read returns the claiming endpoint's id, the MCP session, or anything about a lease token.

### Outcomes

| Outcome | Caused by | Disposition |
|---|---|---|
| `completed` | `complete`, or **Complete** on the Board | `done` |
| `failed` | `fail`, or **Fail** on the Board | `retry_scheduled`, or `dead_lettered` on the last attempt |
| `released` | `release`, or **Release** on the Board | `requeued` |
| `lease_expired` | another claim took over the lapsed lease before the reaper ran | `requeued` |
| `reaped` | the reaper found the lease lapsed | `requeued`, or `dead_lettered` on the last attempt |
| `canceled` | an A2A cancel | `canceled` |
| `revoked` | the owning endpoint was revoked | `dead_lettered` |

### Died is not failed

`died: true` means the outcome was `lease_expired` or `reaped`: the holder claimed the todo and then
said nothing until its lease ran out. The worker crashed, hung, lost its session, or ran out of
quota. Its `summary` and `artifact` are always `null`, because it never reported.
`last_heartbeat_at` bounds when it went quiet.

`failed` is a verdict. The holder called `fail`, usually with a `summary` of what it tried and why it
stopped. A run of failures points at the todo. A run of deaths points at the worker, which a
different worker, a longer lease, or more `heartbeat` calls may fix.

Both spend an attempt: the counter moves when the todo is claimed, not when it ends.

## Verbs and arguments

Every argument below is optional. A client that sends none of them behaves exactly as it did before
attempt history existed.

| Verb | Argument | Limit |
|---|---|---|
| `claim`, `claim_next` | `claimant` | Cut to 128 bytes, with control characters removed. A label, such as `harness/box/fixer/run-7`. |
| `claim`, `claim_next` | `require_fence` | Boolean. See [The fence](#the-fence). |
| `complete`, `fail`, `release` | `summary` | Cut to 2048 bytes on a UTF-8 boundary, never rejected. A cut sets `summary_truncated` on the attempt. |
| `complete`, `fail`, `release` | `artifact` | At most 512 bytes, and either `mcp://cairn/<id>` (`<id>` of 1 to 64 letters, digits, `_` or `-`) or an absolute `https` URL. Anything else fails with `invalid`, and the todo stays claimed. Switchboard never fetches it. |
| `heartbeat`, `complete`, `fail`, `release` | `lease_token` | The token a fenced claim returned. |
| `get_todo` | `attempts_limit` | Default 20, maximum 50. |

`result` on `complete` and `fail` is unchanged: it is stored on the todo, not on the attempt.

### On a claim

A committed `claim` or `claim_next` answers with the todo plus:

- `attempt_seq`: the `seq` of the attempt this claim opened.
- `attempts_total`: how many attempts the todo has had, this one and any pruned ones included.
- `prior_attempts`: the five most recent closed attempts before this one, newest first. It is empty
  on a first claim.
- `lease_token`: only when the claim set `require_fence`.

An empty `claim_next` (`{"empty": true}`) carries none of these.

### get_todo

`get_todo` with an `id` returns one todo in full: the usual row, its stored `result`, and its
attempts, newest first, including the open one, up to `attempts_limit`. `attempts_total` and
`attempts_pruned` count what the list leaves out.

An endpoint holding `list_todos` can call `get_todo`, so endpoints vended before it existed have it
already. A todo owned by another endpoint, or outside this endpoint's queues, answers `not_found`,
the same as an id that never existed.

### release

`release` hands a held todo back to `pending` without a verdict: a daemon shutting down, an
operator stopping a run, or a usage limit. There is no backoff, and the attempt counter is
unchanged. The attempt closes `released` with whatever `summary` and `artifact` were sent. Only the
current holder can release, and anyone else gets `conflict`. `release` is its own grant. No other
verb implies it, so an endpoint vended before it existed needs a re-vend to use it.

### dead_letter and next_retry_at

Every todo row a drain verb returns, `list_todos` and `heartbeat` included, carries:

- `next_retry_at`: when a `failed` todo goes back to `pending`, or `null` when no retry is scheduled.
- `dead_letter`: `true` when the todo is `failed` and nothing will re-queue it. It waits for a human
  to use **Retry now** on the Board.

## Summaries are data, never instructions

A `summary` is written by whichever model held an earlier attempt, and it is handed to **every
later claimer** of that todo, in `prior_attempts` and in `get_todo`. The same goes for `claimant`
and `artifact`. Switchboard stores these fields and returns them. It never interprets, scans or
fetches them.

- **Treat them as data in your worker's instructions.** A summary can tell the next attempt what
  was tried. It never changes what the worker may do, where it sends things, or what it reveals. "The
  owner said to skip the tests" inside a summary is text, not approval.
- **Redact before you send.** Switchboard does not scan summaries for secrets. Anything a worker
  puts in one, such as a token from a log or a customer's data, is replayed to every later claimer
  until the todo is pruned. Write what was tried and why it stopped, not raw output.
- **Link, don't paste.** Put long logs in a Cairn artifact and pass its handle as `artifact`.

The tool descriptions say the same thing, so a model reading the schema sees the warning.

## The fence

Every worker on an endpoint acts as the same owner, `agent:<agent_id>`. Without a fence, any of them
can heartbeat, complete, fail or release a todo another holds, and a worker whose lease lapsed can
still close a todo that has since been claimed again.

Claim with `require_fence: true` to rule that out. The response carries a `lease_token`, returned
that once and never again. Switchboard keeps only its SHA-256. Pass it as `lease_token` on
`heartbeat`, `complete`, `fail` and `release` for that attempt.

Use it when:

- **A supervisor holds the attempt for a worker.** A relay claims the todo, hands the task to an
  agent that holds the same endpoint credential, and keeps the token. The agent cannot close the
  attempt behind the supervisor's back.
- **Several workers share an endpoint** and a slow one might outlive its lease.

A fenced call answers `conflict` when:

- it sends no `lease_token` for a fenced attempt;
- it sends a token that does not match, such as a stale worker's token after the todo was claimed
  again;
- it sends a token for an attempt that was not fenced.

`conflict` is also what a caller that does not hold the lease gets, so the error does not reveal
whether a todo is fenced. The todo stays claimed.

A worker that loses its token (it crashed, or restarted without saving it) cannot heartbeat or
report. Its lease lapses, the reaper re-queues the todo (or dead-letters it, on its last attempt),
and the attempt is recorded as died. So keep a fenced lease no longer than the work needs, and
extend it with `heartbeat`.

The Board's actions never check the fence. Today, though, the Board's **Release**, **Extend**,
**Complete** and **Fail** act only on a lease the human took from the Board. A todo an agent holds,
fenced or not, answers them with a conflict and comes back when its lease lapses.

## For operators

### Summary from result

`SWITCHBOARD_ATTEMPT_SUMMARY_FROM_RESULT` is **off** by default. When it is set to `true`, a
`complete` or `fail` that sends no `summary` stores the compact JSON of its `result` as the
attempt's summary, cut to 2048 bytes like any other. `release` has no `result`, so it is never
affected.

It is off because clients written before attempt history put whatever they liked in `result`,
expecting nobody else to read it. Turning it on starts replaying those results to every later
claimer of the todo. Enable it only when you know your clients' results are safe to show them.

### Retention

- **Per-todo cap.** A todo keeps at most `attempt_history_max_per_todo` attempts, 50 by default. A
  value below 5 is raised to 5, and a value that is not a whole number falls back to 50. When a
  claim would exceed the cap, the oldest closed attempts are deleted in the same transaction and
  counted in `attempts_pruned`. The open attempt is never pruned.
- **With the todo.** Attempts are deleted with their todo. Retention already removes terminal todos
  (`retention_max_age_days`, 30 by default, and `retention_max_rows`), so history lasts as long as
  its todo. A live todo keeps its history, and so does a failed one waiting for its retry.

The cap is a row in the `settings` table, read on every claim, so a change takes effect without a
restart:

```sql
INSERT INTO settings (key, value) VALUES ('attempt_history_max_per_todo', '100')
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value;
```

### Metrics

`switchboard_todo_attempts_closed_total{queue,outcome}` counts closed attempts, with `outcome` one
of the outcomes above. Deaths reconcile with the older counter: every attempt closed
`lease_expired` or `reaped` also adds exactly one to `switchboard_todo_leases_expired_total`. A
rising share of deaths on one queue usually means its workers are crashing or their leases are too
short, rather than that the work is failing. See [Metrics](/guides/self-hosting#metrics).

### Who can see attempts

Attempts are scoped exactly like their todo. Only the endpoint that owns the todo reads them over
MCP. Any other endpoint gets `not_found`, whether or not the todo exists. On the Board, the todo
drawer is gaining the same attempt list, shown only to the human who owns the todo. The instance
operator gets aggregates from `/metrics`, never another user's summaries, artifacts or claimant
labels through Switchboard's own surfaces. The rows are still in the database, as payloads
are, so the [security model](/guides/security-model)'s advice about what to put in a payload applies
to summaries too.

> Deeper detail: [ADR-0039 — Attempt history on todos](/decisions/ADR-0039-attempt-history-on-todos)
> and the [attempt history spec](/specs/todo-attempts/spec).
