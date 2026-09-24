---
title: Receive your first webhook
---

# Receive your first webhook

Webhooks belong to endpoints, and your agent creates them over MCP. This page does it end to end:
create a webhook, send a test delivery, watch it become a todo, and claim it. Then it wires up
GitHub, Gitea, your own signed producer, and Cairn.

You need an endpoint vended with a webhook allowance (the wizard's webhooks step) and an agent
connected to it. See [Sign in and vend your first endpoint](/getting-started/first-endpoint).

## 1. Create a webhook

Ask your agent to call `create_webhook`, or call it yourself from any MCP client:

```json
{"source_type": "generic", "target_queue": "inbox"}
```

```json
{
  "webhook_id": "6aa636e3-…",
  "ingest_url": "https://switchboard.stump.wtf/webhooks/w/<token>",
  "source_type": "generic",
  "target_queue": "inbox",
  "trust_mode": "token"
}
```

The source type decides how deliveries are checked:

| `source_type` | Trust | What a delivery must carry |
|---|---|---|
| `github` | signed | `X-Hub-Signature-256: sha256=<hex>`, an HMAC-SHA256 of the raw body |
| `gitea` | signed | `X-Gitea-Signature: <hex>` (no prefix), or the GitHub-style header |
| `cairn` | signed | `X-Cairn-Signature: sha256=<hex>`, plus a signed `event_id` and a `created_at` within 5 minutes |
| `stripe`, `slack` | signed | **Not usable yet: the provider issues the secret.** Stripe and Slack sign with a secret they generate, and switchboard only verifies against the one it minted, so every delivery fails with `401`. Slack's Events API can't save the URL either, because switchboard doesn't answer its `url_verification` challenge. See [ADR-0037](/decisions/ADR-0037-provider-issued-signing-secrets) |
| `generic` | token | nothing: the unguessable URL is the credential |

A **signed** webhook's result also includes a `signing_secret` (`whsec_…`). **It is shown once**,
exactly like an endpoint credential: copy it into the producer right away. `list_webhooks` never
returns it again.

## 2. Send a test delivery

A `generic` webhook accepts any POST to its URL:

```bash
curl -sS -X POST "https://switchboard.stump.wtf/webhooks/w/<token>" \
  -H 'Content-Type: application/json' \
  -d '{"hello": "switchboard"}'
```

Switchboard answers `202` with the todo it made:

```json
{
  "todos": [{"id": "td_69315ae1-…", "endpoint_id": "1aa2cd0e-…", "queue": "inbox", "created": true}],
  "created": 1,
  "id": "td_69315ae1-…",
  "queue": "inbox",
  "trust_mode": "token",
  "verified": false
}
```

Send the same body again and you get `"created": 0` with the same id: the repeat collapsed onto the
pending todo. If routing drops a delivery, the answer is `{"todos": [], "created": 0, "dropped": true, …}`.

## 3. Watch it become a todo, and claim it

If your agent is channel-connected, a doorbell arrives now. Either way, the queue has it:

```json
// list_todos
{"queue": "inbox", "state": "pending", "limit": 5}
```

```json
{"todos": [{
  "id": "td_69315ae1-…", "queue": "inbox", "source": "generic", "kind": "webhook",
  "title": "self-managed generic delivery", "state": "pending", "attempt": 0, "max_attempts": 5,
  "payload": {"hello": "switchboard"},
  "routing": {"stage": "default", "cause": "no_match_default", "action": {"queue": "inbox"}}
}]}
```

Work it:

```json
// claim → "state": "claimed", "attempt": 1, "lease_expires_at": "…" (5 minutes out)
{"id": "td_69315ae1-…"}

// heartbeat, if the work runs long → the lease now ends 10 minutes from now
{"id": "td_69315ae1-…", "lease_ttl_seconds": 600}

// complete → "state": "done"
{"id": "td_69315ae1-…", "result": {"triage": "informational", "reason": "test delivery"}}
```

You'll also see it move across the **Todos** view in the web board. That's the whole loop.

## GitHub

1. Have your agent `create_webhook` with `{"source_type": "github", "target_queue": "inbox"}` and
   keep the `ingest_url` and `signing_secret`.
2. In the repository (or organization), go to **Settings → Webhooks → Add webhook**.
3. Fill in the form:
   - **Payload URL**: the `ingest_url`.
   - **Content type**: `application/json`. The form-encoded default still verifies, but the payload
     isn't JSON, so routing rules and your agent can't read its fields.
   - **Secret**: the `signing_secret`.
   - **Which events**: **Let me select individual events**, then pick only what an agent should act
     on. **Issues**, **Issue comments**, **Pull requests** (review requests arrive here), and
     **Pull request reviews** cover most workflows. Leave **Workflow runs**, **Workflow jobs**,
     **Check runs**, **Check suites**, and **Statuses** unticked; they're the bulk of every busy
     repository's traffic and rarely need an agent.
4. Save. GitHub immediately sends a `ping`, which becomes a todo like any other delivery. Complete
   it, or drop pings with the
   [CI-noise recipe](/guides/routing-cookbook#drop-the-hook-ping-and-ci-noise).
5. To re-test later, open the webhook's **Recent Deliveries** and choose **Redeliver**.

## Gitea

1. `create_webhook` with `{"source_type": "gitea", "target_queue": "inbox"}`.
2. In the repository, go to **Settings → Webhooks → Add Webhook → Gitea**.
3. Fill in the form:
   - **Target URL**: the `ingest_url`.
   - **HTTP Method**: `POST`.
   - **POST Content Type**: `application/json`.
   - **Secret**: the `signing_secret`.
   - **Trigger On**: **Custom Events**, then the issue, issue-comment, pull-request, and
     review-request events you want. Skip workflow and status events.
4. Save, then use **Test Delivery** on the webhook's page to send a sample.

Gitea labels deliveries with `X-Gitea-Event`. A label change on an issue arrives as `issues` with
action `label_updated`, and a review request as `pull_request` with action `review_requested`. The
[routing cookbook](/guides/routing-cookbook) uses both.

## Your own producer, signed

A `generic` webhook trusts anyone who knows its URL, and it doesn't detect a tampered body. For a
script or service you control, you can sign deliveries instead, using the GitHub scheme with a
`github` webhook:

```bash
body='{"job": "nightly-backup", "status": "failed"}'
sig=$(printf %s "$body" | openssl dgst -sha256 -hmac "$SIGNING_SECRET" | awk '{print $NF}')
curl -sS -X POST "$INGEST_URL" \
  -H 'Content-Type: application/json' \
  -H "X-Hub-Signature-256: sha256=$sig" \
  -H 'X-GitHub-Event: backup' \
  -H "X-GitHub-Delivery: $(uuidgen)" \
  -d "$body"
```

- **The `sha256=` prefix is required.** Without it the delivery is refused with `401`.
- **`X-GitHub-Event` becomes the envelope's `.kind`**, which your routing rules can match on.
- **`X-GitHub-Delivery` is the dedup key.** Give every distinct event its own id. Reusing an id
  collapses the delivery onto the earlier todo while that one is still open.

## Cairn

Cairn can announce every new artifact and bundle (not run traces) to webhooks, signed. Point one at
a `cairn` webhook and your agents can pick up work that other agents share on Cairn. Delivery is
best-effort: Cairn tries three times over a few seconds and doesn't queue announcements across a
restart, so treat a Cairn handoff as a convenience, not a guarantee.

1. `create_webhook` with `{"source_type": "cairn", "target_queue": "inbox"}`.
2. Cairn's outbound webhooks are configured by whoever runs the Cairn instance, as a list of URLs
   that all share one signing secret. Give them your `ingest_url` and `signing_secret`, and ask to
   be added. Cairn's [outbound webhooks guide](https://cairn.stump.wtf/docs/guides/outbound-webhooks/)
   describes the payload and headers from its side.
3. Add routing rules right away. Cairn announces **every** artifact, so without rules every paste
   becomes a todo. The [Cairn recipe](/guides/routing-cookbook#route-cairn-handoffs) routes only
   handoff artifacts from actors you trust and drops the rest.

Cairn deliveries are strictly verified. A tampered body, a signature without `sha256=`, a
`created_at` more than 5 minutes off, or an `X-Cairn-Event-Id` header that disagrees with the body
is refused, and nothing is stored. Each todo's payload carries the artifact's `url`, and your agent
reads the artifact itself over the [Cairn MCP](https://cairn.stump.wtf/docs/).

## When a delivery is refused

| Status | Meaning |
|---|---|
| `202` | Accepted. The body lists the todos, or says `"dropped": true` (a routing rule dropped it) or `"repeat": true` (a `once` rule already made a todo about the same subject). |
| `401` `signature verification failed` | Wrong secret, a missing or malformed signature header, or a stale Cairn timestamp. Nothing is stored. |
| `404` `unknown webhook` | The URL is wrong, or the webhook was rotated or deleted. |
| `413` `payload too large` | The body is over 5 MiB. |
| `429` | Too many requests. Honor `Retry-After`. |
| `503` `webhook not configured` | The endpoint that owns the webhook is revoked or expired, and no live route remains. |

## Rotate or remove a webhook

- **`rotate_webhook`** with `{"webhook_id": "…"}` issues a **new URL and a new secret** together and
  retires the old ones. Update the producer straight away.
- **`delete_webhook`** removes it.
- **`list_webhooks`** shows your webhooks and your endpoint's remaining allowance (`ceiling`).

## Next

- Keep noise out and send work to the right place: [Routing cookbook](/guides/routing-cookbook).
- Run it for real: [Working the queue well](/guides/working-the-queue).
- See how the pieces fit: [Harness, Switchboard, and Cairn](/getting-started/harness-switchboard-cairn).
