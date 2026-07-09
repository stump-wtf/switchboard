---
status: accepted
date: 2026-07-05
decision-makers: Joe Stump
related: [ADR-0007, ADR-0009, ADR-0010, ADR-0011, ADR-0012]
---

# ADR-0008: Human Principal + Per-Agent Vended MCP Endpoints

## Context and Problem Statement

Once switchboard's core object is a durable todo ([ADR-0007](ADR-0007-todos-as-core-primitive.md)) that agents claim and complete, we must answer *who* an agent is, *who is accountable* for what it does, and *how* it is granted access to a scoped slice of switchboard. Agents are numerous, disposable, and untrusted; humans are few, durable, and accountable. Modeling a bot as a first-class account in the identity provider produces orphaned bot identities, IdP bloat, and murky accountability when a bot misbehaves.

This ADR decides the identity and access-granting model for the agent layer: **who the principal is, how agents are represented, and what the unit of access is.** It also decides where the credentials switchboard mints are stored.

## Decision Drivers

* **Every action traces to an accountable human.** When an agent claims/completes/creates a todo or manages a webhook, there must be a single human owner behind it — no orphan bot actions.
* **Bots do not belong in the IdP.** The identity provider should hold **humans only**. Minting a passkey/OIDC identity per bot bloats the directory and creates long-lived credentials no human is watching.
* **Access is a capability, not a role lookup.** An agent should get exactly the slice of switchboard its owner granted — specific queues, specific verbs — and nothing else, enforced at the boundary.
* **Revocation must be instant and total.** Killing an agent's access should be one operation with no residue: no orphaned identity, no lingering token elsewhere.
* **Credentials are short-lived and revocable.** Agent credentials must be rotatable/expirable independently of the human, stored **hashed** in PostgreSQL ([ADR-0002](ADR-0002-postgres-persistence-and-retention.md)), never committed.
* **Simple to reason about beats maximally flexible.** The access model should be legible enough that a human can look at a grant and know exactly what it permits.

## Considered Options

* **(A) Agents as accounts / OIDC identities** — each agent is a principal in Pocket ID (its own login/credential), authorizes like a user.
* **(B) One shared service token** — agents share a single switchboard credential, distinguished (if at all) by convention.
* **(C) Human principal + per-agent vended MCP endpoints** — humans authenticate via OIDC and are the accountable tenants; a human **registers** agents; switchboard **vends** each agent a scoped MCP endpoint (URL + credential) that *is* the capability grant; agent credentials are switchboard-minted, stored **hashed** in PostgreSQL, short-lived and revocable. *(chosen)*

## Decision Outcome

Chosen option: **"(C) human principal + per-agent vended endpoints."**

### The model

1. **Humans are the principals/tenants.** A human authenticates to switchboard via **OIDC against Pocket ID** ([ADR-0011](ADR-0011-identity-assurance-oidc-passkey-deferred.md)). The human is the accountable owner and the **policy authority** — they decide which queues and verbs any of their agents may touch.
2. **Humans register agents.** An agent is a lightweight record owned by a human (name, description, optional base-runtime metadata). Registration alone grants nothing.
3. **Switchboard vends a scoped MCP endpoint per agent.** Vending produces **(a) an endpoint URL** and **(b) a credential**. Together they *are* the capability grant — possessing the vended endpoint is the access. The endpoint is scoped: a set of allowed **queues** and a **verb allowlist** (a subset of the todo/webhook/friending verbs, [agent-mcp-tools spec](../openspec/specs/agent-tools/spec.md)).
4. **The human is the policy authority.** Scope is set by the human at vend time. The agent cannot widen its own scope; it can only exercise verbs within the allowlist and touch queues within its grant.

### Identity split

| Holds | Where | Lifetime |
|-------|-------|----------|
| **Humans** (principals) | Pocket ID (OIDC) | durable, person-managed |
| **Agent endpoint credentials** | **switchboard-minted**, stored **hashed in PostgreSQL** ([ADR-0002](ADR-0002-postgres-persistence-and-retention.md)) | short-lived, revocable |

The IdP **never holds bots.** Agent credentials are tokens switchboard mints and stores **hashed** in its own PostgreSQL, so they can be rotated or revoked without touching the human's identity and without polluting the directory.

### Properties this buys

* **Traceability:** every vended endpoint belongs to exactly one agent, which belongs to exactly one human. Every todo/webhook/friend action carries that chain.
* **Revocation = kill the vended endpoint.** Revoking an endpoint invalidates its stored credential and unroutes its URL. The agent instantly loses all access; nothing lingers in the IdP because the bot was never there.
* **Least privilege by construction:** an endpoint can only do what its verb allowlist and queue grant permit.

### Proposed default — immutable + revocable scope (decision to confirm)

**Proposed:** a vended endpoint's scope is **immutable**. To change what an agent can do, you **revoke and re-vend** a new endpoint with the new scope, rather than mutating the existing grant in place. Rationale: an immutable capability is far simpler to reason about and audit — a given URL+credential always means one fixed set of powers, forever; there is no "when did this scope change and who changed it" question. Revocation remains always available.

This is marked **decision-to-confirm** with Joe: the alternative is mutable scope (edit a live endpoint's queues/verbs). Mutable is more convenient for incremental permission tweaks but reintroduces the audit ambiguity immutability removes. Recorded as an open question in [docs/README.md](../README.md) and the [accounts-and-endpoints spec](../openspec/specs/vended-endpoints/spec.md).

### Consequences

* Good, because every agent action is attributable to a human owner — clean accountability.
* Good, because the IdP stays humans-only; no orphan bot identities, no directory bloat.
* Good, because the vended endpoint is a self-contained capability: scope travels with the grant, enforced at the boundary.
* Good, because revocation is one instant operation with no residue.
* Good, because agent credentials live (hashed) in switchboard's own PostgreSQL — no external secret-manager dependency to run.
* Bad, because switchboard now runs its own credential-minting/storage path (a mini authorization server) it must build and secure — accepted as the cost of not putting bots in the IdP.
* Bad (under the proposed immutable default), because a scope change means re-vending and re-configuring the agent with a new URL/credential — more churn than editing in place; mitigated because scope changes should be rare and the re-vend is a single call.

### Confirmation

* The [accounts-and-endpoints spec](../openspec/specs/vended-endpoints/spec.md) defines the human-account, agent-registration, and vend/revoke flows and the scope shape (queues + verb allowlist).
* A test asserts a vended endpoint can exercise only its allowlisted verbs on its granted queues, and is denied outside them.
* A test asserts revoking an endpoint invalidates its stored credential and unroutes its URL (subsequent calls fail).
* A test asserts no agent identity is ever created in Pocket ID.
* The immutable-vs-mutable scope default is confirmed with Joe before the code session implements vend.

## Pros and Cons of the Options

### (A) Agents as accounts / OIDC identities (rejected)

* Good, because it reuses the IdP's authN and token machinery.
* Bad, because it creates long-lived orphan bot identities no human is accountable for.
* Bad, because it bloats Pocket ID with non-human principals and muddies "who did this."
* Bad, because Pocket ID is passkey-only and humans-oriented; bots do not fit its model ([ADR-0011](ADR-0011-identity-assurance-oidc-passkey-deferred.md)).

### (B) One shared service token (rejected)

* Good, because trivial to implement.
* Bad, because no per-agent scoping, no per-agent revocation, and no traceability — one leak compromises everything.

### (C) Human principal + per-agent vended endpoints (chosen)

* Good, because accountability, least privilege, clean revocation, and a humans-only IdP all hold at once.
* Bad, because switchboard must operate its own credential mint/store — accepted.

## Architecture Diagram

```mermaid
flowchart TB
  human[Human principal<br/>OIDC via Pocket ID]
  subgraph sb[switchboard]
    reg[Agent registry]
    vend[Endpoint vending<br/>+ policy authority]
    db[(PostgreSQL<br/>credentials, hashed)]
    ep1[[Vended endpoint A<br/>URL + credential<br/>scope: queues+verbs]]
    ep2[[Vended endpoint B<br/>URL + credential<br/>scope: queues+verbs]]
  end
  human -->|registers agents,<br/>sets scope| reg
  reg --> vend
  vend -->|mints + stores credential (hashed)| db
  vend --> ep1
  vend --> ep2
  ep1 -->|scoped MCP calls| sb
  ep2 -->|scoped MCP calls| sb
  human -. revoke = kill endpoint .-> vend
```

## More Information

* Personas subdivide a single agent's vended tools into narrower capability sets: [ADR-0009](ADR-0009-personas-as-scoped-agent-cards.md).
* Cross-agent access is granted by human approval of a friend request, which *is* a vend: [ADR-0010](ADR-0010-a2a-discovery-human-vended-friending.md).
* Identity/assurance posture (why Pocket ID, what is deferred): [ADR-0011](ADR-0011-identity-assurance-oidc-passkey-deferred.md).
* Webhook self-management within a vended ceiling: [ADR-0012](ADR-0012-agents-self-manage-webhooks.md).
* Where minted credentials are stored (hashed): [ADR-0002](ADR-0002-postgres-persistence-and-retention.md).
