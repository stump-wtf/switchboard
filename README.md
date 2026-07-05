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
[ADR-000](docs/adr/ADR-000-project-naming-and-scope.md)).

> [!IMPORTANT]
> **Status: docs-first bootstrap.** This repository currently contains the **architecture decision
> records** (`docs/adr/`), the **API/stream/tool specs** (`docs/specs/`), and the repo/CI scaffolding.
> There is **no application code yet** beyond empty package placeholders under `app/` — it is written
> *fresh from these documents* in a follow-up session. The "Running it" and "Usage" sections below
> describe the **intended** behavior these specs define, not something you can `pip install` and run
> today. Start at the design index: [`docs/README.md`](docs/README.md).

## Two layers

- **Event-store core (ADR-000–005):** receive, verify, persist, and expose inbound webhooks/queue
  events — the pipeline described in this README.
- **Agent layer (ADR-007–015):** inbound events become durable **todos** that agents claim and
  complete; humans register agents and are vended scoped MCP endpoints; personas are advertised as A2A
  Agent Cards; and cross-agent work is granted by human-approved friending. See
  [ADR-007](docs/adr/ADR-007-todos-as-core-primitive.md) and the
  [design index](docs/README.md).

## Why this exists

Different providers have wildly different security stories, and pretending otherwise is a security
bug. `switchboard` makes each source's trust level **explicit, per-provider, enforced, and visible** —
a signed GitHub event and a token-authenticated Docker Hub event are never displayed or exposed as if they
were the same thing. See [ADR-003](docs/adr/ADR-003-per-provider-ingestion-and-trust-model.md).

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
[ADR-001](docs/adr/ADR-001-web-stack-go-htmx-pico.md).

## Trust model at a glance (ADR-003)

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
| [ADR-000](docs/adr/ADR-000-project-naming-and-scope.md) | Project name + MVP/session scope |
| [ADR-001](docs/adr/ADR-001-web-stack-go-htmx-pico.md) | Web/UI stack (Go net/http + chi, html/template, HTMX + Pico) — and why not a framework / Tailwind / icon fonts |
| [ADR-002](docs/adr/ADR-002-postgres-persistence-and-retention.md) | PostgreSQL persistence, queue mechanics, schema sketch, retention |
| [ADR-003](docs/adr/ADR-003-per-provider-ingestion-and-trust-model.md) | Ingestion provider types (webhook / queue) & the trust model (signed / token / open / queue) |
| [ADR-005](docs/adr/ADR-005-mcp-tool-and-resource-contract.md) | MCP tool/resource contract shape |
| [openapi.yaml](docs/specs/openapi.yaml) | HTTP surface: webhook ingestion + UI endpoints |
| [asyncapi.yaml](docs/specs/asyncapi.yaml) | SSE event/message schema |
| [mcp-tools.md](docs/specs/mcp-tools.md) | Exact MCP tool + resource JSON Schemas |

## Web UI (4 screens)

1. **Dashboard / status** — live connection strip (`● receiving` / `○ idle`), recent event count per provider, last-event timestamp per provider.
2. **Webhook log** — paginated table (provider icon, event type, timestamp, verify status, size); row → detail (raw payload, sanitized headers, verification result).
3. **Provider config** — each provider's type, path/channel, secret status (`configured` / `missing` / `none-by-design` — never the secret itself), enable/disable toggle.
4. **Settings** — retention policy (age + row cap), SSE reconnect behavior, general config.

## Running it (intended — code deferred)

```bash
# Once the code session lands:
make build            # compile the switchboard binary (assets embedded)
./switchboard         # serves the UI, /webhooks/*, /events (SSE), and the MCP mount on 127.0.0.1
```

The service is **loopback-bound by default and ships no in-app auth**. If you ever expose it on the
homelab LAN it **must** sit behind Caddy `forward_auth`, like everything else in the stack — auth is
the reverse proxy's job, not this app's (ADR-001, brief §8).

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
[ADR-003](docs/adr/ADR-003-per-provider-ingestion-and-trust-model.md).

## Development

```bash
make ci     # gofmt + go vet + golangci-lint + govulncheck + go test — the local mirror of CI
make fmt    # auto-format
make test   # go test ./...
```

The docs site builds with Docusaurus and deploys to **GitHub Pages** via `.github/workflows/pages.yml`.

## Repository hosting

- **Primary (source of truth):** <https://gitea.stump.rocks/joestump/switchboard>
- **Mirror (backup/reach):** <https://github.com/joestump/switchboard> — a Gitea push-mirror.

## License

[MIT](LICENSE) — copyright © 2026 Joe Stump.
