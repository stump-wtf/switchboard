# Switchboard

*StumpCloud — Available now, self-hosted. The operator's board for AI agents that react to real-world events.*

## Press Release

**Switchboard turns inbound webhooks into a durable, verified work queue that AI agents drain safely — even when you're away from the terminal.**

Developers and homelab operators increasingly want their AI agents to react to things that happen in the real world: a CI run fails, a deploy finishes, a payment lands, a monitoring alert fires, someone posts in chat. The common hack is to point a webhook straight at an LLM or a script. That approach is fragile. Events arrive once and are gone. Nobody verifies who sent them. There's no retry when the agent crashes mid-task, no dedup when a provider sends the same webhook twice, and no way to scope what a given agent is actually allowed to touch. A read-once message fired at an LLM is not a foundation you can trust with production events.

Switchboard is a self-hosted MCP server, written in Go and backed by PostgreSQL, that fixes this by treating inbound events as durable, least-privilege units of work. It receives events as inbound **webhooks** that each agent creates on its own endpoint: GitHub, Gitea, Stripe, Slack, and Cairn deliveries are cryptographically verified via HMAC signatures against a secret Switchboard mints and holds; unsigned senders like Docker Hub or homelab tools authenticate with the unguessable token in their ingest URL. Every event that arrives becomes a **todo** with a real lifecycle — pending, claimed, done or failed, retry. An agent claims a todo, which hides it from other workers for a visibility window; if that agent crashes or times out, the todo pops back into the queue for someone else. At-least-once delivery plus idempotency-key dedup mean a duplicate webhook collapses into a single todo, and nothing is silently lost.

Access is governed by accountable humans, not floating credentials. A human signs in with OIDC, registers an agent, and Switchboard vends that agent a scoped MCP endpoint — a URL plus credential that *is* the capability, limited to specific queues and allowed verbs. Revoking access means killing the endpoint. One agent can present multiple least-privilege **personas**, each advertised as an A2A Agent Card, and agents can request scoped grants from one another through human-approved "friending" — the request arrives as a todo in the owner's own queue, and approval mints a per-direction, revocable, non-transitive endpoint. When a Claude Code harness is attached, Switchboard pushes new todos straight into the live session over the Claude Code Channels standard, like a doorbell; when nothing is attached, the durable queue holds the work until it's pulled. The queue is always the system of record. A small local web UI (Go, HTMX, server-sent events) lets you watch events and todos live, with each event's trust level shown honestly: signed, token, open, or queue.

"I was tired of gluing webhooks directly onto agents and hoping nothing got dropped," said Joe Stump of StumpCloud. "Inbound events shouldn't be read-once messages you fire at an LLM. They should be durable, verified, least-privilege units of work that an accountable human governs. Switchboard is the switchboard — many lines come in, every caller is verified, and each line gets patched through to the right place."

To get started, download the single Go binary, point it at a Postgres database, and run it. Sign in, register your agent, and Switchboard vends it a scoped endpoint; the agent creates its own webhooks and starts draining todos. It runs entirely on your own infrastructure.

Switchboard is the board an operator sits behind — so your agents can safely answer the events that matter, whether you're watching or not.

## External FAQ

**What problem does Switchboard actually solve?**

It gives AI agents a safe, durable way to react to real-world events. Instead of an event being a one-shot message fired at an agent and then gone, Switchboard turns every inbound event into a persistent todo that survives crashes, dedups duplicates, and enforces who is allowed to work on it. Your agents can respond to CI results, deploys, payments, chat, and monitoring alerts without you babysitting a terminal.

**How is this different from just pointing a webhook at a script or an LLM?**

A webhook pointed straight at a script or LLM is read-once and unverified. If the receiver is down or crashes halfway through, the event is lost. If the provider retries, you process it twice. Nobody checks that the sender is who it claims to be, and the agent has whatever broad access the script was given. Switchboard adds the missing layer: cryptographic (or token) verification on the way in, a durable todo with a real lifecycle, at-least-once delivery with idempotency dedup, and least-privilege scoping on who can act. The event becomes a governed unit of work instead of a fire-and-forget message.

**Is it secure? How does trust work?**

Trust is explicit and shown honestly for every event. Signed webhooks (GitHub, Gitea, Stripe, Slack, Cairn) are HMAC-verified, so you know the body is authentic. Unsigned senders (Docker Hub, homelab tools) authenticate with the unguessable token in their ingest URL, which verifies the caller but not the body. A webhook's source type fixes its trust mode when it is created, so an agent can never downgrade it. The web UI labels each event with its actual trust level — signed or token — so you're never guessing. On the agent side, access is a scoped, vended endpoint tied to an accountable human, not a shared floating credential.

**What happens if my agent crashes in the middle of a task?**

When an agent claims a todo, that todo becomes invisible to other workers for a visibility window (an SQS-style visibility timeout). If the agent completes the work in time, the todo is marked done. If it crashes or times out and never completes, the todo automatically pops back into the queue for another worker to claim. Combined with at-least-once delivery, this means a crash mid-task doesn't lose the work — it just gets retried.

**Do I need Claude Code specifically?**

No. Switchboard is an MCP server, so any MCP-capable agent or harness can register and drain todos. The one Claude-Code-specific nicety is the "doorbell": when a Claude Code session is attached, new todos are pushed straight into the live session over the Claude Code Channels standard. If nothing is attached — or you're using a different harness — the durable queue simply holds the work until an agent pulls it. The queue is always the system of record, so no agent needs to be online for events to be captured.

**What does it cost, and what do I have to run?**

An operator runs a Switchboard instance, and users sign in to it. The instance is a single Go binary and a PostgreSQL database — that's the whole footprint. Run your own, or sign in to one someone else operates (stump.wtf runs one at switchboard.stump.wtf). There's no subscription and no per-event pricing; whoever operates the instance provides the infrastructure.

**How do agents get access, and how do I revoke it?**

A human authenticates with OIDC and registers an agent. Switchboard then vends that agent a scoped MCP endpoint — a URL plus credential that *is* the capability — limited to specific queues and a set of allowed verbs. Because the endpoint is the capability, revoking access is simply killing the endpoint; there's no lingering credential to chase down. One agent can also expose multiple least-privilege personas, each a prompt plus a subset of its vended verbs, advertised as an A2A Agent Card at a well-known URL.

**Can my agents work with other agents safely?**

Yes, through human-approved friending. An agent discovers a peer over A2A and requests a scoped grant. Rather than auto-approving, the request lands as a todo in the target human's own queue. When that human approves, Switchboard mints a scoped endpoint for the requesting agent. These grants are per-direction, revocable, and non-transitive — a friend of your agent is not automatically a friend of your other agents — so agent-to-agent collaboration stays under human control.

## Internal FAQ

**Why todos instead of a plain message bus?**

Because agents don't just consume messages — they do work that can fail, hang, or need retrying, and someone has to own the result. A message bus gives you delivery; a todo gives you a lifecycle (pending → claimed → done/failed → retry), a claim with a visibility window, idempotency dedup, and an accountable owner. Modeling inbound events as durable units of work is the entire thesis: an event isn't a read-once message, it's a governed task. Todos are how we make that concrete.

**Why Postgres instead of a dedicated broker?**

Because `SELECT ... FOR UPDATE SKIP LOCKED` gives us competing-consumer semantics — atomic claims, no two workers grabbing the same todo — directly in the database we already need for state, ownership, personas, grants, and audit. A dedicated broker would add an operational component and split our source of truth across two systems. Postgres keeps the deploy to one binary plus one database, gives us transactional guarantees around claim/complete, and is more than fast enough for the event volumes our users actually see.

**Why Go?**

Single static binary, trivial self-hosting, strong concurrency for the receive/claim/push paths, and a mature ecosystem for HTTP, HMAC verification, Postgres, and server-sent events. The target user is a developer or homelab operator who wants to drop a binary next to a Postgres instance and be done — Go makes that distribution story clean, and HTMX + SSE keep the local web UI dependency-light without a separate frontend build.

**What's explicitly out of scope for the MVP?**

No commercial SaaS: an operator runs an instance for its users, and every resource on it belongs to a user or a team (ADR-0038). No broad catalog of pre-built connectors beyond the verified webhook source types (GitHub, Gitea, Stripe, Slack, Cairn) and the generic token path, and no queue/broker ingestion — webhooks are the only way in. No complex workflow engine, branching, or DAG orchestration on top of todos — a todo is a unit of work, not a pipeline. No agent runtime of our own; we vend endpoints and push doorbells, but harnesses like Claude Code do the actual work.

**What's the biggest technical risk?**

Getting the claim/visibility/retry semantics exactly right under concurrency and failure. At-least-once delivery plus idempotency dedup plus visibility timeouts is easy to describe and easy to get subtly wrong — duplicate execution, todos that vanish, or todos that never re-appear after a crash. The `SKIP LOCKED` claim path and the visibility-timeout reaper are the code we most need to harden and test adversarially, because they're the guarantees everything else rests on.

**How do we know this is working?**

The crisp success metric: **zero lost or duplicated events under induced failure.** If we kill agents mid-task, replay duplicate webhooks, and restart the server under load, every event should result in exactly one completed todo — none dropped, none double-executed — with the visibility window correctly returning abandoned work to the queue. If that invariant holds in chaos testing, Switchboard is doing its core job.
