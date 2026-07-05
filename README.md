# Switchboard

*Many lines come in. The operator verifies each caller, and patches it through.*

Switchboard is the operator's board for your inbound webhooks. It receives events from external
providers (GitHub, Stripe, Slack, Docker Hub, and self-hosted/homelab senders), verifies and
normalizes each one, stores them in SQLite, and patches them through to **two consumers of the same
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
> today.

## Why this exists

Different providers have wildly different security stories, and pretending otherwise is a security
bug. `switchboard` makes each source's trust level **explicit, per-provider, enforced, and visible** —
a signed GitHub event and an unverified Docker Hub event are never displayed or exposed as if they
were the same thing. See [ADR-003](docs/adr/ADR-003-per-provider-ingestion-and-trust-model.md).

## Architecture

```
Provider (GitHub/Stripe/Slack/Docker/…)        Redis (pub/sub or stream)
      │  HTTPS POST + signature header               │  in-process consumer subscribes
      ▼                                               ▼
[Starlette app: /webhooks/{provider}]        [Redis consumer task, in-process]
      │  verify signature → normalize                │  normalize (trust = Redis ACL/TLS,
      │  (or: generic/no-verify path)                │   no HTTP signature concept)
      ▼                                               ▼
   SQLite (events table) ◄────────────────────────────┘
      │
      ├──► MCP tools/resources  (list / get / replay / list_providers)
      ├──► SSE broadcast (/events) ──► Web UI (Jinja2 + HTMX + Pico.css)
      └──► retention pruning (age + row-cap)
```

Single process, single repo. The MCP server and the web server share the same Starlette app
(different route groups) and the same SQLite layer. Stack rationale — Starlette over FastAPI, HTMX
over a SPA, Pico over Tailwind, inline SVG over icon fonts — is in
[ADR-001](docs/adr/ADR-001-web-stack-starlette-htmx-pico.md).

## Trust model at a glance (ADR-003)

| Mode | Providers | How it's trusted | On failure |
|------|-----------|------------------|-----------|
| **signed** | GitHub, Stripe, Slack | Mandatory per-provider HMAC signature verification (constant-time; Stripe/Slack also enforce a timestamp freshness window) | **401, payload NOT persisted**, redacted rejection logged |
| **generic** (unverified by design) | Docker Hub, homelab/self-hosted | **No signature exists.** Opt-in, disabled by default, guarded by a non-crypto shared token (bozo-filter), labeled *unverified* everywhere. Trusted-network only. | 403 on bad token / disabled |
| **redis** (queue consumer) | anything publishing to the subscribed channel | Trust boundary is the **Redis connection** (auth/ACL, TLS) — "who can publish to this channel" | connection-level |

Docker Hub has no native webhook signing, so it is routed through the **generic** endpoint rather
than faked as "signed." Inventing verification where none exists would make the `signed` badge
meaningless for every other provider.

## Documentation

| Doc | What it covers |
|-----|----------------|
| [ADR-000](docs/adr/ADR-000-project-naming-and-scope.md) | Project name + MVP/session scope |
| [ADR-001](docs/adr/ADR-001-web-stack-starlette-htmx-pico.md) | Web/UI stack — and why not FastAPI / Tailwind / icon fonts |
| [ADR-002](docs/adr/ADR-002-sqlite-persistence-and-retention.md) | SQLite persistence, schema sketch, retention/pruning |
| [ADR-003](docs/adr/ADR-003-per-provider-ingestion-and-trust-model.md) | Per-provider ingestion & the three trust models |
| [ADR-004](docs/adr/ADR-004-secrets-management-openbao-approle.md) | Secrets via OpenBao AppRole |
| [ADR-005](docs/adr/ADR-005-mcp-tool-and-resource-contract.md) | MCP tool/resource contract shape |
| [ADR-006](docs/adr/ADR-006-gitea-primary-github-mirror-and-ci.md) | Gitea-primary/GitHub-mirror repo + CI |
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
make install          # editable install + dev tooling + pre-commit
uvicorn app.main:app  # serves the UI, /webhooks/*, /events (SSE), and the MCP mount on 127.0.0.1
```

The service is **loopback-bound by default and ships no in-app auth**. If you ever expose it on the
homelab LAN it **must** sit behind Caddy `forward_auth`, like everything else in the stack — auth is
the reverse proxy's job, not this app's (ADR-001, brief §8).

Secrets (provider HMAC secrets, the Redis URL, generic tokens) are pulled at runtime from **OpenBao**
via **AppRole** — never from `.env` or committed config. See
[ADR-004](docs/adr/ADR-004-secrets-management-openbao-approle.md).

### Pointing a provider at this service (for testing)

Because the service is localhost-bound, expose it to a provider during testing with a tunnel
(e.g. `cloudflared tunnel`, `tailscale funnel`, or an SSH reverse tunnel), then set the provider's
webhook URL to the tunnel's public URL + the provider path:

- GitHub → `https://<tunnel>/webhooks/github`
- Stripe → `https://<tunnel>/webhooks/stripe`
- Slack → `https://<tunnel>/webhooks/slack`
- Docker Hub (unverified) → `https://<tunnel>/webhooks/generic/dockerhub?token=<shared-token>`

Put the corresponding signing secret in OpenBao at `secret/switchboard/providers/<provider>` first,
or the signed endpoint will (correctly) 401.

## Adding a new provider (intended shape)

1. **Signed provider:** add an adapter under `app/providers/<name>.py` implementing the verification
   for its signature scheme (raw-body HMAC, constant-time compare, timestamp window if the scheme
   signs one), register it with `trust_mode=signed`, and store its secret in OpenBao at
   `secret/switchboard/providers/<name>`.
2. **Unsigned / homelab sender:** don't write an adapter — create a **generic** provider
   (`/webhooks/generic/<name>`), which is unverified by design and disabled until you opt in.
3. **Queue source:** point the Redis consumer at another channel/stream; the trust boundary is that
   channel's Redis ACL.

The trust mode is always declared per provider and shown in the UI — never silently assumed. See
[ADR-003](docs/adr/ADR-003-per-provider-ingestion-and-trust-model.md).

## Development

```bash
make ci     # ruff + mypy + bandit + pip-audit + pytest — the local mirror of CI
make fmt    # auto-format
make test   # pytest
```

CI runs on **Gitea** (primary, `.gitea/workflows/ci.yaml` — the fast + security gate) and on the
**GitHub mirror** (`.github/workflows/ci.yml` — the same suite across a Python 3.12/3.13 matrix). See
[ADR-006](docs/adr/ADR-006-gitea-primary-github-mirror-and-ci.md).

## Repository hosting

- **Primary (source of truth):** <https://gitea.stump.rocks/joestump/switchboard>
- **Mirror (backup/reach):** <https://github.com/joestump/switchboard> — a Gitea push-mirror.

## License

[MIT](LICENSE) — copyright © 2026 Joe Stump.
