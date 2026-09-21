---
title: Troubleshooting
---

# Troubleshooting

Symptoms first, then the checks that separate their causes.

## The queue looks backed up

Todos pile up as `pending` in **Todos**, and nothing claims them.

1. **Is a worker connected?** Each card on **Endpoints** shows when the endpoint was last used
   (`seen 5m ago`, or `never seen`). `never seen` means no client has ever connected with that
   credential.
2. **Did the worker opt in to doorbells?** Push only reaches a session that loaded switchboard as a
   channel: Crush with `channel_enabled` or `--channels server:switchboard`, Claude Code with
   `--dangerously-load-development-channels server:switchboard`. A session without it still holds
   the connection, so switchboard logs `mcp doorbell delivered` for every todo while the client
   discards each one. **A delivered doorbell is not evidence the agent heard it.** Poll, or enable
   the channel. See [Connect an agent](/getting-started/connect-an-agent).
3. **Can the worker actually act?** The doorbell is delivered to the session even when the session
   can't use it: its model login expired, it's waiting on a permission prompt, or its model quota
   ran out. Attach to it (`harness attach <name>`) and look. This is the most common cause of
   "doorbells delivered, nothing claimed".
4. **Is something else taking the doorbells?** A doorbell rings one session per todo. If another
   session has the same server channel-enabled, such as a chat agent nobody is watching, it can be
   the one that wins. Give push to exactly one kind of session
   ([one channel consumer per server](/getting-started/connect-an-agent#one-channel-consumer-per-server)).
5. **Does the worker only handle the todo it was rung for?** Re-rings for unclaimed work are paced
   over hours. A worker should keep calling `claim_next` until the queue is empty
   ([drain after every doorbell](/guides/working-the-queue#drain-after-every-doorbell)).
6. **Is the todo on a queue the worker can see?** A routing rule can create a todo on a queue that
   is in the webhook's grant but not in the receiving endpoint's scoped queues. The worker can't
   list it (`list_todos` with that queue answers `forbidden`), and no doorbell rings. Route only to
   queues every target endpoint was scoped to, or vend the worker a new endpoint that includes the
   queue.

Also look at what's *not* pending. Many `claimed` todos with no progress means a worker is claiming
and then dying; they come back when their leases expire. Many `failed` todos show `↻ retry` timers or
dead-letter notes.

## A delivery is refused

| Response | Cause | Check |
|---|---|---|
| `401` `signature verification failed` | The producer's secret doesn't match. | Paste the `signing_secret` again. If the webhook was rotated, the producer still has the old secret. |
| `401` on a signed source you wrote yourself | Header format. | GitHub-style needs `X-Hub-Signature-256: sha256=<hex>`. The prefix is required. Gitea accepts a bare hex `X-Gitea-Signature`. Sign the exact bytes you send; a client that re-serializes JSON after signing breaks the signature. |
| `401` from Cairn | Clock or header mismatch. | Cairn deliveries must have a `created_at` within 5 minutes of switchboard's clock, and `X-Cairn-Event-Id` must equal the body's `event_id`. |
| `404` `unknown webhook` | Wrong URL. | `rotate_webhook` retires the old URL. Get the current one from `list_webhooks`. |
| `503` `webhook not configured` | The owning endpoint is revoked or expired, and no live route remains. | Vend a new endpoint and create a new webhook. |
| `413` | Body over 5 MiB. | |
| `429` | Rate limited. | Honor `Retry-After`. |

A forge's own delivery log (GitHub's **Recent Deliveries**, Gitea's webhook history) shows the
response body, which names the failure.

## A rule never matches, or everything takes the default

Look at the `routing` trace on a todo, or on the event (`list_webhook_events`), before changing
anything.

- **`stage: default`, `cause: no_match_default`, with no `faults`.** The expression evaluated and
  returned false. Dry-run it against the real event with `test_webhook_rules` and `event_id`, and
  compare your paths to the returned `envelope`. The usual misses:
  - **`.kind` is `null` in a dry-run.** `.kind` comes from the `X-GitHub-Event` / `X-Gitea-Event`
    header, so pass `headers` along with a hand-written `payload`, or use `event_id`.
  - **Wrong path.** Forge fields live under `.payload`: `.payload.issue.user.login`, not
    `.issue.user.login`. Header names are lower-case: `.headers["x-github-event"]`.
  - **Gitea vs GitHub actions differ.** A label change is `label_updated` on Gitea and `labeled` on
    GitHub.
  - **An earlier rule matched.** First match wins; check `rule_id` on the trace.
- **`faults` is present.** The rule errored (`error`), ran too long (`timeout`,
  `budget_exhausted`), or no longer compiles (`compile_error`). A faulting rule counts as no match.
  A jq error usually means indexing into a missing value; use `//` defaults (`.payload.issue.title
  // ""`) and `[]?` for lists.
- **`cause: rule_not_granted`.** The rule matched, but its queue or one of its `endpoints` is no
  longer reachable: the route was removed or the target revoked. The default applied, and later
  rules were skipped.
- **`cause: default_not_granted`.** The default itself is unreachable, so the delivery went to the
  webhook's target queue on every target.
- **A dry-run fails with `invalid_rule`.** Give every candidate rule an `id`. A save fills missing
  ids in; a dry-run doesn't.
- **The save fails with `forbidden`.** The queue isn't one of the endpoint's webhook queues, or an
  `endpoints` entry isn't a delivery target yet (`add_webhook_route` first). An `exclusive` rule
  also fails to save when no delivery target is scoped to its queue.
- **Everything drops after a rules change.** `set_webhook_rules` replaces rules, default, and
  `params` together, and leaving `params` out clears them. An allowlist written to fail closed
  (as the [cookbook's](/guides/routing-cookbook#only-act-on-trusted-people) are) then trusts no
  one, and so does a list saved as a string by mistake. `list_webhook_rules` shows the params in
  force.
- **The delivery answers `{"repeat": true}` and no todo appears.** A `once` rule already created a
  todo about the same issue or artifact for that queue. That is the point of `once`. Re-sizing to
  a different queue routes again.

## A todo keeps coming back

- **The lease expired.** Leases default to 300 seconds. Long work needs `heartbeat` (with
  `lease_ttl_seconds`) before the lease runs out, or a longer lease at `claim`.
- **Every claim spends an attempt.** An attempt counts when the todo is claimed, not when it fails.
  A worker that claims and then crashes, five times, dead-letters the todo without ever calling
  `fail`.
- **It was dead-lettered.** After 5 attempts, a `failed` todo stops retrying. Fix the cause, then use
  **Retry now** on the todo in **Todos** to re-queue it with a fresh budget.
- **A redelivery made a new one.** Once a todo is `done`, the same delivery id arriving again creates
  a new todo. Forge "redeliver" buttons do exactly that.

## `failed` does not mean finished

`failed` is two different situations wearing one label:

- **Still retrying.** Attempts remain, so switchboard re-queues the todo after a backoff that starts
  at 30 seconds and doubles to a 15-minute cap. It will be worked again on its own.
- **Dead-lettered.** The attempt budget is spent. Nothing re-queues it, and it waits for a human.

`list_todos` returns no field that says which one a todo is in — there is no "next retry" time in its
output — so a queue full of `failed` todos looks the same whether it is busy recovering or has
quietly stopped. Tell them apart this way:

- **Compare `attempt` with `max_attempts`,** both of which every todo carries. `attempt` below
  `max_attempts` means a retry is still coming; `attempt` equal to it means dead-lettered.
- **Open the Todos view** for the definitive answer: a retrying todo shows a `↻ retry` countdown, and
  a dead-lettered one says it was dead-lettered after N/M attempts and offers **Retry now**.
- **Watch whether the count moves.** Retrying todos leave `failed` on their own within 15 minutes.
  A `failed` count that never changes is dead letters, not work in progress.

Two things follow from this for anyone writing a worker or an alert. Don't treat `failed` as a
terminal state, and don't count it as a queue-health signal on its own: the number that matters is
how many todos are at their attempt ceiling.

## Tool errors

| Error | Meaning |
|---|---|
| `forbidden: queue … not in this endpoint's scope` | The endpoint wasn't vended that queue. |
| a tool is missing from the client's tool list | The endpoint wasn't granted it. Scope can't be widened; vend a new endpoint. |
| `not_found` on `claim` or `complete` | The id is wrong, or the todo belongs to a different endpoint. |
| `ceiling_exceeded` on `create_webhook` | The endpoint has used its webhook allowance. `list_webhooks` shows `ceiling.max` and `ceiling.used`. |
| `forbidden_source_type` | The endpoint wasn't allowed that source type. CLI-vended endpoints allow only `generic`; use the web wizard for GitHub, Gitea, or Cairn. |
| `list_todos` returns 50 when you asked for more | Ask for 200 or fewer. A larger limit is currently treated as the default. |
| `401` from the MCP URL | The credential is wrong, the endpoint was revoked, or its lifetime ran out. |

## OAuth sign-in goes to the board and stops

The client opened switchboard's consent screen while you were signed out. Sign-in then lands on
the board instead of returning to the consent screen. Sign in to the web board first, then start
the client's connection again.
