---
title: Overview
---

# Switchboard, in one page

Switchboard is the operator's board for inbound events. Many lines come in — webhooks from
GitHub, Stripe, Slack, Docker Hub; messages off a Redis queue — and switchboard does three things
with each one:

1. **Receive** it on a scoped line (a *provider*).
2. **Verify** the caller at the boundary and stamp the event with a *trust mode*.
3. **Patch it through** into a durable *todo* that an agent claims under a lease and completes.

The name is the architecture. A manual telephone exchange took many incoming lines, an operator
verified the caller, and patched the line through. Switchboard is that exchange for your agents.

## The objects you'll work with

You configure and operate switchboard through six kinds of object. Each has its own guide.

| Object | What it is | Guide |
|--------|------------|-------|
| **Provider** | An inbound line — a webhook receiver or a pulled queue — with an enforced trust mode. | [Connect a provider](/guides/connect-a-provider) |
| **Todo** | A durable unit of work derived from an event. Claimed under a lease, completed with an ack. | [Drain the queue](/guides/draining-the-queue) |
| **Endpoint** | A per-agent, scoped MCP endpoint (a URL + credential) that an agent uses to reach switchboard's verbs. | [Vend an endpoint](/guides/vend-an-endpoint) |
| **Persona** | A named, least-privilege face of one agent, published as an A2A Agent Card. | [Personas](/guides/personas) |
| **Friend** | A human-approved grant that lets one agent hand another agent work as durable todos. | [Friending](/guides/friending) |

## The core loop

```
provider ──receive──▶ verify ──patch through──▶ todo ──claim(lease)──▶ agent ──complete──▶ done
   │                    │                          │                                        │
 GitHub/Stripe/     signed / token /          durable, deduped,                     acked; retained
 Slack/Docker/      open / queue              at-least-once                          as an audit record
 Redis
```

Everything downstream of "verify" is the same regardless of which family the event came from: it
normalizes to a common shape, persists, broadcasts over Server-Sent Events to the web UI, and
becomes a todo the agent surface can drain.

## Who does what

- **You (the human) are the accountable principal.** You connect providers, register agents, and
  decide — at vend time — exactly which queues and verbs each agent may touch. Nothing an agent does
  is un-attributable to you.
- **Agents are least-privilege workers.** An agent reaches switchboard only through an endpoint you
  vended it, and can only exercise the verbs and queues that endpoint grants.

## Where to go next

- New here? Start with [Getting started](/getting-started/concepts): the concepts in five minutes,
  then your first endpoint, a connected agent, and a real webhook turning into a todo.
- Routing, triage, security, and fixes: the [routing cookbook](/guides/routing-cookbook),
  [working the queue well](/guides/working-the-queue), the [security model](/guides/security-model),
  and [troubleshooting](/guides/troubleshooting).
- Want the HTTP contract? The [API reference](/api) renders every endpoint from the OpenAPI spec.
- Want the *why*? The [Decisions (ADRs)](/decisions) and [Specifications](/specs) are the canonical,
  governed design record this whole system is built from.
