---
title: Connect an agent over MCP
---

# Connect an agent over MCP

Your endpoint is a remote MCP server over Streamable HTTP. There's nothing to install on the
switchboard side: the agent needs the URL and either the bearer credential or an OAuth sign-in.

Getting todos into the agent works two ways:

- **Push (channels).** Switchboard advertises the `claude/channel` capability and sends a doorbell
  notification the moment a todo is ready. The agent reacts without being asked.
- **Pull (polling).** The agent calls `claim_next` or `list_todos` on its own schedule. It works
  with every MCP client.

Push is faster; pull is universal. Because the queue is the record, you can mix them freely.

| Client | Push | Status |
|---|---|---|
| Crush, the `joestump-agent` fork | yes, with `--channels` or `channel_enabled` | Runs the always-on workers this service was built around. |
| Claude Code | research-preview channels | Not verified with switchboard; see below. |
| Any other MCP client | no | Poll. |

## Crush

Channel support and the fixes that keep a channel session alive are in the fork at
[github.com/joestump-agent/crush](https://github.com/joestump-agent/crush). Upstream Crush has a
hidden `--channels` flag, but the persistent `channel_enabled` setting and the session-recovery
fixes the switchboard workers depend on are fork features. The fork publishes no releases, so build
it with Go:

```bash
git clone https://github.com/joestump-agent/crush.git
cd crush && go build -o crush .
```

Add switchboard to your `crush.json` (the project's, or the global one in `~/.config/crush/`):

```json
{
  "mcp": {
    "switchboard": {
      "type": "http",
      "url": "https://switchboard.stump.wtf/mcp/<slug>",
      "headers": { "Authorization": "Bearer $SWITCHBOARD_TOKEN" },
      "channel_enabled": true
    }
  }
}
```

Crush expands `$SWITCHBOARD_TOKEN` when it starts the server, so the token stays in your
environment. `channel_enabled` opts this server in to push. Instead of setting it in config, you
can opt in per launch:

```bash
crush --channels server:switchboard
```

If you use Crush's newer `crushrc` format instead of `crush.json`, declare the server there and opt
in with the flag:

```bash
# crushrc
mcp add switchboard --type http --url "https://switchboard.stump.wtf/mcp/<slug>" \
  --header Authorization "Bearer $SWITCHBOARD_TOKEN"
```

A server listed in `mcp` stays silent until one of those opts it in. Each doorbell arrives in the
session as a `<channel source="switchboard" todo_id="…" queue="…">` block telling the agent to claim
the todo. From there, the agent uses the switchboard tools like any other.

## Claude Code

Add the endpoint as an HTTP MCP server. In a project `.mcp.json`, reference the token through an
environment variable so the file is safe to commit:

```json
{
  "mcpServers": {
    "switchboard": {
      "type": "http",
      "url": "https://switchboard.stump.wtf/mcp/<slug>",
      "headers": { "Authorization": "Bearer ${SWITCHBOARD_TOKEN}" }
    }
  }
}
```

Or from the command line, for your user only:

```bash
claude mcp add --transport http --scope user switchboard https://switchboard.stump.wtf/mcp/<slug> \
  --header "Authorization: Bearer $SWITCHBOARD_TOKEN"
```

That form expands the variable immediately, so the literal token is saved in your user config.

The tools work in any Claude Code session. **Push is a different matter, so here is exactly where it
stands.** Claude Code's channels are a
[research preview](https://code.claude.com/docs/en/channels-reference). A server that isn't a
Claude Code plugin has to be loaded with a development flag:

```bash
claude --dangerously-load-development-channels server:switchboard
```

Channels also require a claude.ai login, and on Team and Enterprise plans an admin has to enable
them. Switchboard implements the channel contract, but **nobody has confirmed a Claude Code session
turning a switchboard doorbell into work**. The one attempt to run always-on Claude Code workers
failed for an unrelated reason (the sessions weren't logged in) and was retired. If you depend on
Claude Code picking up work, poll as described below, or run the worker in Crush.

## Sign in with OAuth instead of a token

Clients that support MCP OAuth can connect with only the URL. The reveal screen's **URL-only
wiring** is that config. On first connect, switchboard answers `401` with a pointer to its
authorization server. The client registers itself, opens your browser on switchboard's consent
screen (**Authorize access →**), and receives a token scoped to that one endpoint. For Claude Code,
add the server without a header and run `/mcp` to authenticate.

**Sign in to the web board first.** If the consent request arrives while you're signed out, the
sign-in lands on the board and the client is left waiting. Start the client's connection again once
you're signed in.

OAuth never creates an endpoint. It authorizes a client against one you already vended.

## Any other MCP client: poll

Without push, a worker loop is two calls:

1. `claim_next` with `{"queue": "inbox"}` hands back the oldest available todo and takes its lease
   in one step. When there is nothing to do it returns `{"empty": true}`, which is normal rather
   than an error. Several workers can call it concurrently and each gets a different todo.
2. Do the work, then `complete` with `{"id": "…", "result": {…}}` or `fail` with the same shape.

To look before claiming, call `list_todos` with a queue, a state, and a limit:

```json
{"queue": "inbox", "state": "pending", "limit": 20}
```

Always pass all three. Each todo carries its **entire** webhook payload, so an unfiltered list on a
busy queue can run to megabytes. `limit` defaults to 50; ask for 200 or fewer, because a larger
value currently comes back as 50.

Poll at the latency you actually need. Polling an empty queue is cheap for switchboard, but each
poll can cost a model turn on your side.

## One channel consumer per server

A doorbell rings **one** connected session per todo, not all of them. On an endpoint with several
sessions, switchboard prefers one with an open stream and rotates among them. That's what makes
competing consumers work. It also means every session you point at an endpoint must actually work
the queue:

- **Don't channel-enable the same switchboard server in two different jobs.** Suppose a chat agent
  you only glance at and a queue worker both have `switchboard` channel-enabled. Doorbells land in
  whichever wins, and the queue looks dead because the events went to the session nobody was
  watching. Give switchboard push to exactly one kind of session, and connect the others with
  channels off.
- **Don't leave a deaf session connected.** A session that holds the connection but can't act on it
  (not logged in to its model, stuck on a permission prompt, out of quota) still gets rung. The todo
  waits for the next ring.
- **One endpoint per job.** Doorbells follow the endpoint's queue scope, so a worker for reviews
  and a worker for triage should each have their own endpoint, not share one.

## Keep a worker running with Harness

For a worker that's always on, supervise the agent with
[Harness](https://stump-wtf.github.io/harness/). It restarts the agent on failure, keeps its
scrollback, and lets you attach from another terminal or over SSH. Install it and start its daemon
by following the [Harness quickstart](https://stump-wtf.github.io/harness/usage/quickstart/), then
add a worker to `~/.config/harness/harness.toml`:

```toml
[harness.switchboard-worker]
harness = "crush"
args = ["--yolo", "--channels", "switchboard"]
workdir = "~/work/switchboard-worker"
env_file = "~/.config/harness/switchboard-worker.env"   # SWITCHBOARD_TOKEN=sbk_…, mode 0600
description = "Crush · switchboard doorbells"
restart = "on-failure"
restart_delay = 30
enabled = true
```

```bash
harness reload                       # pick up the new harness
harness list                         # is it running?
harness attach switchboard-worker    # watch it work (Ctrl-C detaches)
harness logs switchboard-worker --follow
```

Notes on those choices:

- **`--yolo`** lets the agent run tools without a human approving each one, which an unattended
  worker needs. It also means a malicious payload meets an agent with no brakes. Give the worker a
  dedicated, disposable `workdir`, the fewest credentials it needs, and routing rules that only
  admit trusted senders. Read [Security model](/guides/security-model) before you turn this on.
- **`restart = "on-failure"`** rather than `always`: a worker that exits cleanly was told to stop,
  and one that fails on every launch (an expired model quota, say) should stop spending after its
  retry budget instead of looping. `restart_delay` spreads those retries out.
- **Run the daemon under your init system** so it survives reboots. See
  [Harness supervision](https://stump-wtf.github.io/harness/usage/supervision/).

For scheduled, one-shot sweeps of a queue instead of an always-on session, see Harness's
[configuration reference](https://stump-wtf.github.io/harness/usage/configuration/) for `prompt` and
`schedule`.

## Give the agent the queue discipline too

Connecting an agent gets it the tools. It doesn't get it the judgement about when to use them, and a
busy queue punishes that quickly: most todos are exhaust rather than work, every `claim` and
`complete` echoes the producer's entire payload, and draining a flood by hand is a treadmill while
the producer keeps sending.

The `switchboard` skill packages that judgement for Claude Code and Crush. It doesn't replace this
documentation — [Working the queue well](/guides/working-the-queue) stays the reference, and the
skill points back at it.

For Claude Code:

```bash
claude plugin marketplace add stump-wtf/claude-plugin-switchboard
claude plugin install switchboard@claude-plugin-switchboard
```

It also ships three commands that run in the session holding the MCP tools: `/switchboard:triage`
(read-only bucketing), `/switchboard:work-next` (claim one and carry it to done), and
`/switchboard:drain` (clear a noise flood, source first).

Crush discovers skills through `options.skills_paths` in `crush.json`, an explicit list of
directories. Clone the plugin, then append the clone's own `skills` directory to that list as an
absolute path — nothing reads a directory that isn't in the list.

```bash
git clone https://github.com/stump-wtf/claude-plugin-switchboard.git ~/src/claude-plugin-switchboard
```

To check it loaded, ask the agent to look at a queue. With the skill it filters `list_todos` by
queue, state and a bounded limit, and buckets what comes back before touching anything, rather than
listing unfiltered and acting on the first row.

The skill grants nothing. The endpoint's scope is the only capability boundary and it's immutable
after vend, so no skill can widen a verb or a queue — see [Vend an endpoint](/guides/vend-an-endpoint),
which also lists every tool the surface serves.

## Check the connection

Ask the agent to call `list_webhooks`, or `list_todos` with `{"queue": "inbox", "limit": 5}`. An
empty list is a successful connection. A `forbidden` error names a queue or tool the endpoint wasn't
granted. A `401` means the credential is wrong, revoked, or expired.

## Next

[Receive your first webhook](/getting-started/first-webhook).
