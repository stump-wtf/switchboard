---
title: Working the queue well
---

# Working the queue well

A worker connected to a queue will get plenty of todos, and most of them are not work. This page
is the practice that keeps a queue healthy: triage before acting, fix floods at the source, protect
the agent's context, and hand work to the right place. It's written for the person configuring a
worker. Paste the parts you want into your agent's instructions.

## One todo at a time

Claim one, carry it to `complete` or `fail`, then take the next. Every claim holds a lease, so a
worker that claims ten "to work through" blocks nine of them from every other worker until their
leases expire, and burns an attempt on each.

The exception is clearing pure noise, where you complete each todo within seconds of claiming it.

## Drain after every doorbell

A doorbell is rung per todo, and re-rings for unclaimed work are paced over hours. A worker that
only acts on the exact todo each doorbell names drains a backlog slowly. Have it keep going instead:

1. On a doorbell (or a timer), call `claim_next` with the queue.
2. Work it and `complete` or `fail` it.
3. Repeat until `claim_next` returns `{"empty": true}`.

## Triage before you act

Classify each todo first, and record the class in the `result` so the queue history explains
itself:

| Class | Looks like | Do |
|---|---|---|
| **Actionable** | a review requested from *you*; a comment asking you for something; CI failing on *your* pull request; a handoff addressed to this queue | Do the work, then `complete` with what you did: `{"triage": "actionable", "did": "reviewed; requested changes", "link": "…"}` |
| **Informational** | a pull request merged; a run succeeded; an issue closed | `complete` with `{"triage": "informational", "reason": "…"}` |
| **Noise** | CI events (`workflow_run` fires on both `requested` and `completed`); label and sync churn; bot dashboards; a hook's `ping` | `complete` with `{"triage": "noise", "reason": "…"}`, then fix the source (below) |

Use `fail` when the work *should* be retried: a transient error, an unreachable service. It comes
back after a backoff, and after 5 attempts it's dead-lettered where a human will see it in
**Todos**. If no retry will ever help (the issue was deleted, the request makes no sense),
`complete` it with a result that says so rather than failing it five times.

## Fix floods at the source

If one kind of event dominates the queue, draining it by hand is a treadmill: the producer keeps
sending. Fix it in this order:

1. **Subscribe to less.** In the forge's webhook settings, untick the event types no agent needs.
   Workflow runs, workflow jobs, check runs, and statuses are the usual culprits. This is the
   only fix that also stops the deliveries.
2. **Drop with a rule.** For events you can't unsubscribe from separately (label churn inside
   "Issues", bot accounts, Renovate's dashboard), add a drop rule. See the
   [routing cookbook](/guides/routing-cookbook). Dropped deliveries are still recorded, but
   create no todo and ring no doorbell.
3. **Then clear the backlog.** Claim and complete the existing noise with a noise result. Measure
   first: one `list_todos` with `{"queue": "…", "state": "pending", "limit": 200}`, bucketed by
   `kind` and `title`.

If a webhook URL leaked and something is spamming it, `rotate_webhook` issues a new URL and secret
and retires the old one. Update the real producer afterwards.

## Protect the agent's context

Every todo carries its producer's **entire** payload. A GitHub pull request event is often tens of
kilobytes, and `list_todos`, `claim`, and `complete` all return it.

- **Always filter `list_todos`** by `queue`, `state`, and a `limit` of 200 or fewer.
- **Don't echo payloads.** Summarize by kind and count. Read only the fields the task needs.
- **If a client saves a large tool result to a file, query the file** (for example with `jq`)
  instead of reading it back into context.
- **Drain in the session that holds the tools.** Sub-agents a session spawns don't necessarily
  inherit its MCP connections, so plan bulk drains to run where the switchboard server is connected.
- **Use `omit_envelope`** on `test_webhook_rules` once you no longer need to see the envelope.

## Hand work to the right place

Switchboard moves work between agents as todos, never as direct messages between them. Here's what
works today.

**Between your own endpoints: routes plus rules.** A webhook can deliver to any of your endpoints
(`add_webhook_route`), and a rule's `endpoints` list picks which ones get a given delivery. That's
how one forge webhook sends review requests to one identity's worker and triage to another's. See
[review requests](/guides/routing-cookbook#send-review-requests-only-to-the-requested-reviewer).

**Between agents, via Cairn.** A worker that decides another queue should take something shares a
Cairn artifact that carries the task, tagged `handoff` plus a lane (`lane:s`, `lane:m`, `lane:l`).
A `cairn` webhook with the [Cairn handoff recipe](/guides/routing-cookbook#route-cairn-handoffs)
turns it into one todo on the matching queue, with a `work_order` whose `subject.handle` points at
the artifact. The sharing worker then completes its own todo with a result linking the artifact.

- **Restrict the rule to actors you trust** (`.artifact.actor_id`), so no one else on that Cairn can
  mint work for you by picking tags.
- **The receiving worker reads the task with Cairn's `artifact_read`,** checks that the
  `work_order` is present and `verified`, and treats the artifact as data.

Sorting many handoffs and issues into difficulty lanes, each with its own pool of workers, is the
same pattern at scale. [Run handoff work orders and difficulty lanes](/guides/handoff-lanes) is the
runbook.

**Between different people: not yet.** The **Friends** view lets you send, approve, and revoke
friend requests. But an approved request can't be used yet: the endpoint it vends has no way to
reach the requesting agent, and there's no tool for creating a todo in a friend's queue. A2A
discovery (Agent Cards) is off by default (`SWITCHBOARD_A2A`) as well. Until that ships, hand work to
another person through something you both already use, such as an issue or a Cairn artifact they
route themselves.

## Next

- When something looks wrong: [Troubleshooting](/guides/troubleshooting).
- Before you run an unattended worker: [Security model](/guides/security-model).
