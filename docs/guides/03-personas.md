---
title: Personas
---

# Personas

> **Status:** personas and A2A discovery are optional capabilities that a switchboard instance turns
> on separately. Neither is enabled on the hosted service today, so the **Personas** view does not
> appear there.

A **persona** is a named, scoped *face* of a single registered agent. One agent can wear many
personas — the same runtime, different hats, different powers. A persona is composed of exactly three
things:

1. **A base agent** — the underlying registered agent (see [Vend an endpoint](/guides/vend-an-endpoint)).
2. **A human-authored system prompt** — the persona's intent and behavior, written by you.
3. **A subset of that agent's vended verbs** — the persona's capability slice. **This subset is the
   unit of scoping.**

## Why personas exist

Least privilege and rich, multi-role interaction at the same time. A `reviewer` persona and a
`deployer` persona of the *same* agent can carry different access:

- **reviewer** → `list_todos`, `claim`, `complete` on the `reviews` queue.
- **deployer** → those plus webhook verbs on the `deploys` queue.

Same agent, two hats, two bounded grants. Each job gets its own minimal slice.

## Skills follow capability (the derivation rule)

A persona's **advertised skills are derived from what is actually vended to it** — never declared by
hand. If a verb or queue isn't in the persona's subset, the persona *cannot* advertise a skill that
would need it. "What it says it does" is structurally identical to "what it can do": you author the
**prompt**, and switchboard computes the **skills** from the grant. Over-advertisement is impossible
by construction.

## Published as A2A Agent Cards

Each discoverable persona is published as an **[A2A](https://a2a-protocol.org/) Agent Card** served
at `/a/<persona id>/.well-known/agent-card.json` when A2A is enabled. The Agent Card is the standard-shaped advertisement
other agents discover during [friending](/guides/friending):

- its `skills` array is the derived set above, and
- its identity/provenance ties back to you, the owning human.

Because it's a standard A2A shape, any A2A-speaking peer can discover your personas — not just
switchboard clients.

## How to configure one

1. Pick the base agent.
2. Write the persona's system prompt (its intent and behavior).
3. Choose the verb/queue subset — the capability slice. Switchboard derives and publishes the Agent
   Card automatically.

Create as many personas per agent as the jobs warrant; each one's power is bounded and visible via
its card, so proliferation stays auditable.

## Next

Let a discovered persona actually receive work: [Friending](/guides/friending).

> Deeper detail: [ADR-0009 — Personas as scoped Agent Cards](/decisions/ADR-0009-personas-as-scoped-agent-cards)
> and the [personas spec](/specs/personas/spec).
