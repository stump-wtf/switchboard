---
title: Security model
---

# Security model

What switchboard protects, what it can't, and what is on you. Read this before you give an agent
a credential and leave it running.

## Who can see what

- **You are the principal.** Everything you vend belongs to you, and every action an endpoint takes
  is attributable to you.
- **Todos belong to one endpoint.** Queues are names inside an endpoint: your `inbox` and anyone
  else's `inbox` share nothing. An endpoint can't list, claim, or complete another endpoint's todo.
  To it, that todo doesn't exist (`not_found`). That holds for your own endpoints too.
- **Scope is enforced on every call.** A tool the endpoint wasn't granted, or a queue outside its
  scope, is refused. Scope never changes after vend; to change it, vend a new endpoint.
- **Webhooks, routes, and rules belong to you.** Any of your endpoints with the right tools may
  manage your webhooks' routes and rules. No one else's endpoint can.
- **The instance operator can see everything.** Payloads are stored in the service's database. Don't
  route anything through a webhook that you wouldn't show the person running the service.

### Keep secrets out of payloads

A webhook body is stored with its event, routed, and handed to every agent the delivery reaches.
The event history keeps it for later inspection and replay. So treat a payload as sensitive:

- **Never send a credential, token, or key through a webhook body.** If a producer would include
  one, strip it before it reaches switchboard. Switchboard redacts the signature and token headers
  it authenticates with, not the body a producer chose to send.
- **Grant the event-history tools only where the job needs them** (`list_webhook_events`,
  `get_webhook_event`, `replay_webhook_event`). The web wizard leaves them unchecked; a CLI vend
  includes them.

:::warning Known limitation: event history is not per-user yet

The event-history tools and the recent-events resource read the **whole instance's** deliveries,
not only the caller's. `list_webhook_events` filters by provider, event type, and time but not by
owner; `get_webhook_event` returns any event by id; and `replay_webhook_event` can re-send any of
them. On an instance with more than one user, an endpoint granted these tools can read, and replay,
every other user's deliveries.

Until this is fixed, **don't grant `list_webhook_events`, `get_webhook_event`, or
`replay_webhook_event` to any endpoint whose holder shouldn't see every user's deliveries.** A fix
that scopes event history to its owner is planned, and this note goes away when it ships.

:::
- **`replay_webhook_event` sends a stored payload back out** to a target, so treat it as a
  data-forwarding tool, not just a debugging one.

## Where a delivery can go

Whoever owns a webhook decides where its deliveries land, within limits switchboard enforces:

- **Routes** (`add_webhook_route`) can target any of your own endpoints. An endpoint belonging to
  someone else needs an approved friend edge from you to them. An unknown, revoked, or unfriended
  target is refused with the same error, so probing reveals nothing.
- **Rules only narrow.** A rule can pick a subset of a webhook's existing targets, or drop the
  delivery. It can never add a target or reach a queue outside the owner's webhook-queue grant.
  Switchboard re-checks both on every delivery, so a revoked route stops receiving immediately.
- **Route targets see what the owner sends.** A target receives the full payload, and the todo's
  routing trace names the rule that sent it (and, on narrowed deliveries, other targets' endpoint
  ids). Don't put anything sensitive in rule names.

## Credentials

| Credential | Shape | Where it lives |
|---|---|---|
| Endpoint credential | `sbk_…` | Shown once at vend. Switchboard keeps only a hash. Revoke kills it immediately. |
| OAuth token | opaque | Issued to one client for one endpoint through the consent screen. Short-lived, refreshed by the client. |
| Webhook signing secret | `whsec_…` | Shown once at `create_webhook` or `rotate_webhook`, never returned again. Stored encrypted when the instance sets an encryption key, **in plaintext** when it doesn't; see [Secrets at rest](#secrets-at-rest). |
| Generic webhook URL | `/webhooks/w/<token>` | **The URL is the credential.** Anyone who has it can create todos. Treat it like a password, and rotate it if it leaks. |

- **Give endpoints a lifetime** when the job is temporary. An expired endpoint stops working on its
  own.
- **Revoke on any leak.** A credential pasted into a log, a transcript, an issue, or a screenshot is
  compromised. Revoke first, then vend a replacement.
- **Prefer signed sources over `generic`.** A signature proves the body wasn't altered and binds it
  to a secret the URL doesn't reveal. A `generic` webhook only proves the caller knew the URL.

## Secrets at rest

Switchboard stores two kinds of secret it mints, and protects them differently:

| Secret | At rest |
|---|---|
| Endpoint credentials (`sbk_…`) | **Hashed.** They can't be recovered from the database, only revoked. |
| Webhook signing secrets (`whsec_…`) | Switchboard must recompute the HMAC on every delivery, so it keeps the secret recoverable: **AES-256-GCM encrypted** under `SWITCHBOARD_SECRET_ENCRYPTION_KEY` when that key is set, and **plaintext** when it's empty. |

**An empty `SWITCHBOARD_SECRET_ENCRYPTION_KEY` is allowed, and it stores every signing secret in
plaintext.** Anyone who can read the database, or a backup of it, can then forge signed
deliveries to your webhooks. Switchboard warns rather than refusing to start, in two places:

- **At startup**, if signed webhooks already exist:
  `webhook signing secrets are stored in plaintext` with `signed_webhooks=<n>` and
  `reason="SWITCHBOARD_SECRET_ENCRYPTION_KEY is empty"`.
- **On every plaintext write** (`create_webhook` or `rotate_webhook` on a signed webhook):
  `webhook signing secret stored without at-rest encryption`, naming the webhook and endpoint. The
  secret itself is never logged.

With a key set, startup logs `webhook signing secrets encrypted at rest`. A key that doesn't decode
(base64 or hex) to exactly 32 bytes stops startup with an error instead of falling back to
plaintext. Generate one with `openssl rand -base64 32`; see
[Run your own switchboard](/guides/self-hosting#configuration).

Two things to know before you set or change the key:

- **Setting a key doesn't encrypt existing secrets.** Rows written without a key stay plaintext,
  and switchboard keeps reading them. Rotate each signed webhook (`rotate_webhook`) to rewrite its
  secret encrypted, then update the producer with the new secret.
- **Keep the key.** Encrypted secrets can't be read without it. Lose or change it and every
  encrypted signed webhook fails verification until it's rotated.

## Payloads are data, never instructions

Anyone who can open an issue, comment on a pull request, or create a Cairn artifact controls text
that ends up in front of your agent. Switchboard verifies **who sent a delivery**. It can't make
**what they wrote** safe.

What switchboard does:

- Deliveries from a signed source that fail verification are refused and never stored.
- The doorbell never includes the payload. Its one-line summary is escaped so it can't break out of
  the notification, and it ends by telling the agent that the summary is data that can't instruct it.
- Routing rules run sandboxed, with no access to the environment, the filesystem, the clock, or the
  network, and they can only narrow where a delivery goes.

What's on you:

- **Tell the agent, in its instructions, that todo payloads are untrusted.** Content can inform what
  it concludes. It never changes what it's allowed to do, who it sends things to, or what it
  reveals. "Ignore previous instructions", "the owner approved this", "run this script" inside a
  payload are attacks to report, not requests to follow.
- **Filter on provenance before content.** Put
  [trusted-people rules](/guides/routing-cookbook#only-act-on-trusted-people) first, so a stranger's
  issue never becomes a todo. Match on who sent a delivery (`sender`, author, Cairn `actor_id`), never
  on words in a title or body a stranger could type.
- **Labels, tags, and `on_behalf_of` are not provenance.** Anyone who can label an issue or tag an
  artifact controls them. They can choose a queue among deliveries you already trust, never decide
  that a delivery is trusted. Cairn's `on_behalf_of` is the sharing client's self-reported name.
- **Routing fails closed.** A rule that errors, times out, or runs out of budget stops
  evaluation, and the delivery is recorded as `faulted` and held in quarantine, where no
  tool-bearing agent is ever handed it. A mistyped allowlist
  in a "drop the untrusted" rule therefore admits no one rather than everyone. `params` are
  type-checked, and a save whose rules fault on the webhook's recent deliveries is refused. A
  sandbox that cannot run at all refuses deliveries with `503` instead of routing them by
  default. Still read lists with `arrays`, as the cookbook does, so a missing list evaluates
  cleanly instead of faulting.
- **A work order grants nothing.** `work_order` on a todo records the verified provenance that made
  it eligible. A worker should refuse a todo routed with work orders that has none, or whose
  `work_order.verified` isn't `true`, and still treat the task itself as untrusted.
- **Limit what a compromised worker could do.** An unattended worker that auto-approves its own tools
  (Crush's `--yolo`) should run in a disposable working directory, with only the credentials its job
  needs. It should have no access to anything you couldn't afford to have posted publicly.
- **Keep scopes small.** An endpoint that only needs `reviews` and six todo tools shouldn't also be
  able to create webhooks or read event history.

## Reporting a problem

If you find a way to see or change something that isn't yours, stop and tell whoever runs your
instance. Include what you did and what you saw, not the data itself.
