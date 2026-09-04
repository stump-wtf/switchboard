---
title: Drain the queue
---

# Drain the queue

A **todo** is switchboard's unit of work — a durable work-item derived from an event. Todos are how
agents actually do things: an agent claims a todo under a lease, does the work, and acks it. Nothing
is "read once and lost."

## The lifecycle

```
                claim (lease)          complete
   ┌─────────┐ ───────────────▶ ┌─────────┐ ─────────▶ ┌──────┐
   │ pending │                  │ claimed │            │ done │
   └─────────┘ ◀─────────────── └─────────┘ ─────────▶ ┌──────┐
        ▲       lease expiry /        │       fail      │failed│
        │       release (crash-safe)  │                 └──────┘
        └──────────────── retry ◀─────┴──────── (attempt++, back to pending)
```

- **pending** — created and unclaimed; visible to any consumer whose scope covers its queue.
- **claimed** — a consumer holds a **lease** (a visibility timeout: owner + expiry). The todo goes
  invisible to every other consumer for the window. If the window lapses before completion — the
  worker crashed, hung, or dropped off — the todo **pops back to pending** and is re-claimable. This
  is the crash-safety guarantee (the same model as SQS visibility timeouts).
- **done** — the consumer acked completion. Terminal; retained as an audit record.
- **failed** — the consumer reported failure, or retries were exhausted (a dead-letter state).
- **retry** — a failed or expired todo re-enters **pending** with an incremented attempt count, up
  to a max.

## How an agent drains

Over its [vended endpoint](/guides/vend-an-endpoint), an agent runs a simple loop:

1. `list_todos` — see what's pending on its granted queues.
2. `claim` — take one under a lease. Claiming is atomic: no two consumers get the same todo at once.

   Or, when several workers share the endpoint, **`claim_next`** — no id, no listing: it scans the
   granted queues oldest-first and atomically hands back one available todo. Concurrent callers each
   receive a *different* one, so a pool needs no coordination. An empty queue answers `empty: true`
   rather than an error, because a worker polling and finding nothing is the steady state.
3. Do the work. If it's slow, `heartbeat` to extend the lease so it doesn't lapse mid-flight.
4. `complete` on success, or `fail` on error.

Because a crash between claim and complete leaves the todo re-claimable once the lease lapses,
delivery is **at-least-once** — so **handlers must be idempotent**.

## Two properties worth knowing

- **Idempotency keys collapse duplicates.** Every producer supplies (or switchboard derives) an
  idempotency key — for webhooks, the provider's delivery id (`X-GitHub-Delivery`, Stripe event `id`,
  …). Creating a todo with a key that already exists in a non-terminal state is a no-op that returns
  the existing todo, so at-least-once webhook deliveries fold into a single work-item.
- **Assignee vs. pool queues.** A todo either targets a specific **assignee** (point-to-point) or a
  **pool/topic queue** that many consumers drain competitively (work-sharing).

## Push: a doorbell, not the ledger

### Running several workers on one endpoint

An endpoint is vended to one agent, so several sessions on it are that agent running as competing
consumers. Point each instance at the same endpoint URL and credential; each calls `claim_next` and
gets distinct work. Nothing else needs configuring — the store's scan holds `FOR UPDATE SKIP LOCKED`,
so the pool cannot double-claim.

This is load-sharing, and it is not the same as fan-out. Fan-out (`add_webhook_route`) delivers one
event to *several endpoints* as several todos, so every one of them acts — that is what you want for
different agents with different jobs. Competing consumers share *one* todo, so exactly one acts —
that is what you want for capacity. Routing a webhook to two endpoints owned by the same agent
duplicates work rather than sharing it.

The doorbell rings one worker per todo rather than the whole pool, rotating between them, and
prefers a session with an open notification stream. That keeps a pool from spending N model turns to
do one todo's work.

Pulling with `list_todos` is always correct on its own. Where a harness supports it, switchboard also
**pushes** a notification the moment a todo is ready for a channel-attached consumer — over the same
vended MCP endpoint, via the Claude Code **Channels** standard. Push is lossy by design: if no
session is attached, the todo simply stays `pending` and the worker drains it on return. **The
durable queue is always the ledger; push is just the doorbell**, so an offline agent loses nothing.

## Watch it live

The operator board's **Todos** view shows the queue in real time over Server-Sent Events — claims,
completions, retries, and the reaper returning lapsed leases — wearing the same trust badges the API
and MCP surfaces carry.

> Deeper detail: [ADR-0007 — Todos as the core primitive](/decisions/ADR-0007-todos-as-core-primitive),
> [ADR-0013 — Channels push delivery](/decisions/ADR-0013-channels-push-delivery), and the
> [todo-queue spec](/specs/todo-queue/spec).
