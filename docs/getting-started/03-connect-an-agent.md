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

"Push" is two things, and a client needs both. **Delivery** is switchboard writing the doorbell to
the session's open notification stream. **Wake** is the client turning that notification into a
model turn while it sits idle. Switchboard only controls delivery: its log says
`mcp doorbell delivered` the moment the write succeeds, whether or not the client does anything with
it. A client that holds the stream open but hasn't opted the server in as a channel is the usual
reason a doorbell is delivered and nothing happens.

| Client | Push | Status |
|---|---|---|
| Crush, the `joestump-agent` fork | yes, with `--channels` or `channel_enabled` | Runs the always-on workers this service was built around. |
| Claude Code | yes, with a channels flag | Verified: an idle session wakes, claims, and completes. Needs a startup flag; see below. |
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

This page writes the flag as `server:switchboard`, which matches Claude Code's syntax. The fork
also accepts the bare server name (`--channels switchboard`), case-insensitively.

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

The tools work in any Claude Code session. **Push needs one more thing: the server has to be loaded
as a channel.** Listing it in `.mcp.json` is not enough. Claude Code keeps the notification stream
open either way, so switchboard reports every doorbell as delivered, and Claude Code silently
discards each one unless the server was named as a channel at startup. There is no error on either
side.

Claude Code's channels are a [research preview](https://code.claude.com/docs/en/channels). A server
that isn't on the preview's allowlist is loaded with the development flag:

```bash
claude --dangerously-load-development-channels server:switchboard
```

With that flag, push works end to end over switchboard's HTTP transport — no local adapter process.
Measured against Claude Code 2.1.270, with a real webhook delivery each time:

| Session | Doorbell delivered | What Claude Code did |
|---|---|---|
| Interactive, idle, **no** channels flag | yes | Nothing. Dropped silently. |
| Interactive, idle, with the flag | yes | Started a turn on its own, claimed the todo, completed it. |
| Same session, after that turn finished and 45s more idle | yes | Woke again, claimed, completed. |
| `claude -p` (print mode) | n/a | Answered its prompt and exited. No process is left to wake. |

What to know before you rely on it:

- **The flag asks for confirmation at startup**, every launch. An unattended worker sits at that
  prompt until someone answers it, so a supervised restart is not hands-off. Run it somewhere you
  can attach to.
- **It wants a claude.ai or Console login** — not Bedrock, Vertex, or Foundry — and on Team and
  Enterprise plans an admin has to enable channels. See
  [Team and Enterprise organizations](#team-and-enterprise-organizations).
- **A startup line reading `server:switchboard · no MCP server configured with that name` can be
  wrong.** It appears when the server comes from `--mcp-config` rather than a settings file, and
  the channel works regardless. Judge by whether a doorbell starts a turn.
- **Print mode can't be woken.** `claude -p` exits when its turn ends. For one-shot or scheduled
  runs, poll with `claim_next` as described below.
- **The worker still needs permission to use the tools.** A woken session that stops on a
  permission prompt for `claim` has been rung and can't act. Allow the switchboard tools up front,
  for example `--allowedTools mcp__switchboard`.

### Team and Enterprise organizations

Pro and Max accounts without an organization use the development flag above and nothing else.
In an organization, two managed settings decide what loads
([Anthropic: enterprise controls](https://code.claude.com/docs/en/channels#enterprise-controls)):

- **`channelsEnabled`** is the master switch. On claude.ai Team and Enterprise plans channels are
  blocked until an Owner turns it on (**Admin settings → Claude Code → Channels**) or it's set to
  `true` in managed settings. While it's off, **every** channel is blocked, including
  `--dangerously-load-development-channels`: the MCP server still connects and its tools work, but
  doorbells never start a turn. Console organizations using API keys are allowed by default unless
  they deploy managed settings.
- **`allowedChannelPlugins`** replaces Anthropic's allowlist with the organization's own list of
  `{ "marketplace": …, "plugin": … }` entries. A plugin on that list loads with
  `--channels plugin:<plugin>@<marketplace>`, without the development flag and its startup
  confirmation.

Switchboard's channel isn't packaged as a plugin yet: the
[Switchboard Claude Code plugin](https://github.com/stump-wtf/claude-plugin-switchboard) ships
skills and commands, but no MCP server. So today a managed organization needs `channelsEnabled`
and the development flag. Shipping the channel server inside the plugin is planned; once it's
released, an organization can list the plugin in `allowedChannelPlugins` and drop the development
flag.

For how channels behave inside Claude Code — event queueing while a turn is running, the
`<channel>` wrapper the model sees, permission relay, org policy — read Anthropic's
[Channels guide](https://code.claude.com/docs/en/channels) and
[Channels reference](https://code.claude.com/docs/en/channels-reference). An agent setting this up
for you should read both before changing anything.

## Reconnecting

A session that opens its notification stream is rung **at once** for work that was already
waiting, so a worker that restarts doesn't wait for the next re-ring. The catch-up is bounded:

- **At most 3 todos per attach**, the **oldest** pending ones in the endpoint's scope. The rest are
  reached by the regular re-ring (5 minutes, 20 minutes, 1 hour, 6 hours) or by your agent's own
  `list_todos`/`claim_next`.
- **Once a minute per todo, at most.** A worker that dies on a doorbell and reconnects isn't rung
  for the same todo again within a minute.
- **Charged to the same ring budget** as the re-rings: after the ring when it's created, a todo
  gets at most 5 more, whichever mechanism sends them. A todo whose budget is spent is still on the
  queue; it just isn't pushed.

So "I reconnected and only three todos rang" is expected. Push is only the doorbell: have the agent
drain with `claim_next` after it wakes. Endpoint presence
([ADR-0027](/decisions/ADR-0027-endpoint-presence-clock-in-clock-out)) will replace these
individual rings with one clock-in digest once it's implemented.

To prove a worker actually heard a doorbell, and not just that switchboard delivered it, send a
real delivery (see [Receive your first webhook](/getting-started/first-webhook)) and check that
the agent **claims and completes** the todo. "Delivered" in switchboard's log doesn't count.

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
args = ["--yolo", "--channels", "server:switchboard"]
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

Connecting an agent gets it the tools. It doesn't get it the judgement about when to use them, and
a busy queue punishes that quickly: most todos turn out to be exhaust rather than work, every
`claim` and `complete` echoes the producer's entire payload back into the agent's context, and
draining a flood by hand is a treadmill while the producer keeps sending.

The **switchboard skill** packages that discipline — triage before acting, one todo at a time under
its lease, narrow the source before clearing a backlog, and the sharp edges that return a plausible
wrong answer instead of an error (notably that a `limit` above 200 comes back as 50, so a flooded
queue reads as a nearly empty one). It doesn't replace this documentation:
[Working the queue well](/guides/working-the-queue) stays the reference, and the skill points back
at it.

The skill lives in a **public** repository, `stump-wtf/claude-plugin-switchboard`, even though the
service itself is closed source.

**Claude Code** installs it as a plugin:

```bash
claude plugin marketplace add stump-wtf/claude-plugin-switchboard
claude plugin install switchboard@claude-plugin-switchboard
```

It also ships three commands, which run in the session holding the MCP tools:
`/switchboard:triage` (read-only bucketing), `/switchboard:work-next` (claim one todo and carry it
to done), and `/switchboard:drain` (clear a noise flood the right way).

**Crush** discovers skills by directory rather than by installing plugins. Clone the repository and
point Crush at its `skills/` directory in your `crushrc`:

```bash
git clone https://github.com/stump-wtf/claude-plugin-switchboard.git ~/src/claude-plugin-switchboard
```

```bash
# ~/.config/crush/crushrc
option skill-path ~/src/claude-plugin-switchboard/skills
```

Crush also loads skills from a few directories automatically, `~/.config/crush/skills/` among them,
so **copying** the repository's `skills/` contents there works too. **Linking does not:** Crush
resolves symlinks when it decides whether a file belongs to a skills directory, so a symlinked skill
loads but its reads resolve back outside that directory and lose the exemption that lets an agent
read them without a permission prompt or a size limit. Either way, point Crush at the **directory of
skills**, not at one skill's folder inside it, and remember that a directory Crush doesn't know about
is never read.

**Check that it loaded** by asking the agent to look at a queue. With the skill, it filters
`list_todos` by queue, state and a bounded limit, and buckets what comes back before touching
anything, rather than listing unfiltered and acting on the first row.

**What it does not do.** It grants nothing. The endpoint's scope is still the only capability
boundary, and no skill can widen it — scope is fixed when the endpoint is vended. It also doesn't
connect anything: the wiring above is still required.

## Check the connection

Ask the agent to call `list_webhooks`, or `list_todos` with `{"queue": "inbox", "limit": 5}`. An
empty list is a successful connection. A `forbidden` error names a queue or tool the endpoint wasn't
granted. A `401` means the credential is wrong, revoked, or expired.

## Next

[Receive your first webhook](/getting-started/first-webhook).
