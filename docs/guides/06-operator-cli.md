---
title: Operator CLI and API
---

# Operator CLI and API

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
  Verbs         list_todos claim complete fail heartbeat create_webhook …
  Expires       never (valid until revoked)

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

## Listing what you own

```
switchboard endpoint list   # slug, agent, state, queues, expiry per vended endpoint
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

## The API, for other clients

The CLI is a thin client over three OAuth-guarded endpoints, documented in the site's
[API reference](/api):

| Method | Path | Does |
|--------|------|------|
| `POST` | `/api/v1/endpoints` | Vends agent + endpoint + queue + webhook in one call; the response carries the credential once, plus the ready-to-paste `mcp_json` wiring. |
| `GET` | `/api/v1/endpoints` | Lists your vended endpoints (no credentials). |
| `POST` | `/api/v1/endpoints/{slug\|id}/revoke` | Kills one endpoint: its credential stops authenticating and its live MCP sessions are torn down. `409` if it is already revoked, `404` if it is not yours. |
| `GET` | `/api/v1/agents` | Lists your registered agents. |

Any HTTP client that can complete the OAuth authorization-code + PKCE flow with
`resource = <base>/api` can act as the operator — that is precisely what the CLI does, and
there is nothing else to it.

## Prefer a browser?

The Endpoints view offers a one-step **quick vend** — name, queues, verbs, lifetime on a
single page with the same one-time reveal — beside the
[full vend wizard](/guides/vend-an-endpoint) for personas and webhook ceilings.
