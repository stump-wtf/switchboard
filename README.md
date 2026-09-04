# Switchboard

*Many lines come in. The operator verifies each caller, and patches it through.*

Switchboard is the operator's board for your inbound webhooks. It receives events from external
providers (GitHub, Stripe, Slack, Docker Hub, and self-hosted/homelab senders), verifies and
normalizes each one, stores them in PostgreSQL, and patches them through to **two consumers of the same
backend**:

- **MCP clients** (Claude Code, other agents) — via MCP tools (`list` / `get` / `replay` / `list_providers`) and a recent-events resource.
- **A human** — via a small, local-only web UI (4 screens) that updates live over Server-Sent Events.

A fourth incoming line — a **Redis queue consumer** — feeds the same pipeline without any HTTP
endpoint, proving the abstraction generalizes beyond HTTP webhooks.

The name is the architecture: a manual telephone exchange took many incoming lines, an operator
verified the caller, and patched the line through to its destination. That's exactly this — and it's
why the UI and docs wear a switchboard-era palette (brass, bakelite, operator-cream, oxblood, and
patch-cable tones — see [`static/tokens.css`](static/tokens.css) and
[ADR-0000](docs/adrs/ADR-0000-project-naming-and-scope.md)).

> [!IMPORTANT]
> **Status: MVP working.** The design record — the **architecture decision records** (`docs/adrs/`) and
> the **specs** (`docs/openspec/specs/`) — is served as a compiled static site at [switchboard.stump.wtf/docs](https://switchboard.stump.wtf/docs/)
> and remains the source of truth. The **MVP is implemented and verified end-to-end**: OIDC login
> (Pocket ID RP) → register an agent → **vend a scoped MCP endpoint** → the agent connects **directly
> over Streamable HTTP** (`type: "http"` `.mcp.json`: minted URL + bearer credential — no local
> binary, subprocess, or stdio adapter), which serves the todo work-tools and **pushes new todos into
> a live session as channel doorbells**, backed by the durable PostgreSQL queue (ADR-0017; SPEC-0014).
> A signed GitHub webhook is the reference ingestion path. Start at the design index:
> [`docs/README.md`](docs/README.md).

## Two layers

- **Event-store core (ADR-0000–005):** receive, verify, persist, and expose inbound webhooks/queue
  events — the pipeline described in this README.
- **Agent layer (ADR-0007–015):** inbound events become durable **todos** that agents claim and
  complete; humans register agents and are vended scoped MCP endpoints; personas are advertised as A2A
  Agent Cards; and cross-agent work is granted by human-approved friending. See
  [ADR-0007](docs/adrs/ADR-0007-todos-as-core-primitive.md) and the
  [design index](docs/README.md).

## Why this exists

Different providers have wildly different security stories, and pretending otherwise is a security
bug. `switchboard` makes each source's trust level **explicit, per-provider, enforced, and visible** —
a signed GitHub event and a token-authenticated Docker Hub event are never displayed or exposed as if they
were the same thing. See [ADR-0003](docs/adrs/ADR-0003-per-provider-ingestion-and-trust-model.md).

## Architecture

```
Provider (GitHub/Stripe/Slack/Docker/…)        Redis (pub/sub or stream)
      │  HTTPS POST + signature header               │  in-process consumer subscribes
      ▼                                               ▼
[Go app: /webhooks/{provider}]        [Redis consumer task, in-process]
      │  verify signature → normalize                │  normalize (trust = Redis ACL/TLS,
      │  (or: generic/no-verify path)                │   no HTTP signature concept)
      ▼                                               ▼
   PostgreSQL (events + todo queue) ◄─────────────────────┘
      │
      ├──► MCP tools/resources  (list / get / replay / list_providers)
      ├──► SSE broadcast (/events) ──► Web UI (html/template + HTMX + Pico.css)
      └──► retention pruning (age + row-cap)
```

One service, single repo. The MCP server and the web server share the same Go HTTP server
(different route groups) and the same PostgreSQL layer. Stack rationale — net/http + chi over a framework, HTMX
over a SPA, Pico over Tailwind, inline SVG over icon fonts — is in
[ADR-0001](docs/adrs/ADR-0001-web-stack-go-htmx-pico.md).

## Trust model at a glance (ADR-0003)

Two provider families — **webhook** (push) and **queue** (pull) — and every event's trust level is
explicit and shown.

| Family · `trust_mode` | Providers | How it's trusted | On failure |
|------------------------|-----------|------------------|-----------|
| webhook · **signed** | GitHub, Stripe, Slack | Mandatory HMAC verification of the body (constant-time; Stripe/Slack also enforce a timestamp window). Integrity + authenticity. | **401, payload NOT persisted**, redacted rejection logged |
| webhook · **token** | Docker Hub, homelab/self-hosted | **No signing scheme exists.** A configured **shared-secret** the caller presents — `Authorization: Bearer` (or a header), or a `?token=` URL fallback for URL-only senders like Docker Hub. Authenticates the *caller*, **not** the body; no replay protection. Required by default. | 403 on missing/bad token |
| webhook · **open** | senders that can't present any secret | No check. **Off by default**, trusted-network only, loudest-labeled. Prefer `token`. | n/a (accepted, labeled `open`) |
| queue · **queue** | Redis (reference), SQS/NATS/AMQP later | No per-message signature; trust is the **broker connection** (auth/ACL + TLS) — "who may publish to this queue." | connection-level |

Docker Hub has no native webhook signing, so it is a **token** webhook (URL token) rather than a faked
"signed" one — inventing verification where none exists would make the `signed` badge meaningless for
every other provider. A shared-secret **token** is a real tier *between* `signed` and `open`: it proves
the caller knows a secret, but unlike HMAC it can't attest the payload.

## Documentation

| Doc | What it covers |
|-----|----------------|
| [ADR-0000](docs/adrs/ADR-0000-project-naming-and-scope.md) | Project name + MVP/session scope |
| [ADR-0001](docs/adrs/ADR-0001-web-stack-go-htmx-pico.md) | Web/UI stack (Go net/http + chi, html/template, HTMX + Pico) — and why not a framework / Tailwind / icon fonts |
| [ADR-0002](docs/adrs/ADR-0002-postgres-persistence-and-retention.md) | PostgreSQL persistence, queue mechanics, schema sketch, retention |
| [ADR-0003](docs/adrs/ADR-0003-per-provider-ingestion-and-trust-model.md) | Ingestion provider types (webhook / queue) & the trust model (signed / token / open / queue) |
| [ADR-0005](docs/adrs/ADR-0005-mcp-tool-and-resource-contract.md) | MCP tool/resource contract shape |
| [openapi.yaml](docs/reference/openapi.yaml) | HTTP surface: webhook ingestion + web-UI endpoints |
| [asyncapi.yaml](docs/reference/asyncapi.yaml) | SSE event/message schema |
| [SPEC-0005 mcp-tools](docs/openspec/specs/mcp-tools/spec.md) | MCP tool + resource contract & JSON Schemas |
| [SPEC-0014 mcp-transport](docs/openspec/specs/mcp-transport/spec.md) | Vended MCP endpoints served over Streamable HTTP (`/mcp/{endpoint}`) |

## Web UI (4 screens)

1. **Dashboard / status** — live connection strip (`● receiving` / `○ idle`), recent event count per provider, last-event timestamp per provider.
2. **Webhook log** — paginated table (provider icon, event type, timestamp, verify status, size); row → detail (raw payload, sanitized headers, verification result).
3. **Provider config** — each provider's type, path/channel, secret status (`configured` / `missing` / `none-by-design` — never the secret itself), enable/disable toggle.
4. **Settings** — retention policy (age + row cap), SSE reconnect behavior, general config.

## The basics (ADR-0023 MVP): webhook → todo → doorbell

The MVP is MCP/API-first. One call registers an agent and vends the whole loop —
the agent endpoint, its scoped queue, and an ingestion webhook — and every verified
delivery becomes a durable todo that rings the agent's live MCP session:

```bash
# 1. Log in as the operator (gh-style OAuth in your browser; credentials saved locally).
go run ./cmd/switchboard login http://127.0.0.1:8080

# 2. Register: one call vends agent + endpoint + queue + webhook, and prints the credential
#    ONCE — with a ready-to-paste .mcp.json block. (--json prints the raw API response.)
go run ./cmd/switchboard endpoint vend my-agent --queue inbox
```

The CLI (and any API client) authenticates with an OPERATOR OAuth grant — the same
authorization-code + PKCE flow MCP clients use, with `resource = <base>/api` — so there is no
static shared API token: auth is OAuth everywhere (ADR-0019/ADR-0023). Verbs are grouped under
the resource they manage — `endpoint list` / `endpoint vend` / `endpoint revoke`, `agent list` —
alongside the session verbs `login`, `status`, `logout`, `version`; `switchboard help` lists them.
Prefer a browser? The Endpoints view offers a one-step quick vend at `/endpoints/quick`.

Point the agent's MCP client at `mcp_url` with `token` as the bearer credential,
point any producer at `ingest_url`, and done: deliveries become todos on `inbox`
and the live session receives a `notifications/claude/channel` doorbell (a
disconnected agent finds the todo on its next `list_todos`). The full loop is
proven end to end in CI (`TestMVPRegistrationToDoorbell`).

**Advanced capabilities are hidden by default** and stay in the codebase behind
flags: personas (`SWITCHBOARD_PERSONAS=1`), the A2A protocol surface
(`SWITCHBOARD_A2A=1`), and the A2UI resources (`SWITCHBOARD_A2UI=1`). Friending
remains gated by `SWITCHBOARD_FRIENDING=1`. See ADR-0023.

## Running it

Switchboard needs PostgreSQL. Point it at a database and run the service:

```bash
make build                                   # compile ./bin/switchboard (assets embedded)
export SWITCHBOARD_DATABASE_URL='postgres://user@127.0.0.1:5432/switchboard?sslmode=disable'
export SWITCHBOARD_OIDC_ISSUER=https://pocket-id.example \
       SWITCHBOARD_OIDC_CLIENT_ID=… SWITCHBOARD_OIDC_CLIENT_SECRET=…
./bin/switchboard serve                      # web UI + /webhooks/* + /mcp/{endpoint} on 127.0.0.1:8080
```

Migrations apply on startup. For a local spin without a real Pocket ID, set `SWITCHBOARD_DEV_LOGIN=1`
(loud, dev-only) to log in and vend.

**The vend → work loop (the MVP):**

1. Log in, register an agent, and **vend a scoped endpoint** (queues + verbs). The vend page shows the
   credential once, plus a ready-to-paste `.mcp.json` — a Streamable-HTTP MCP server block, no local
   command:
   ```json
   {"mcpServers":{"switchboard":{"type":"http","url":"https://<host>/mcp/<slug>","headers":{"Authorization":"Bearer <credential>"}}}}
   ```
2. Drop that `.mcp.json` into your project and start your MCP client (e.g. Claude Code). The client
   connects **directly over HTTP/S** to `/mcp/<slug>` — there is no binary to install or put on PATH.
   The endpoint serves the work tools (`list_todos` / `claim` / `complete` / `fail` / `heartbeat`) and
   pushes new todos into the session as `notifications/claude/channel` doorbells on the notification
   stream (ADR-0017; SPEC-0014).
3. Send a signed webhook (`POST /webhooks/github`) — or, in dev mode, `POST /dev/todos` — and the todo
   arrives in your session.

The service is **loopback-bound by default and ships no in-app auth**. If you ever expose it on the
homelab LAN it **must** sit behind Caddy `forward_auth`, like everything else in the stack — auth is
the reverse proxy's job, not this app's (ADR-0001, brief §8).

Secrets (provider HMAC secrets, the Postgres/Redis DSNs, generic tokens, the OIDC client secret) are
injected via **environment/deployment config** — never committed. Switchboard-*minted* secrets (agent
credentials and agent-created webhook signing secrets) are generated by switchboard and stored
**hashed** in PostgreSQL.

### Pointing a provider at this service (for testing)

Because the service is localhost-bound, expose it to a provider during testing with a tunnel
(e.g. `cloudflared tunnel`, `tailscale funnel`, or an SSH reverse tunnel), then set the provider's
webhook URL to the tunnel's public URL + the provider path:

- GitHub → `https://<tunnel>/webhooks/github`
- Stripe → `https://<tunnel>/webhooks/stripe`
- Slack → `https://<tunnel>/webhooks/slack`
- Docker Hub (token) → `https://<tunnel>/webhooks/generic/dockerhub?token=<shared-token>`

Set the corresponding signing secret in the environment/config first,
or the signed endpoint will (correctly) 401.

## Adding a new provider (intended shape)

1. **Signed provider:** add an adapter (`<name>.go`) implementing the verification
   for its signature scheme (raw-body HMAC, constant-time compare, timestamp window if the scheme
   signs one), register it with `trust_mode=signed`, and set its secret in the environment/config.
2. **Unsigned / homelab sender:** don't write an adapter — create a **generic** provider
   (`/webhooks/generic/<name>`), which requires a shared-secret token (or explicit `open`) and is disabled until you opt in.
3. **Queue source:** point the Redis consumer at another channel/stream; the trust boundary is that
   channel's Redis ACL.

The trust mode is always declared per provider and shown in the UI — never silently assumed. See
[ADR-0003](docs/adrs/ADR-0003-per-provider-ingestion-and-trust-model.md).

## Development

```bash
make ci     # the gate: go vet + go test ./... + go build — the local mirror of CI
make fmt    # gofmt -w .
make vet    # go vet ./...
make test   # go test ./...
make lint   # golangci-lint (optional; not part of `make ci`)
```

### Database-backed tests

Most unit tests run without a database. The store/ingest/server integration tests need a real
Postgres and skip cleanly when one is not configured — point them at a database with
`SWITCHBOARD_TEST_DATABASE_URL` (a superuser DSN, since each package provisions its own database on
first use):

```bash
export SWITCHBOARD_TEST_DATABASE_URL='postgres://postgres@127.0.0.1:5432/postgres?sslmode=disable'
go test ./...
```

Each DB-backed package derives its **own** database from that DSN rather than sharing one, so
`go test ./...` is safe at the default (parallel) parallelism — no `-p 1` required. Every package's
setup helper truncates between tests, and running them concurrently against a single shared database
would race those truncations and drop another package's rows mid-run. The isolation avoids that:

| Package | Test database |
|---|---|
| `internal/store` | `switchboard_test_store` |
| `internal/ingest` | `switchboard_ingest_test` |
| `internal/server` | `switchboard_test_server` |
| `internal/db` (migrations) | a throwaway `sb_migrate_test_*` per test, dropped on cleanup |

Each package `CREATE DATABASE`s its target on first use (tolerating the `42P04 duplicate_database`
from a prior run), migrates it, and truncates between tests. The databases persist across runs; drop
them with `DROP DATABASE switchboard_test_store` (etc.) if you want a clean slate.

The docs site builds with Docusaurus and deploys to **GitHub Pages** via `.github/workflows/pages.yml`.

## Repository hosting

- **Source:** <https://github.com/joestump/switchboard>
- **Docs:** built with Docusaurus and served as a compiled static site at <https://switchboard.stump.wtf/docs/> — the front Caddy routes `/docs/*` to the `switchboard-docs` container (built + pushed by `.gitea/workflows/docs.yaml`). GitHub Pages was retired: a private org repo on the Team plan cannot serve Pages.

## License

[MIT](LICENSE) — copyright © 2026 Joe Stump.
