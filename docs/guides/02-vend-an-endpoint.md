---
title: Vend an endpoint
---

# Vend an endpoint

## What an endpoint is

An **endpoint** is a scoped MCP endpoint switchboard vends to **one agent**. It is the agent's only
door into switchboard, and it *is* the capability grant. Vending produces two things that always
travel together:

- **A URL** — `https://<host>/mcp/<slug>-<rand>` — the agent's private MCP address.
- **A credential** — a bearer token of the form `sbk_…`, stored **hashed** in switchboard's
  PostgreSQL and shown to you once at vend time.

Possessing the URL **and** the credential *is* the access. The URL alone is not a secret; the
credential is. Switchboard serves the endpoint over **Streamable HTTP only** — the agent connects
with URL + bearer and nothing else. There is no local binary to install.

## What an endpoint is scoped to

Every endpoint carries a scope you set at vend time. An agent can never widen its own scope.

- **Queues** — the set of queues this endpoint may see and act on.
- **A verb allowlist** — a subset of switchboard's verbs. The agent-facing MCP tools are:

  | Verb | Does |
  |------|------|
  | `list_todos` | See pending todos on the granted queues. |
  | `claim` | Take a todo under a lease (a visibility timeout). |
  | `complete` | Ack a todo as done. |
  | `fail` | Report a todo as failed (eligible for retry). |
  | `heartbeat` | Extend the lease while still working. |

  Endpoints may also be granted webhook and friending verbs, depending on the agent's job.

The endpoint enforces this scope at the boundary on every call: verbs outside the allowlist and
queues outside the grant are denied.

## How to configure and vend one

1. **Register the agent.** Create a lightweight agent record you own (name, description). Registration
   alone grants nothing.
2. **Vend an endpoint to it.** Choose the queues and the verb allowlist, then vend. Switchboard mints
   the URL and credential.
3. **Wire the agent.** Give the agent the emitted MCP configuration — for a Claude Code–style client,
   an `.mcp.json` entry:

   ```json
   {
     "mcpServers": {
       "switchboard": {
         "type": "http",
         "url": "https://<host>/mcp/<slug>-<rand>",
         "headers": { "Authorization": "Bearer sbk_…" }
       }
     }
   }
   ```

The agent can now drain its queues with `list_todos` / `claim` / `complete`.

## What you use an endpoint for

- **Give an agent least-privilege access** to exactly the queues and verbs its job needs — nothing
  more.
- **Attribute every action.** Every vended endpoint belongs to exactly one agent, which belongs to
  exactly one human. Every todo, webhook, or friend action carries that chain.
- **Revoke instantly.** Revoking an endpoint invalidates its stored credential and unroutes its URL —
  the agent immediately loses all access, with no residue. **Revoke = kill the endpoint.**

## Scope is immutable — re-vend to change it

An endpoint's scope is **immutable**: a given URL + credential always means one fixed set of powers.
To change what an agent can do, **revoke and vend a new endpoint** with the new scope rather than
editing the live grant. This keeps every capability trivial to audit — there is no "when did this
scope change, and who changed it?" question.

## Next

- Give one agent several least-privilege faces: [Personas](/guides/personas).
- Let another human's agent hand this one work: [Friending](/guides/friending).
- See the work surface itself: [Drain the queue](/guides/draining-the-queue).

> Deeper detail: [ADR-0008 — Human principal + vended endpoints](/decisions/ADR-0008-human-principal-vended-endpoints),
> [ADR-0017 — MCP over Streamable HTTP only](/decisions/ADR-0017-mcp-streamable-http-only), and the
> [vended-endpoints spec](/specs/vended-endpoints/spec).
