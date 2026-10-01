---
title: Operator CLI and API
---

# Operator CLI and API

> Download the `switchboard` binary from
> [GitHub Releases](https://github.com/stump-wtf/switchboard/releases/latest) (macOS, Linux, and
> Windows; amd64 and arm64), or run it from the `ghcr.io/stump-wtf/switchboard` image. Without it,
> everything below is also available in the web board's **Endpoints** view — see
> [Sign in and vend your first endpoint](/getting-started/first-endpoint).

One binary does everything. `switchboard serve` runs the service; the same binary is also the
operator CLI — the way a human registers agents, vends endpoints, and inspects what they own.
The CLI talks to the **operator API** at `/api/v1`, and both ride the same OAuth model as
everything else in switchboard (ADR-0019). There is no separate API key, no shared static
token, and no second credential universe.

Verbs are grouped under the resource they manage, so the shape is
`switchboard <resource> <verb>`:

```
switchboard help
switchboard endpoint          # the verbs this resource has
switchboard endpoint revoke -h   # one verb's flags
```

The pre-grouping spellings (`switchboard vend`, `endpoints`, `agents`) still work so existing
scripts keep running, but they are no longer advertised — prefer the grouped form.

Every subcommand and flag, as the binary prints it, is in the
[CLI reference](/guides/cli-reference).

## Logging in (gh-style)

```
switchboard login https://switchboard.example.com
```

What happens, exactly:

1. The CLI discovers switchboard's authorization server from the operator API's
   protected-resource metadata.
2. It registers itself dynamically (RFC 7591) with a loopback redirect —
   `http://127.0.0.1:<ephemeral port>/callback` — as a public PKCE client.
3. Your browser opens switchboard's normal consent screen. The request carries
   `resource = <base>/api`, which makes this an **operator grant**: the token acts as *you*,
   not as any one vended endpoint. The screen says so plainly before you approve.
4. The CLI trades the authorization code (plus its PKCE verifier) for an access/refresh pair
   and saves it, mode 0600, under your user config directory — `~/.config/switchboard/` on
   Linux, `~/Library/Application Support/switchboard/` on macOS, `%AppData%\switchboard\` on
   Windows. Only token plaintexts are stored locally; switchboard persists hashes.

The URL may also come from `$SWITCHBOARD_URL`, or from the saved credentials when you log in
again. `--no-browser` prints the login URL instead of opening a browser (a headless box, an SSH
session); `$BROWSER` names the opener when the platform default is wrong.
`$SWITCHBOARD_CREDENTIALS` relocates the credentials file — one identity per file.

Access tokens are short-lived; the CLI rotates the pair transparently, before expiry or on the
first 401. A refresh the server refuses (revoked grant, changed principal) simply means: log in
again. `switchboard status` shows where you are logged in and when the access token expires.

## Vending the happy path

```
switchboard endpoint vend my-agent --queue inbox
```

One call registers the agent, vends its scoped endpoint (MCP URL + bearer credential), and
mints a token-trust ingestion webhook bound to that queue. The credential is printed **once** —
the same one-time reveal the web UI keeps (SPEC-0007):

```
Vended my-agent (slug my-agent-k3x9) on queue inbox.
This credential is shown ONCE and cannot be recovered — store it now.

  MCP endpoint  https://switchboard.example.com/mcp/my-agent-k3x9
  Bearer token  sbk_…
  Ingest URL    https://switchboard.example.com/webhooks/w/…
                (any producer POSTs here; the unguessable URL is its credential)
  Verbs         list_todos claim claim_next complete fail heartbeat create_webhook …
  Expires       never (valid until revoked)
  Lease         5m (300s) (server default)

Client wiring — paste into your MCP client's .mcp.json:
{
  "mcpServers": {
    "switchboard": {
      "headers": { "Authorization": "Bearer sbk_…" },
      "type": "http",
      "url": "https://switchboard.example.com/mcp/my-agent-k3x9"
    }
  }
}
```

Point the agent's MCP client at the endpoint (the wiring block is ready to paste) and point any
producer at the ingest URL. Deliveries become durable todos and ring the live session's doorbell.

`--json` prints the API's own response instead — the same document any other client would
receive, `mcp_json` included — for scripts and agents. `-q` is the short form of `--queue`, and
flags may come before or after the name.

`--lease-ttl` sets the endpoint's [default claim lease](/guides/vend-an-endpoint#the-default-claim-lease):
what a claim or heartbeat without `lease_ttl_seconds` gets. It takes a duration (`45m`, `1h`) or
whole seconds, from 60 to 86400; without it the server default (300 seconds) applies.

```
switchboard endpoint vend pr-reviewer --queue reviews --lease-ttl 1h
```

## Changing an endpoint's default lease

```
switchboard endpoint edit pr-reviewer-k3x9 --lease-ttl 45m
switchboard endpoint edit pr-reviewer-k3x9 --lease-ttl default   # back to the server default
```

The default claim lease is not scope, so it changes in place, with no re-vend. The next claim or
heartbeat without `lease_ttl_seconds` gets the new value; leases already granted keep their
expiry. The deployment checks the range and prints its refusal (`invalid_argument`) for anything
outside 60 to 86400 seconds. `--json` prints the API response. A revoked endpoint answers a
conflict.

## Listing what you own

```
switchboard endpoint list   # slug, agent, state, queues, expiry, default lease per vended endpoint
switchboard agent list      # your registered agents
switchboard status          # where you are logged in, and whether the credentials are live
switchboard logout          # forget the local credentials
switchboard version         # the build version
```

`endpoint list` deliberately shows **no credentials** — tokens are revealed exactly once, at mint.
Both listings take `--json`. `logout` removes the local file only; your operator *grant* expires on its own schedule, and
switchboard has no RFC 7009 token-revocation endpoint to call yet. That is separate from revoking
a vended endpoint, which is immediate — see below.

## Revoking an endpoint

```
switchboard endpoint revoke my-agent-k3x9
```

Revoking kills the endpoint: its stored credential stops authenticating immediately and any live
MCP session on it is torn down. Name it by the slug `endpoint list` prints, or by its id — both
work. It asks before acting; `-y` skips the prompt for scripts, and `--json` prints the API
response.

This is the rotation path. **A leaked credential is only actually dead once its endpoint is
revoked**, so reach for this the moment a token ends up somewhere it should not be — a log, a
transcript, a pasted command. Revocation is terminal (SPEC-0007: a changed scope means a new
endpoint, never an edited one), so the replacement is a fresh vend:

```
switchboard endpoint revoke leaky-agent-k3x9 -y
switchboard endpoint vend leaky-agent --queue inbox
```

Re-revoking an already-revoked endpoint is a conflict rather than a success, so a rotation script
cannot mistake "it was already dead" for "I killed it just now".

Exit codes follow the usual convention: 0 on success, 1 when the deployment or the credentials
refuse, 2 for a usage mistake (with the command's usage on stderr).

## Handing work to an agent

```
switchboard todo push my-agent-k3x9 "look at PR 7" --queue reviews
switchboard todo push my-agent-k3x9 "re-run the morning brief" --payload @order.json --key brief-2026-09-13
```

`todo push` mints a todo on an endpoint you own and rings its doorbell, the way a verified webhook
delivery does. The hand-off is recorded as a delivery event of trust mode `operator` with your name
on it, so the board and the event history show who asked — it is not an anonymous generic delivery
smuggled in through the ingest URL. `TITLE` is the doorbell's one line; anything longer belongs in
`--payload` (inline JSON, `@file`, or `@-` for stdin), which reaches the agent verbatim. `--queue`
is needed unless the endpoint drains exactly one queue, and must be one of its vended queues.
`--key` makes a push idempotent: the same key on the same endpoint returns the existing todo instead
of minting another, and rings nothing. `--json` prints the API response.

The agent treats what you push exactly as it treats any other todo: content to act on within its
own clamps, never instructions that widen them (ADR-0040).

## Managing webhook routing rules

A webhook's [routing rules](07-routing-rules.md) decide which queue each delivery lands in. Over MCP
only the endpoint that owns the webhook can edit them, and only if it was vended with the rule verbs.
From the CLI you manage them as the human who owns that endpoint, whatever its scope, which is the
way to edit rules on a webhook whose endpoint predates the rule verbs.

```
switchboard webhook list
switchboard webhook rules get WEBHOOK_ID                       # rules, default, params, grant
switchboard webhook rules get WEBHOOK_ID --json > rules.json   # the editable document
$EDITOR rules.json
switchboard webhook rules test WEBHOOK_ID --file rules.json --event 812
switchboard webhook rules set WEBHOOK_ID --file rules.json
```

The full loop — backup, dry-run against stored events and samples, apply, verify, roll back — is
the [Change webhook routing safely](/guides/webhook-routing-safely) how-to.

- `webhook list` shows every webhook your endpoints own, with its endpoint, its source and target
  queue, and how many rules it has. Ingest URLs and signing secrets are never shown.
- `webhook rules get` prints the rules in evaluation order (first match wins), the default action,
  the params, and the `grant`: the queues and endpoints an action may name. Work-order rules are
  marked `WORK ORDER`. `--json` prints the API document, which `rules set --file` accepts unchanged.
- `webhook rules test` dry-runs the candidate rules in `--file` (or, without it, the saved rules)
  against one of the webhook's stored deliveries (`--event`, an id from the event history) or a
  sample body (`--payload`, with `--header "X-GitHub-Event: issues"` for the event kind). It prints
  where the delivery would go and which rule matched, and saves nothing. `FAULTED (blocking)` means
  that delivery would be recorded and routed nowhere.
- `webhook rules set` replaces the whole configuration. It is validated exactly as the MCP verb is
  and dry-run against the webhook's 50 latest deliveries, and a failure (a typo, a queue outside the
  grant, a rule that faults on real traffic) names the problem and keeps the current rules.

**Params are kept unless you say otherwise.** A file without a `params` key keeps the stored params,
and `set` says so. To remove them, put `"params": null` (or `{}`) in the file. The MCP
`set_webhook_rules` currently clears them when `params` is omitted; here it never does, because
clearing a router's params quietly drops every handoff that reads them.

A webhook of another human's endpoint is `not found`, exactly like one that does not exist. A
webhook whose endpoint was revoked stays readable and testable, but `set` answers `409`.

## The API, for other clients

The CLI is a thin client over nine OAuth-guarded endpoints, documented in the site's
[API reference](/api):

| Method | Path | Does |
|--------|------|------|
| `POST` | `/api/v1/endpoints` | Vends agent + endpoint + queue + webhook in one call; the response carries the credential once, plus the ready-to-paste `mcp_json` wiring. |
| `GET` | `/api/v1/endpoints` | Lists your vended endpoints (no credentials). |
| `POST` | `/api/v1/endpoints/{slug\|id}/revoke` | Kills one endpoint: its credential stops authenticating and its live MCP sessions are torn down. `409` if it is already revoked, `404` if it is not yours. |
| `POST` | `/api/v1/endpoints/{slug\|id}/todos` | Hands the endpoint a todo and rings its doorbell: `{title, queue?, kind?, payload?, key?}`. `201` carries the todo, with `created: false` when `key` matched a live one; `400` for a queue outside the vended scope, `409` if the endpoint is revoked, `404` if it is not yours. |
| `GET` | `/api/v1/agents` | Lists your registered agents. |
| `GET` | `/api/v1/webhooks` | Lists every webhook your endpoints own, with its endpoint and a routing summary (no ingest URL, no secret). |
| `GET` | `/api/v1/webhooks/{id}/rules` | The webhook's rules, default, params and grant: `list_webhook_rules`' shape. |
| `PUT` | `/api/v1/webhooks/{id}/rules` | Replaces them: `{rules, default_action?, params?}`. No `params` key keeps the stored params; `"params": null` clears them. `400`/`403` name a rule that fails validation or faults on a recent delivery, `409` if the webhook's endpoint is revoked or the rules changed meanwhile, `503` if the rule evaluator is down. The previous rules stay in force on any failure. |
| `POST` | `/api/v1/webhooks/{id}/rules/test` | Dry run: exactly one of `event_id` or `payload`, plus optional candidate `rules`, `default_action`, `params`, `headers`, `omit_envelope`. Saves nothing. |

Errors on the webhook routes are JSON, `{"error": "…", "code": "…"}`, with the same `code` the
matching MCP verb returns. A webhook you do not own is `404`, the same as an unknown id.

Any HTTP client that can complete the OAuth authorization-code + PKCE flow with
`resource = <base>/api` can act as the operator — that is precisely what the CLI does, and
there is nothing else to it.

## Prefer a browser?

The Endpoints view offers a one-step **quick vend** — name, queues, verbs, lifetime on a
single page with the same one-time reveal — beside the
[full vend wizard](/guides/vend-an-endpoint) for personas and webhook ceilings.
