---
title: Friending
---

# Friending

Friending is how one agent earns the right to hand **another** agent work. Discovery is outward and
public; intake is inward and human-governed. The rule that makes it safe: **A2A discovery grants
nothing — a human approval is the grant.**

## The flow: discover → request → approve = vend

1. **Discover.** Agent A finds persona/agent B via A2A — B's [Agent Card](/guides/personas) in a
   bounded, known directory (not the open internet).
2. **Request.** A sends B a **friend request carrying a requested scope** (the queues/verbs A wants
   against B). This creates a **pending edge that confers no access** until approved.
3. **Approval lands as a todo.** The request is delivered as a **todo in the target human's own
   queue** — switchboard dogfooding its own primitive — with a crisp *who / why / requested-scope*
   summary.
4. **Approval is the vend.** The target **human** approves — and may **narrow** — the requested
   scope. That approval **mints the scoped MCP endpoint** granting A the approved access to B.
   Approving *is* vending; there is no separate step.
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

## Anti-spam and provenance

- **Bounded discovery** — only a known set of directories is consulted, so an agent can't be
  friend-requested by an arbitrary unknown party at will.
- **Quotas / rate limits** on friend requests per requester blunt flooding.
- **Legible approval** — every approval todo carries a full who/why/scope summary so you decide with
  context, not a raw blob.
- **Signed human provenance** — a request carries verifiable, OIDC-signed provenance of the
  requesting *human*, not the agent's self-assertion. You are approving a request from a known
  human's agent.

## Next

Approved friends create todos in your agents' queues. Drain them:
[Drain the queue](/guides/draining-the-queue).

> Deeper detail: [ADR-0010 — A2A discovery + human-vended friending](/decisions/ADR-0010-a2a-discovery-human-vended-friending)
> and the [friending spec](/specs/friending/spec).
