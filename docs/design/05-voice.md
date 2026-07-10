---
title: Voice
---

# Voice

Switchboard speaks like a 1940s telephone exchange run by a very good operator: warm, terse,
technically exact. The metaphor is load-bearing — it names the product, the views, and the verbs.

## The metaphor

| Domain thing | Switchboard word |
|--------------|------------------|
| Inbound webhook/queue event | a **line** ringing |
| Signature/trust verification | the operator **verifying the caller** |
| Enqueue as durable todo | **patched through** |
| Mint a scoped MCP endpoint | **vend** an endpoint |
| Revoke | **kill the endpoint** |
| Lease expiry recovery | the **reaper re-surfaces** the todo |
| The web UI | the **operator board** |

## Taglines

Every view opens with a Zilla Slab title and a lowercase, middot-separated **mono tagline** that
states the contract, not marketing:

- `many lines in · each verified · patched through`
- `claim under a lease · complete with an ack · dedup by idempotency key · at-least-once`
- `humans are the accountable principals · each endpoint is a scoped, revocable capability · revoke = kill the endpoint`
- `one agent, many least-privilege faces · a human-authored prompt plus a verb subset · advertised as an A2A Agent Card`
- `agents discover each other over A2A · no one talks by default · links are mutual and non-transitive — both operators must approve`

Rules: lowercase, `·` separators, no trailing period, each clause a true system guarantee.

## Microcopy rules

- **State labels are mono and literal**: `verifying…`, `patched → todo`, `claimed · ci-bot`,
  `done ✓`, `⧗ 23s lease`, `↻ retry 18s`, `endpoint killed · 4h ago`, `none negotiated`.
- **Toasts narrate the ledger**, id first: `td_8f2a · lease expired, re-surfaced to queue`,
  `td_41bd · completed, ack sent to source`, `ci-bot · endpoint killed`,
  `@maya · approved, A2A link live`.
- **Warnings say the consequence plainly**: *"Copy the credential now — it is shown once. The
  endpoint is the capability; revoking kills it."* / *"if the lease expires, the reaper re-surfaces
  this todo to the queue"*.
- **Buttons pair verb + contract** where the contract matters: `Claim under lease`,
  `Complete · ack`, `Vend endpoint →`, `Send request →`.
- **Counts ride labels**: `Pending · 2`, `LIVE · 23/min`, `dedup ×3`.
- Sentence case for prose, lowercase for mono meta, UPPERCASE only for micro-labels
  (`SCOPED QUEUES`, `IDEMPOTENCY KEY`).
- Never anthropomorphize agents beyond the operator frame; never exclamation points.

## Writing checklist

1. Could an operator say it while patching a call? (short, factual, present tense)
2. Does it state the guarantee (lease, ack, dedup, revoke) rather than vibes?
3. Is the id present when a specific todo/endpoint/friendship is meant?
4. Mono for machine facts, Sans for human sentences, Slab for names of things.
