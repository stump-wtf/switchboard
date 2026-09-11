---
title: Sign in and vend your first endpoint
---

# Sign in and vend your first endpoint

An endpoint is the one thing your agent needs: an MCP URL and a credential, scoped to the queues
and tools you choose. This page gets you one, in the browser or from the command line.

## Sign in

1. Open [switchboard.stump.wtf](https://switchboard.stump.wtf) and choose **open the operator
   board →**.
2. Choose **Log in with Pocket ID** and sign in with your passkey.

Your switchboard account is created the first time you sign in. Sign-in itself goes through the
instance's identity provider, so if it doesn't recognize you, ask whoever invited you to switchboard
for an account there. A browser session lasts 12 hours.

The left rail has **Board** (live deliveries), **Todos** (the queue), **Endpoints** (what you've
vended), **Friends**, and **Providers**.

## Vend in the browser

There are two ways to vend from **Endpoints**:

- **quick vend** asks for a name, queues, tools, and a lifetime on one page. It **cannot** grant
  webhooks, so its agent can drain queues but can't create an ingest URL.
- **+ vend endpoint** is the full six-step wizard. Use it for your first endpoint, because the
  webhooks step is what lets your agent receive GitHub, Gitea, or Cairn events.

### The wizard, step by step

| Step | Field | For a first endpoint |
|---|---|---|
| 01 agent | **Agent name** | Something you'll recognize in a list, e.g. `my-laptop-crush`. |
| 02 queues | **Scoped queues** / **Add queues** | `inbox`. Add every queue your routing rules will use, e.g. `reviews`. |
| 03 verbs | **Allowed verbs** | The six todo verbs are pre-checked. Add `create_webhook`, `list_webhooks`, `rotate_webhook`, `delete_webhook`, and the seven rule verbs (`list_webhook_rules` … `test_webhook_rules`) if this agent will manage webhooks. Add `list_webhook_events` and `get_webhook_event` only if you need them (see [Security model](/guides/security-model)). |
| 04 webhooks | **Max self-managed webhooks** | `2` or `3`. Leaving it at `0` disables webhooks. |
| | **Allowed source types** | The producers you'll use: `github`, `gitea`, `cairn`, `generic`. |
| | **Webhook target queues** | The same queues as step 02. A rule can only route to a queue listed here. |
| 05 lifetime | **Credential lifetime** | `until revoked`, or `30 days` if you'd rather it expire. |
| 06 confirm | | Check the summary and choose **Vend endpoint →**. |

**Scope is permanent.** An endpoint's queues, tools, and webhook limits can't be edited later. To
change them you vend a new endpoint and revoke the old one; the **rotate** button on an endpoint
card starts the wizard pre-filled from the old one. So grant what the agent's job needs now, not
everything it might ever need.

### The one-time reveal

After **Vend endpoint →** you see, **exactly once**:

- **MCP endpoint URL**: `https://switchboard.stump.wtf/mcp/<slug>`
- **Credential · shown once**: `sbk_…`
- **Wire it into your MCP client**: a ready-to-paste `.mcp.json` block with the credential
  embedded.
- **OAuth-capable client? URL-only wiring**: the same block with no credential in it, for clients
  that sign in through the browser instead (see
  [Connect an agent](/getting-started/connect-an-agent#sign-in-with-oauth-instead-of-a-token)).

Switchboard stores only a hash of the credential. If you lose it, revoke the endpoint and vend
another.

## Store the credential safely

The credential is the whole capability: anyone holding it can do everything the endpoint's scope
allows, until you revoke it.

- **Put it in a secret store**, such as your password manager or your machine's keychain, and hand
  it to the agent through an environment variable such as `SWITCHBOARD_TOKEN`.
- **Reference the variable from config instead of pasting the token.** Crush expands `$VAR` in MCP
  headers, and Claude Code expands `${VAR}` in `.mcp.json`. Both are shown in
  [Connect an agent](/getting-started/connect-an-agent).
- **Never commit it.** A project `.mcp.json` or `crush.json` with a literal `sbk_…` in it is one
  `git add .` from public.
- **Or skip storing it.** OAuth wiring keeps no long-lived credential on disk.

If a credential ends up somewhere it shouldn't, such as a log, a transcript, a screenshot, or a
chat, **revoke the endpoint right away**. A leaked credential stays live until you do.

## Revoke

On **Endpoints**, choose **revoke** on the endpoint's card, then **Revoke — kill this endpoint**.
Every call using that credential fails from that moment, and its live MCP sessions are closed.
Revocation can't be undone; vend a replacement.

## Or use the command line

The `switchboard` binary is also a small operator CLI. It isn't published for download, so this
section applies only if the person running your instance gave you a build.

```
switchboard login https://switchboard.stump.wtf    # opens your browser; --no-browser prints the URL
switchboard status                                  # where you're logged in, and whether it's live
switchboard endpoint vend my-agent --queue inbox    # vend, printing the credential once
switchboard endpoint list                           # slug, agent, state, queues, expiry (never tokens)
switchboard endpoint list --json                    # the same, with each endpoint's id
switchboard endpoint revoke my-agent-k3x9           # asks first; -y skips the prompt
switchboard agent list
switchboard logout                                  # forgets local credentials
```

Sign in to the web board first. If `switchboard login` opens the browser while you're signed out,
the sign-in lands on the board instead of the consent screen; run `switchboard login` again once
you're signed in.

`endpoint vend` makes a **different** endpoint from the wizard:

| | `switchboard endpoint vend` | The web wizard |
|---|---|---|
| Queues | exactly one (`--queue`, default `inbox`) | as many as you list |
| Tools | every agent tool | what you check |
| Webhooks | one `generic` webhook, created for you; its **Ingest URL** is printed | up to your max, of the source types you allow |
| Lifetime | until revoked | your choice |

A CLI-vended endpoint can't create a GitHub, Gitea, or Cairn webhook, and it has already used its
one webhook. For forge or Cairn events, use the wizard.

## Next

[Connect an agent over MCP](/getting-started/connect-an-agent).
