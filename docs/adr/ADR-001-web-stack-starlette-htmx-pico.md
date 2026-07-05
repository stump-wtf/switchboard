---
status: proposed
date: 2026-07-05
decision-makers: Joe Stump
related: [ADR-000, ADR-002, ADR-005]
---

# ADR-001: Web/UI Stack — Starlette + Jinja2 + HTMX + Pico.css, Inline-SVG Icons

## Context and Problem Statement

`webhook-mcp` is a single process that must serve three surfaces from one ASGI app: HTTP webhook ingestion endpoints, an MCP server, and a small local-only web UI (4 screens) that updates live via Server-Sent Events. The stack was chosen by *elimination* during the design discussion, and the brief is explicit that the alternatives have already been litigated and should not be reopened. This ADR records the chosen stack and — more importantly — the reasoning behind each rejection, so the next session (and future readers) do not re-argue FastAPI vs. Starlette or Tailwind vs. Pico from scratch. What web framework, templating, frontend-interactivity, CSS, and icon approach give us live-updating screens and clean webhook endpoints with the least weight and the fewest moving parts?

## Decision Drivers

* **One process, three surfaces.** Webhook endpoints, MCP server, and web UI share an event pipeline and a SQLite connection. The web layer must compose cleanly with a long-lived MCP server and background tasks (SSE broadcast, Redis consumer, retention pruning).
* **Live updates are a first-class requirement.** Screen 1 (status strip) and screen 2 (event log) update in real time. SSE is the transport (brief §3, §6); the stack must make SSE + partial HTML updates trivial.
* **Minimal weight, minimal build.** This is a homelab tool. A Node build pipeline, a bundler, or a SPA framework is overhead we do not want to own. Vendored assets over CDNs (offline-friendly, CSP-friendly, no external dependency at runtime).
* **Server-rendered is the natural shape.** The data lives in SQLite on the same box; there is no API-first/mobile-client story. Rendering HTML on the server and swapping fragments is simpler than shipping JSON to a client-side framework that re-renders it.
* **Accessibility and theming for near-zero effort.** `currentColor`-driven icons, a classless CSS base, and semantic HTML get us a usable, themeable, accessible UI without a design system.
* **Don't relitigate.** The design discussion already rejected FastAPI, Flask+Redis SSE, Tailwind, Bootstrap, Heroicons, and icon fonts/Nerd Fonts. This ADR's job is to make those rejections durable.

## Considered Options

Grouped by layer (each row is an independent choice):

* **ASGI framework:** Starlette · FastAPI · Flask (WSGI) · plain stdlib `http.server`
* **Templating:** Jinja2 · f-string/manual HTML · a JS-side template
* **Frontend interactivity:** HTMX (+ `htmx-ext-sse`) · a SPA framework (React/Vue/Svelte) · vanilla JS + `fetch`
* **CSS:** Pico.css (classless) + a small `tokens.css` · Tailwind · Bootstrap · Water.css · hand-rolled CSS
* **Icons:** inline SVG (Lucide for chrome + Simple Icons for brands) · an icon webfont/Nerd Fonts · Heroicons · emoji

## Decision Outcome

Chosen stack: **Starlette + uvicorn** (ASGI), **Jinja2** (`starlette.templating.Jinja2Templates`), **HTMX core + `htmx-ext-sse`** (vendored, not CDN), **Pico.css classless + a small `tokens.css` override layer**, and **inline SVG icons** (Lucide for UI chrome, Simple Icons for provider/brand marks, stored under `static/icons/{ui,brands}/` and inlined via Jinja `{% include %}`). SSE is served by **sse-starlette**.

The reasoning per layer:

- **Starlette over FastAPI:** FastAPI's value is Pydantic-modeled request/response validation and OpenAPI generation for JSON APIs. Our HTTP surface is *webhook receivers* (raw body + signature verification — we deliberately read the raw bytes *before* parsing, which fights FastAPI's model-binding grain) and *server-rendered HTML*, neither of which benefits from FastAPI's machinery. FastAPI *is* Starlette underneath; taking Starlette directly removes a dependency layer without losing anything we use. We author the OpenAPI spec by hand (`docs/specs/openapi.yaml`) precisely because the endpoints are not model-driven.
- **Starlette over Flask/stdlib:** the MCP SDK, SSE, the Redis consumer, and retention pruning are all naturally async and long-lived. An ASGI app hosts them in one event loop; WSGI Flask would need a separate async story bolted on, and stdlib `http.server` would mean hand-rolling routing, lifespan, and concurrency.
- **HTMX over a SPA:** the screens are server-rendered tables and status strips backed by SQLite on the same host. HTMX swaps HTML fragments and, via `htmx-ext-sse`, subscribes DOM elements directly to the `/events` stream — live updates with zero client-side state management and no build step. A SPA would add a bundler, a JSON API surface we otherwise do not need, and client/server state duplication.
- **Pico.css over Tailwind/Bootstrap:** Pico is classless — semantic HTML (`<table>`, `<nav>`, `<article>`) is styled out of the box, so templates stay clean and there is no utility-class churn or CSS build. A tiny `tokens.css` layer overrides the palette. Tailwind requires a build toolchain and litters markup with utility classes; Bootstrap pulls in a heavier component/JS framework we would barely use.
- **Inline SVG over icon fonts:** inline `<svg>` themes via `currentColor`, has no flash-of-unstyled-content, is individually cacheable as a static file, and is accessible (`role="img"` + `<title>`) in a way glyph fonts are not. Nerd Fonts in particular are a terminal/editor glyph-patching tool, wrong for browser rendering. The pack split is deliberate: **Simple Icons** carries brand marks (this is a webhook receiver — provider identity is meaningful UI), **Lucide** carries UI chrome (settings, activity, plug). Not interchangeable, not redundant.

### Consequences

* Good, because the whole UI ships with no Node/bundler step — vendored HTMX + Pico + `tokens.css` + inline SVGs are static files served by Starlette.
* Good, because SSE + fragment swaps make the live screens (status strip, event log) a few lines of template plus one broadcast task, not a client-side app.
* Good, because reading the raw request body for signature verification (see [ADR-003](ADR-003-per-provider-ingestion-and-trust-model.md)) is the natural Starlette idiom, not a fight with a model-binding framework.
* Good, because icons theme with the palette for free and remain accessible.
* Bad, because we hand-author the OpenAPI/AsyncAPI specs instead of generating them — accepted, since the endpoints are webhook receivers and HTML routes that would not model cleanly anyway, and hand-authoring is the point of this docs-first session.
* Bad, because HTMX is less familiar to a React-first contributor than a SPA — accepted for a single-maintainer homelab tool where simplicity wins.
* Bad, because vendoring assets means we own updating them (no CDN auto-refresh) — accepted as the price of offline/CSP-friendly operation.

### Confirmation

* `pyproject.toml` lists `starlette`, `uvicorn`, `jinja2`, `sse-starlette` (and *not* FastAPI, Flask, or a JS build tool) as runtime dependencies.
* HTMX core + `htmx-ext-sse`, Pico.css, and `tokens.css` are committed under `static/` (vendored), not referenced from a CDN.
* Icons live under `static/icons/ui/` (Lucide) and `static/icons/brands/` (Simple Icons) and are inlined via Jinja `{% include %}`; there is no webfont in the repo.
* The web UI renders and live-updates with JavaScript limited to the vendored HTMX core + SSE extension.

## Pros and Cons of the Options

### Starlette (chosen ASGI framework)

* Good, because ASGI natively hosts the MCP server, SSE broadcast, Redis consumer, and pruning task in one loop.
* Good, because raw-body access for signature verification is idiomatic.
* Good, because it is the substrate FastAPI itself builds on — no capability lost by dropping down a layer.
* Neutral, because we forgo automatic request validation — irrelevant for webhook receivers where we validate signatures over raw bytes.

### FastAPI (rejected)

* Good, because Pydantic validation and auto-generated OpenAPI are excellent *for JSON APIs*.
* Bad, because our endpoints are webhook receivers (raw body + HMAC) and HTML pages — neither benefits from model binding, and model binding actively complicates reading the raw body before parse.
* Bad, because it adds a dependency layer over the Starlette we would use directly.

### Flask / stdlib `http.server` (rejected)

* Good, because familiar (Flask) / zero-dependency (stdlib).
* Bad, because WSGI Flask needs an extra async story for SSE + the MCP server + background consumers; stdlib means hand-rolling routing, lifespan, and concurrency. Both are more total work than Starlette for this shape.

### HTMX + `htmx-ext-sse` (chosen interactivity)

* Good, because DOM elements subscribe directly to `/events`; live updates need no client state.
* Good, because no build step — vendored core + extension are two static files.
* Bad, because less familiar than React to some contributors — acceptable for this project's audience.

### SPA framework (rejected)

* Bad, because it forces a JSON API surface, a bundler, and client/server state duplication for screens that are fundamentally server-rendered tables. Overkill.

### Pico.css + `tokens.css` (chosen CSS)

* Good, because classless styling keeps templates semantic and build-free; a tiny token layer handles palette.
* Bad, because it is opinionated and less granular than utilities — fine, we want defaults, not a design system.

### Tailwind / Bootstrap (rejected)

* Bad (Tailwind), because it needs a build toolchain and fills markup with utility classes for a 4-screen tool.
* Bad (Bootstrap), because it drags in a component/JS framework we would barely touch.
* Water.css was a near-tie with Pico as a classless base; Pico won on component coverage (forms, tables, nav) for the config/settings screens.

### Inline SVG icons (chosen)

* Good, because `currentColor` theming, no FOUC, per-icon caching, and real accessibility semantics.
* Good, because the Lucide/Simple Icons split maps cleanly to chrome vs. brand.
* Neutral, because we vendor and update icon files ourselves — trivial at this count.

### Icon fonts / Nerd Fonts / Heroicons (rejected)

* Bad (icon fonts / Nerd Fonts), because glyph fonts flash unstyled, theme poorly, and are semantically opaque; Nerd Fonts are a terminal-glyph tool, not a web-icon system.
* Bad (Heroicons), because it covers UI chrome but not brand marks — we would still need Simple Icons for providers, so Lucide + Simple Icons is the cleaner two-pack split.

## Architecture Diagram

```mermaid
flowchart TB
  subgraph proc[Single Starlette + uvicorn process]
    direction TB
    subgraph routes[Route groups]
      wh[/webhooks/{provider}/]
      ui[UI routes → Jinja2 templates]
      sse[/events → sse-starlette/]
      mcpm[MCP server mount]
    end
    pipe[[normalize → SQLite → broadcast]]
    wh --> pipe
    pipe --> sse
    pipe --> mcpm
    ui -. reads .- pipe
  end
  browser([Browser]) -->|HTMX fragment GET/POST| ui
  browser -->|htmx-ext-sse subscribe| sse
  static[/static: vendored HTMX, Pico.css,\ntokens.css, inline SVG icons/] --> browser
  classDef rej fill:#fdd,stroke:#b00
```

## More Information

* Rejected alternatives are drawn directly from the brief §4 ("Explicitly rejected alternatives — already litigated, don't relitigate") and §13 (key learnings on Nerd Fonts and the icon-pack split).
* Icon sourcing: Lucide (<https://lucide.dev/>) for chrome, Simple Icons (<https://simpleicons.org/>) for brands.
* Stack references: Starlette <https://www.starlette.io/>, uvicorn <https://www.uvicorn.org/>, sse-starlette <https://github.com/sysid/sse-starlette>, Jinja2 <https://jinja.palletsprojects.com/>, HTMX <https://htmx.org/>, `htmx-ext-sse` <https://github.com/bigskysoftware/htmx-extensions/tree/main/src/sse>, Pico.css <https://picocss.com/>.
* Related: [ADR-002](ADR-002-sqlite-persistence-and-retention.md) (the SQLite layer the UI reads), [ADR-005](ADR-005-mcp-tool-and-resource-contract.md) (the MCP surface sharing this process).
