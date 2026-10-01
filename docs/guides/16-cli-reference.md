---
title: Operator CLI reference
---

# Operator CLI reference

Every `switchboard` subcommand and flag, as the binary prints it. This page tracks
`switchboard help` — when this page and your binary disagree, your binary is older.

For the OAuth model behind these commands (dynamic client registration, the operator grant, where
credentials live), start with [Operator CLI and API](/guides/operator-cli). This page is the
flag-level reference.

```sh
switchboard help          # the command tree
switchboard endpoint -h   # a resource's verbs
switchboard endpoint vend -h   # one verb's flags
```

Verbs are grouped under the resource they manage: `switchboard <resource> <verb>`. The
pre-grouping spellings (`switchboard vend`, `endpoints`, `agents`) still work for existing scripts
but are no longer advertised.

`switchboard serve` is the same binary running the service — see
[Run your own switchboard](/guides/self-hosting).

## Commands at a glance

| Command | What it does |
|---|---|
| `switchboard serve` | Run the service (the default; configured from the environment). |
| `switchboard login [URL]` | Sign in to a deployment over OAuth (opens your browser). |
| `switchboard status` | Show where you are logged in and whether the credentials are live. |
| `switchboard logout` | Forget the local credentials. |
| `switchboard version` | Print the build version. |
| `switchboard endpoint list` | List the vended endpoints you own. |
| `switchboard endpoint vend NAME` | Register an agent and vend its endpoint in one call. |
| `switchboard endpoint edit SLUG\|ID` | Change an endpoint's default claim lease (`--lease-ttl DUR\|default`). |
| `switchboard endpoint revoke SLUG\|ID` | Kill an endpoint; its credential stops working immediately. |
| `switchboard agent list` | List your registered agents. |
| `switchboard todo push ENDPOINT TITLE` | Mint a todo on an endpoint you own and ring its doorbell. |
| `switchboard webhook list` | List the webhooks your endpoints own. |
| `switchboard webhook rules get WEBHOOK_ID` | Show a webhook's rules, default, params and grant. |
| `switchboard webhook rules test WEBHOOK_ID` | Dry-run rules against a stored event or a sample payload; saves nothing. |
| `switchboard webhook rules set WEBHOOK_ID` | Replace a webhook's rules from a file (the `get --json` shape). |

Exit codes, everywhere: `0` success, `1` the deployment or your credentials refused, `2` a usage
mistake (usage printed on stderr).

## Session

### `switchboard login [URL]`

Sign in over OAuth; opens your browser.

| Flag | Meaning |
|---|---|
| `--no-browser` | Print the login URL instead of opening a browser (headless boxes, SSH). |

The URL may also come from `$SWITCHBOARD_URL`, or from the saved credentials when logging in
again. `$BROWSER` names the opener when the platform default is wrong, and
`$SWITCHBOARD_CREDENTIALS` relocates the credentials file — one identity per file.

### `switchboard status`

Show where you are logged in and whether the local credentials are live. No flags.

### `switchboard logout`

Forget the local credentials. The grant itself stays valid until it expires or the deployment
revokes it; logging out only removes the tokens from this machine. No flags.

### `switchboard version`

Print the build version. No flags.

## Endpoints

### `switchboard endpoint list`

List the vended endpoints you own. Credentials are never shown: a token is revealed exactly once,
at vend time. The LEASE column is each endpoint's default claim lease, or the server default.

| Flag | Meaning |
|---|---|
| `--json` | Print the raw API response. |

### `switchboard endpoint vend NAME`

Register an agent and vend its scoped MCP endpoint, its queue, and a token-trust ingestion webhook
in one call. The credential is printed **once** — store it now (see
[Store the credential without printing it](/guides/vend-an-endpoint#store-the-credential-without-printing-it)).

| Flag | Meaning |
|---|---|
| `--queue` (short `-q`) | The queue the endpoint drains and the webhook feeds. Default `inbox`. |
| `--lease-ttl` | The endpoint's default claim lease: a duration (`45m`, `1h`) or whole seconds, 60 to 86400. What a claim or heartbeat without `lease_ttl_seconds` gets. Default: the server's 300 seconds. |
| `--json` | Print the raw API response. |

### `switchboard endpoint edit SLUG|ID`

Change an endpoint's default claim lease without re-vending it. New claims and heartbeats that pass
no `lease_ttl_seconds` get it; leases already granted keep their expiry. Names by the slug
`endpoint list` prints, or by the id.

| Flag | Meaning |
|---|---|
| `--lease-ttl` | Required. A duration (`45m`, `1h`) or whole seconds, 60 to 86400; `default` resets it to the server default. The deployment refuses a value out of range. |
| `--json` | Print the raw API response. |

### `switchboard endpoint revoke SLUG|ID`

Kill an endpoint: its stored credential stops authenticating immediately and any live MCP session
on it is torn down. This cannot be undone — vend a new endpoint instead. Names by the slug
`endpoint list` prints, or by the id.

| Flag | Meaning |
|---|---|
| `-y` | Skip the confirmation prompt. |
| `--json` | Print the raw API response. |

## Agents

### `switchboard agent list`

List your registered agents.

| Flag | Meaning |
|---|---|
| `--json` | Print the raw API response. |

## Todos

### `switchboard todo push ENDPOINT TITLE`

Hand a todo to an endpoint you own and ring its doorbell, the way a verified delivery does. Name
the endpoint by the slug `endpoint list` prints, or by its id. `TITLE` is the doorbell's one line;
put the detail in `--payload`.

| Flag | Meaning |
|---|---|
| `--queue` (short `-q`) | The queue to put it on. Needed unless the endpoint drains exactly one; must be one of its vended queues. |
| `--payload` | JSON handed to the agent with the todo: inline, `@file`, or `@-` for stdin. |
| `--kind` | The todo's kind, as `list_todos` reports it. Default: operator. |
| `--key` | Idempotency key: the same key on the same endpoint returns the existing todo. |
| `--json` | Print the raw API response. |

## Webhooks

### `switchboard webhook list`

List the webhooks your endpoints own, with their routing-rule counts. Ingest URLs and signing
secrets are never shown: they are revealed once, at create or rotate.

| Flag | Meaning |
|---|---|
| `--json` | Print the raw API response. |

### `switchboard webhook rules get WEBHOOK_ID`

Show a webhook's routing rules in evaluation order (first match wins), its default action, its
params, and the grant: the queues and endpoints its rules may name.

| Flag | Meaning |
|---|---|
| `--json` | Print the raw API response — a document `webhook rules set --file` accepts as it stands. |

### `switchboard webhook rules test WEBHOOK_ID`

Dry-run routing: evaluate candidate rules (`--file`), or the saved ones, against one of the
webhook's stored deliveries (`--event`) or a sample payload (`--payload`), and print where it
would go and why. Saves nothing. A `FAULTED (blocking)` result means that delivery would be
recorded and routed nowhere. Exactly one of `--event` or `--payload`.

| Flag | Meaning |
|---|---|
| `--event` | Route this stored delivery of the webhook (its event id). |
| `--payload` | Route this sample body instead: a path, `@path`, or `-` for stdin. |
| `--header` | A sample request header for `--payload`, `Name: value` (repeatable; e.g. `X-GitHub-Event: issues`). |
| `--file` (short `-f`) | Candidate rules document (the `rules get --json` shape): a path, `@path`, or `-` for stdin. Omit to test the saved rules. |
| `--envelope` | Also print the envelope the rules evaluated. |
| `--json` | Print the raw API response. |

### `switchboard webhook rules set WEBHOOK_ID`

Replace a webhook's whole routing configuration from a file:
`{"rules": [...], "default_action": {...}, "params": {...}}` — the shape
`webhook rules get --json` prints. The save is validated, dry-run against the webhook's 50 latest
deliveries, and refused (keeping the current rules) if anything fails. A file without a `params`
key **keeps** the stored params; `"params": null` clears them.

| Flag | Meaning |
|---|---|
| `--file` (short `-f`) | The rules document: a path, `@path`, or `-` for stdin. Required. |
| `--json` | Print the raw API response. |

For the safe way to run this loop end to end — backup, dry-run, apply, verify, roll back — see
[Change webhook routing safely](/guides/webhook-routing-safely). Rule syntax is
[routing rules](/guides/routing-rules); tested rule recipes are in the
[routing cookbook](/guides/routing-cookbook).
