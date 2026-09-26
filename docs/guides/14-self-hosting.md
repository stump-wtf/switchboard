---
title: Run your own switchboard
---

# Run your own switchboard

Switchboard is a single Go binary and a PostgreSQL database. This guide takes you from nothing to a
running instance with a verified webhook → todo → agent loop.

Every command here was run against a fresh database while writing it, and the outputs are the real
ones. The one step that cannot be run for you is registering an OIDC client in your identity
provider, since that happens in software this project doesn't ship — that section says exactly what
switchboard needs and how to tell it worked.

## What you need

| | Requirement | Notes |
|---|---|---|
| **Database** | PostgreSQL | The schema uses `gen_random_uuid()` and identity columns, so **13 or newer**. Verified on 16; the project's own CI runs 18, and the Docker path below brings its own. Switchboard creates and migrates its own schema at startup — 24 migrations as of this writing. |
| **Identity provider** | Any OIDC provider | Needed for real logins, because every endpoint is vended by an accountable human. See [Sign-in](#sign-in-oidc). |
| **TLS** | A reverse proxy | Switchboard speaks plain HTTP and expects something in front terminating TLS. |

There is no external secret manager, no message broker requirement, and no sidecar. One process, one
database.

## Configuration

Everything comes from the environment; `serve` takes no flags.

| Variable | Default | Purpose |
|---|---|---|
| `SWITCHBOARD_DATABASE_URL` | — | **Required.** PostgreSQL DSN. |
| `SWITCHBOARD_BASE_URL` | `http://127.0.0.1:8080` | **Set this.** The externally reachable base URL, with no trailing slash. It builds the OIDC redirect URL, the vended MCP endpoint URLs, and the webhook ingest URLs you hand to producers. |
| `SWITCHBOARD_ADDR` | `127.0.0.1:8080` | Listen address. The container image sets `0.0.0.0:8080`. |
| `SWITCHBOARD_OIDC_ISSUER` | — | Issuer URL. Discovery runs at startup. |
| `SWITCHBOARD_OIDC_CLIENT_ID` | — | |
| `SWITCHBOARD_OIDC_CLIENT_SECRET` | — | |
| `SWITCHBOARD_OIDC_REDIRECT_URL` | `<base>/auth/callback` | Override only if your proxy rewrites paths. |
| `SWITCHBOARD_GITHUB_CLIENT_ID` | — | Optional second login provider (GitHub OAuth, ADR-0026). When set with its secret, the login page gains "Log in with GitHub". |
| `SWITCHBOARD_GITHUB_CLIENT_SECRET` | — | |
| `SWITCHBOARD_GITHUB_REDIRECT_URL` | `<base>/auth/callback` | Override only if your proxy rewrites paths. |
| `SWITCHBOARD_SECRET_ENCRYPTION_KEY` | — | Recommended. Encrypts webhook signing secrets at rest. 32 bytes, base64 or hex. |
| `SWITCHBOARD_DEV_LOGIN` | off | Unauthenticated local login. Never in production. |
| `SWITCHBOARD_METRICS_TOKEN` | — | Scrape token for `GET /metrics`. Unset, the endpoint answers `401` to everything. At least 32 bytes. See [Metrics](#metrics). |
| `SWITCHBOARD_FRIENDING`, `SWITCHBOARD_PERSONAS`, `SWITCHBOARD_A2A`, `SWITCHBOARD_A2UI` | off | Advanced capabilities, hidden until switched on. |
| `SWITCHBOARD_ATTEMPT_SUMMARY_FROM_RESULT` | off | Set to `true` so a `complete` or `fail` with no `summary` stores the compact JSON of its `result` as the attempt summary. Off because clients written before attempt history never expected anyone else to read `result`: turning it on replays it to every later claimer in `prior_attempts` and `get_todo`. See [Attempt history](/guides/attempt-history#summary-from-result). |

A few limits live in the database's `settings` table rather than the environment: retention
(`retention_max_age_days`, 30 by default, and `retention_max_rows`, 500000) and the per-todo attempt
history cap (`attempt_history_max_per_todo`, 50 by default and never below 5). They are read while
the service runs, so a change needs no restart. See
[Attempt history](/guides/attempt-history#retention).

One setting is easy to get wrong: **`SWITCHBOARD_BASE_URL` decides whether session cookies are
marked `Secure`.** Switchboard sets that flag when the base URL starts with `https://`. Behind TLS,
the base URL must say `https://` or you serve session cookies without the flag.

Generate an encryption key with either of these:

```bash
openssl rand -base64 32     # 44 characters
openssl rand -hex 32        # 64 characters
```

## Get the source

Switchboard is MIT licensed and the repository is public at
https://github.com/stump-wtf/switchboard:

```bash
git clone https://github.com/stump-wtf/switchboard.git
```

Published container images for `linux/amd64` and `linux/arm64` live at
`ghcr.io/stump-wtf/switchboard`, built on every `v*` tag. Everything below works from a clone, and
the Docker path works from the image alone — no checkout needed.

Dependencies are vendored in the repository, so the build needs no network and no module downloads.
Don't run `go mod tidy`: it can rewrite `go.mod` and `vendor/`, which is exactly the hermetic
property the vendoring exists to guarantee.

## Start it

There are two paths. Docker is the shorter one and brings its own PostgreSQL; the binary path suits
an existing database or a systemd unit.

### With Docker

Fastest path — the published image, no checkout:

```bash
curl -fsSL -o compose.yaml https://raw.githubusercontent.com/stump-wtf/switchboard/main/deploy/docker/compose.yaml
docker compose up -d
```

That starts PostgreSQL alongside switchboard (image `ghcr.io/stump-wtf/switchboard:latest`,
multi-arch `linux/amd64` + `linux/arm64`), waiting for the database to pass its health check first.
The service is published on `127.0.0.1:8080`, and `SWITCHBOARD_BASE_URL` defaults to
`http://127.0.0.1:8080`.

If you would rather build from source, clone the repo and, from `deploy/docker/`:

```bash
docker compose build
docker compose up -d
```

That builds the image from the vendored source and runs the same stack.

```bash
docker compose logs switchboard
```

```
level=INFO msg="database ready"
level=INFO msg="switchboard listening" addr=0.0.0.0:8080 base_url=http://127.0.0.1:8080 oidc=false dev_login=false
level=INFO msg="listening for todo_ready wakeups" channel=todo_ready
```

`curl http://127.0.0.1:8080/login` then returns the login page. With no OIDC configured it says
"No login configured", which is the expected state until you do the
[OIDC setup](#sign-in-oidc) below — set the `SWITCHBOARD_OIDC_*` variables in the compose
environment, along with a real `SWITCHBOARD_BASE_URL` and a
`SWITCHBOARD_SECRET_ENCRYPTION_KEY`, and recreate the service.

The compose file is a starting point, not a production deployment: the database password is
`change-me`, the database volume is local, and there is no TLS. Read
[Behind a reverse proxy](#behind-a-reverse-proxy) before exposing it.

`docker compose down` stops it; `docker compose down -v` also deletes the database volume.

### As a binary

```bash
go build -o switchboard ./cmd/switchboard

export SWITCHBOARD_DATABASE_URL='postgres://switchboard:secret@localhost:5432/switchboard?sslmode=disable'
export SWITCHBOARD_BASE_URL='https://switchboard.example.com'
export SWITCHBOARD_ADDR='127.0.0.1:8080'
export SWITCHBOARD_SECRET_ENCRYPTION_KEY="$(openssl rand -base64 32)"
./switchboard serve
```

A healthy start looks like this — the schema is created and migrated on first run, so there is no
separate migrate step:

```
level=INFO msg="database ready"
level=INFO msg="webhook signing secrets encrypted at rest"
level=INFO msg="switchboard listening" addr=127.0.0.1:8080 base_url=https://switchboard.example.com oidc=true dev_login=false
level=INFO msg="listening for todo_ready wakeups" channel=todo_ready
```

Check the `oidc=` and `dev_login=` values on that line: they tell you which login paths are live.

`sslmode=disable` above assumes the database is on the same host or a private network. Across a
network, use `sslmode=verify-full`.

### When it won't start

| Message | Cause |
|---|---|
| `db: empty DATABASE_URL` | `SWITCHBOARD_DATABASE_URL` is unset. |
| `db: ping: … connection refused` | The DSN is right but nothing is listening. |
| `cred: secret encryption key must decode (base64 or hex) to exactly 32 bytes` | The key is the wrong length or encoding. |
| `config: SWITCHBOARD_METRICS_TOKEN is N bytes; it must be at least 32 …` | The scrape token is too short. It must also be printable ASCII with no spaces. |

Each of these exits immediately and says which one it is.

### Behind a reverse proxy

Two rules matter more than the rest:

- **Do not buffer `/mcp/*`.** A vended MCP endpoint's `GET` is a long-lived notification stream. A
  buffering proxy holds doorbells until the connection closes. Caddy:
  `reverse_proxy … { flush_interval -1 }`. nginx: `proxy_buffering off;`.
- **Pass a trusted `X-Forwarded-For`.** The pre-auth rate limiter keys on the client IP, and with
  TLS terminated upstream every agent otherwise arrives from one address and shares a bucket.

Switchboard is a complete OIDC relying party with its own login, so don't put forward-auth in front
of it.

## Sign-in (OIDC)

Register switchboard as a confidential client in your provider:

- **Redirect URI:** `<SWITCHBOARD_BASE_URL>/auth/callback`
- **Grant:** authorization code, with PKCE
- **Scopes:** `openid`, plus whatever your provider needs for name and email

Then set the three `SWITCHBOARD_OIDC_*` variables and restart.

Switchboard trusts the issuer wholesale and enforces no assurance claim, which is only appropriate
for a provider you control and would trust for admin access. **Users are created on first
sign-in**, keyed on the OIDC subject, so whoever can authenticate at your provider can sign in here.
Restrict access at the provider.

To confirm it worked: open `/login`. With OIDC configured you get a login button; with nothing
configured you get "No login configured", and `/auth/login` answers:

```
503 OIDC not configured (set SWITCHBOARD_OIDC_* or SWITCHBOARD_DEV_LOGIN=1)
```

`SWITCHBOARD_DEV_LOGIN=1` mints a session for a fixed local user with no authentication at all. It
is for trying the loop on a laptop, never for a deployment.

## Verify the whole loop

Do this once, on a fresh instance. It takes a few minutes and proves ingestion, verification,
routing, and the agent surface all work together.

**1. Vend an endpoint.** Sign in, open **Endpoints**, and use **+ vend endpoint**. Give it a queue
(`inbox`), the six todo verbs plus `create_webhook`, and a webhook allowance of 1 or 2 with source
type `github`. The reveal shows the MCP URL and a `sbk_…` credential **once**.

**2. Create a webhook.** From an MCP client connected to that endpoint, or any HTTP client speaking
MCP, call `create_webhook`:

```json
{"source_type": "github", "target_queue": "inbox"}
```

You get back an `ingest_url` and a `signing_secret` (`whsec_…`), the secret shown only this once.

**3. Send a signed delivery.**

```bash
BODY='{"action":"opened","issue":{"number":1,"title":"self-host check","user":{"login":"you"}},"sender":{"login":"you"}}'
SIG="sha256=$(printf %s "$BODY" | openssl dgst -sha256 -hmac "$SIGNING_SECRET" | awk '{print $NF}')"

curl -sS -X POST "$INGEST_URL" \
  -H 'Content-Type: application/json' \
  -H 'X-GitHub-Event: issues' \
  -H "X-GitHub-Delivery: $(uuidgen)" \
  -H "X-Hub-Signature-256: $SIG" \
  -d "$BODY"
```

Expect `202` and a body naming the todo it created:

```json
{"todos":[{"id":"td_…","queue":"inbox","created":true}],"created":1,"trust_mode":"signed","verified":true}
```

**4. Prove verification is real.** Send the same body with no signature header:

```json
401 {"error":"signature verification failed"}
```

Nothing is stored for a rejected delivery.

**5. Work the todo.** `list_todos` with `{"queue": "inbox", "state": "pending", "limit": 5}` returns
it, with `attempt: 0` and `max_attempts: 5`. `claim` takes it under a lease (`state: claimed`,
`attempt: 1`, a `lease_expires_at`), and `complete` finishes it (`state: done`).

If all five steps behave that way, the instance is working: signatures are enforced, todos are
durable, and agents can drain them.

### Two things that look like bugs

- **A repeated identical body collapses.** Deliveries dedupe on the producer's delivery id, or on a
  hash of the body when there is none. Posting the same JSON twice with plain `curl` returns
  `"created": 0` and the *first* todo's id — it isn't lost, it's the same work item. Send a unique
  `X-GitHub-Delivery` per delivery, as real producers do.
- **The CLI says nothing is there before you log in.** `switchboard endpoint list` prints
  ``not logged in — run `switchboard login <URL>` first``.

## Routing rules: two live defects

Routing rules are optional — without any, deliveries land on the webhook's target queue. If you
write them, know these two before you write a trust rule, because both are unfixed and both fail in
the dangerous direction.

**Rules fail open.** A rule whose jq expression errors is recorded as a fault and treated as *no
match*. For a drop rule that means it stops dropping. A trust rule that reads a parameter of the
wrong type doesn't fail closed — it lets everything past. Defend by hand: read list parameters
through `arrays` and string parameters through `strings`, so a missing or mistyped value becomes an
empty list rather than an error:

```json
{"id": "untrusted", "expr": "(($params.trusted | arrays) // []) as $t | ((.payload.sender.login // \"\") as $who | any($t[]; . == $who) | not)", "action": {"drop": true}}
```

**Saving rules without `params` clears them.** `set_webhook_rules` replaces rules, default action
and parameters together. Omit `params` and your allowlists are gone — combined with the above, a
trust rule then matches nobody or everybody depending on how it is written. Always send `params`
with the rules, and read them back with `list_webhook_rules`.

The [routing cookbook](/guides/routing-cookbook) has tested recipes that already follow both rules.

## What an endpoint can do

An endpoint is the whole capability grant: an MCP URL plus a credential, scoped at vend time to a
set of queues, a list of tools, a webhook allowance, and a lifetime.

**The endpoint's scope is the only capability boundary.** It cannot be widened after vending — there
is deliberately no verb for that — and no client-side configuration, skill, or prompt changes what it
may do. To change what an agent can do, vend a new endpoint and revoke the old one. Revocation is
immediate: the credential stops authenticating and live sessions are torn down.

Grant the smallest useful scope. The event-history tools in particular should go only to endpoints
whose job needs them.

## Metrics

`GET /metrics` serves Prometheus text format (`text/plain; version=0.0.4`) for VictoriaMetrics,
Prometheus, or anything else that scrapes it. It covers queue liveness, the todo lifecycle, webhook
deliveries and routing decisions, plus the Go runtime and process collectors. The
[metrics spec](/specs/metrics/spec) names every series, and
[ADR-0028](/decisions/ADR-0028-prometheus-metrics-endpoint) explains why the queue gauges lead.

The endpoint is closed until you give it a scrape token:

```bash
export SWITCHBOARD_METRICS_TOKEN="$(openssl rand -hex 32)"
```

The scrape token is a credential of its own. A vended endpoint token, an operator OAuth token, and a
signed-in browser session all get the same answer as no credential at all: `401` with an empty
body, so an unauthenticated prober learns no queue names. With the variable unset, every request
gets `401`. A token shorter than 32 bytes, or one containing spaces or non-ASCII characters, stops
the service at startup. Surrounding whitespace is trimmed, so a token read from a file that ends in
a newline still works. The route is throttled per client address at 1 request per second with a
burst of 20, far above any real scrape interval.

Check it by hand:

```
$ curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8080/metrics
401
$ curl -s -H "Authorization: Bearer $SWITCHBOARD_METRICS_TOKEN" http://127.0.0.1:8080/metrics | grep '^go_goroutines'
go_goroutines 14
```

A scrape job with bearer auth (Prometheus and vmagent read the same block):

```yaml
scrape_configs:
  - job_name: switchboard
    scheme: https
    metrics_path: /metrics
    scrape_interval: 30s
    authorization:
      type: Bearer
      # Keeps the token out of the config file. `credentials: <token>` inline also works.
      credentials_file: /etc/prometheus/secrets/switchboard-metrics-token
    static_configs:
      - targets: ["switchboard.example.com"]
```

`switchboard_todo_attempts_closed_total{queue,outcome}` counts how todo attempts end. Its
`lease_expired` and `reaped` outcomes are deaths, and each also counts once in
`switchboard_todo_leases_expired_total`. See [Attempt history](/guides/attempt-history#metrics).

A series appears only once there is something to count. A counter nothing has incremented yet is
absent rather than zero, because a zero and an unmeasured value look identical once scraped.

## Where to go next

- [Concepts in five minutes](/getting-started/concepts) — the model your users will work in.
- [Upgrading](/guides/upgrading) — read this before moving between releases. It names the breaking
  changes, and anything a release cannot undo.
- [Vend an endpoint](/guides/vend-an-endpoint) — the full scope surface.
- [Receive your first webhook](/getting-started/first-webhook) — GitHub, Gitea, Cairn, and signing
  your own producer.
- [Routing cookbook](/guides/routing-cookbook), [Working the queue well](/guides/working-the-queue),
  [Security model](/guides/security-model), and [Troubleshooting](/guides/troubleshooting).
