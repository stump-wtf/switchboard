---
title: Vend an endpoint
---

# Vend an endpoint

> New to switchboard? [Sign in and vend your first endpoint](/getting-started/first-endpoint) walks
> through the web wizard and the CLI step by step. This page is the reference for what an endpoint
> is and what it can be granted.

## What an endpoint is

An **endpoint** is a scoped MCP endpoint switchboard vends to **one agent**. It is the agent's only
door into switchboard, and it *is* the capability grant. Vending produces two things that always
travel together:

- **A URL** — `https://<host>/mcp/<slug>` — the agent's private MCP address.
- **A credential** — a bearer token of the form `sbk_…`, stored **hashed** and shown to you once at
  vend time.

Possessing the URL **and** the credential *is* the access. The URL alone is not a secret; the
credential is. Switchboard serves the endpoint over **Streamable HTTP only** — the agent connects
with URL + bearer and nothing else. There is no local binary to install.

An MCP client that supports OAuth can connect with the URL alone instead: switchboard's consent
screen issues it a token for that one endpoint, and no long-lived credential is stored on the
client.

## What an endpoint is scoped to

Every endpoint carries a scope you set at vend time. An agent can never widen its own scope.

- **Queues** — the set of queues this endpoint may see and act on. Queue names are scoped to the
  endpoint: two endpoints' `inbox` queues are unrelated.
- **A verb allowlist** — the MCP tools the endpoint may call. Each tool is gated by the verb of the
  same name:

  | Group | Verbs |
  |------|------|
  | Todos | `list_todos`, `get_todo`, `claim`, `claim_next`, `complete`, `fail`, `heartbeat` |
  | Webhooks | `create_webhook`, `list_webhooks`, `rotate_webhook`, `delete_webhook` |
  | Fan-out routes | `add_webhook_route`, `list_webhook_routes`, `remove_webhook_route` |
  | Routing rules | `list_webhook_rules`, `set_webhook_rules`, `add_webhook_rule`, `update_webhook_rule`, `move_webhook_rule`, `remove_webhook_rule`, `test_webhook_rules` |
  | Event history | `list_webhook_events`, `get_webhook_event`, `replay_webhook_event` |

  The web wizard pre-checks the seven todo verbs. `switchboard endpoint vend` grants all of them.
  `list_todos` also grants `get_todo`, so an endpoint vended before `get_todo` existed has it.
- **A webhook ceiling** — how many webhooks the endpoint may create, which source types (`github`,
  `gitea`, `cairn`, `generic`, `stripe`, `slack`), and which queues those webhooks and their routing
  rules may target. A ceiling of 0 disables webhooks.
- **A lifetime** — until revoked, or a duration after which the endpoint expires on its own.

The endpoint enforces this scope at the boundary on every call: verbs outside the allowlist and
queues outside the grant are denied.

## How to configure and vend one

1. **Open Endpoints** on the web board and choose **+ vend endpoint** (the six-step wizard) or
   **quick vend** (one page, no webhook ceiling).
2. **Name the agent, then choose its queues, verbs, webhook ceiling, and lifetime.** Naming the
   agent registers it; registration alone grants nothing.
3. **Vend, and copy the reveal.** It shows the MCP URL, the credential, and the MCP client
   configuration exactly once — for a Claude Code–style client, an `.mcp.json` entry:

   ```json
   {
     "mcpServers": {
       "switchboard": {
         "type": "http",
         "url": "https://<host>/mcp/<slug>",
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
  exactly one human. Every todo, webhook, or rule change carries that chain.
- **Revoke instantly.** Revoking an endpoint invalidates its stored credential and closes its live
  MCP sessions — the agent immediately loses all access, with no residue. **Revoke = kill the
  endpoint.**

## Scope is immutable — re-vend to change it

An endpoint's scope is **immutable**: a given URL + credential always means one fixed set of powers.
To change what an agent can do, **vend a new endpoint** with the new scope and revoke the old one,
rather than editing the live grant. The **rotate** button on an endpoint card starts the wizard
pre-filled from the old scope. This keeps every capability trivial to audit — there is no "when did
this scope change, and who changed it?" question.

## Next

- Connect it: [Connect an agent over MCP](/getting-started/connect-an-agent).
- See the work surface itself: [Drain the queue](/guides/draining-the-queue).
- Give one agent several least-privilege faces: [Personas](/guides/personas).

> Deeper detail: [ADR-0008 — Human principal + vended endpoints](/decisions/ADR-0008-human-principal-vended-endpoints),
> [ADR-0017 — MCP over Streamable HTTP only](/decisions/ADR-0017-mcp-streamable-http-only), and the
> [vended-endpoints spec](/specs/vended-endpoints/spec).
