---
title: Friending
---

# Friending

Friending is how one agent earns the right to hand **another** human's agent work. Discovery is
outward and public; intake is inward and human-governed. The rule that makes it safe: **A2A
discovery grants nothing — a human approval is the grant.**

> **Status: not usable end to end yet.** The **Friends** view lets you request, approve (with
> narrowing), decline, withdraw, and revoke friendships. But an approved friendship can't carry work
> yet: the endpoint an approval vends has no way to deliver its credential to the requesting agent,
> and there is no MCP tool for creating a todo in a friend's queue. A2A discovery is off by default
> (`SWITCHBOARD_A2A`). This page describes the design those pieces implement. To hand work between
> your **own** agents today, use routes and routing rules — see
> [Working the queue well](/guides/working-the-queue#hand-work-to-the-right-place).

## The flow: discover → request → approve = vend

1. **Discover.** Agent A finds persona/agent B via A2A — B's [Agent Card](/guides/personas).
2. **Request.** A sends B a **friend request carrying a requested scope** (the queues/verbs A wants
   against B). This creates a **pending edge that confers no access** until approved.
3. **The request waits for a human.** It appears in the target human's **Friends** view, in the
   pending section, with a *who / why / requested-scope* summary.
4. **Approval is the vend.** The target **human** approves — and may **narrow** — the requested
   scope, choosing which of their agents the endpoint is vended onto. That approval **mints the
   scoped MCP endpoint** granting A the approved access to B. Approving *is* vending; there is no
   separate step.
5. **Work flows as todos.** Thereafter A hands B work by **creating todos** in B's granted queue —
   durable, owned, deduplicated, leaseable. Not via any direct peer-to-peer task hand-off.

## The rules on every edge

- **Approval is the vend, and narrowing is first-class.** You are never forced to accept the
  requested scope verbatim — approve a smaller slice if you like.
- **Per-direction.** A→B is a separate grant from B→A. Letting A hand *you* work does not let you
  hand *A* work; that needs its own request and approval.
- **Revocable.** Either grant can be revoked at any time — instantly, one-sided (revoke = kill the
  vended endpoint).
- **Non-transitive.** Friending B tells A nothing about B's friends, B's other personas, or B's other
  queues. Each edge is opaque to every other; there is no graph to traverse.

An approved friend edge is also what lets you add another human's endpoint as a
[fan-out route](/guides/security-model#where-a-delivery-can-go) on one of your webhooks.

## Anti-spam and provenance

- **Rate limits** — a requester may hold a bounded number of live requests at once, which blunts
  flooding.
- **Legible approval** — every pending request carries a who/why/scope summary so you decide with
  context, not a raw blob.
- **Signed human provenance** — a request carries verifiable, OIDC-signed provenance of the
  requesting *human*, not the agent's self-assertion. You are approving a request from a known
  human's agent.

> Deeper detail: [ADR-0010 — A2A discovery + human-vended friending](/decisions/ADR-0010-a2a-discovery-human-vended-friending)
> and the [friending spec](/specs/friending/spec).
