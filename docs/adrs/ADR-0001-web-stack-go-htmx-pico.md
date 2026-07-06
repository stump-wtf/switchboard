---
status: proposed
date: 2026-07-05
decision-makers: Joe Stump
related: [ADR-0000, ADR-0002, ADR-0005, ADR-0015]
---

# ADR-0001: Web/UI Stack — Go net/http + chi, html/template, HTMX + Pico.css, Inline-SVG Icons

## Context and Problem Statement

`switchboard` is implemented in **Go** ([ADR-0015](ADR-0015-implementation-language-go.md)) and serves several surfaces from one binary: HTTP webhook ingestion endpoints, an MCP server, and a small local-only web UI (4 screens) that updates live via Server-Sent Events. Given Go, the remaining choices are the HTTP router, templating, frontend-interactivity, CSS, and icon approach. This ADR records the chosen web/UI stack and — more importantly — the reasoning behind each rejection, so the code session (and future readers) do not re-argue net/http-vs-a-framework or Tailwind-vs-Pico from scratch. What gives us live-updating screens and clean webhook endpoints with the least weight and the fewest moving parts?

## Decision Drivers

* **One binary, several surfaces.** Webhook endpoints, the MCP server, and the web UI share the event/todo pipeline and a PostgreSQL pool ([ADR-0002](ADR-0002-postgres-persistence-and-retention.md)). The web layer must compose cleanly with a long-lived MCP server and background goroutines (SSE broadcast, pull-adapter consumers, lease reaper, retention pruning).
* **Live updates are first-class.** The status strip and event log update in real time. SSE is the transport; the stack must make SSE + partial HTML updates trivial.
* **Minimal weight, no Node build.** A bundler or SPA framework is overhead we do not want to own. Assets are **vendored and embedded in the binary** (`embed.FS`) — offline-friendly, CSP-friendly, no runtime CDN.
* **Server-rendered is the natural shape.** The data lives in PostgreSQL behind the same service; there is no API-first/mobile-client story. Rendering HTML on the server and swapping fragments beats shipping JSON to a client-side framework.
* **Accessibility and theming for near-zero effort.** `currentColor`-driven inline SVGs, a classless CSS base, and semantic HTML give a usable, themeable, accessible UI without a design system.
* **Idiomatic Go, small surface.** Prefer the standard library plus one light router over a batteries-included web framework; the endpoints are webhook receivers and HTML routes, not a JSON API needing a framework's machinery.

## Considered Options

Grouped by layer (each row is an independent choice):

* **HTTP layer:** `net/http` + a light router (**chi**) · a full framework (Gin / Echo / Fiber) · `net/http` mux alone
* **Templating:** stdlib `html/template` · a third-party engine (templ, quicktemplate) · string building
* **Frontend interactivity:** HTMX (+ `htmx-ext-sse`) · a SPA framework (React/Vue/Svelte) · vanilla JS + `fetch`
* **CSS:** Pico.css (classless) + a small `tokens.css` · Tailwind · Bootstrap · Water.css · hand-rolled CSS
* **Icons:** inline SVG (Lucide for chrome + Simple Icons for brands) · an icon webfont/Nerd Fonts · Heroicons · emoji

## Decision Outcome

Chosen stack: **`net/http` + chi** (router/middleware), **stdlib `html/template`**, **HTMX core + `htmx-ext-sse`** (vendored, not CDN), **Pico.css classless + a small `tokens.css` override**, and **inline SVG icons** (Lucide for UI chrome, Simple Icons for provider/brand marks). SSE is a plain `net/http` handler using **`http.Flusher`** — no extra dependency. All static assets and templates are **embedded via `embed.FS`**, so the whole UI ships inside the single binary ([ADR-0015](ADR-0015-implementation-language-go.md)).

The reasoning per layer:

- **`net/http` + chi over a full framework:** the HTTP surface is *webhook receivers* (raw body + HMAC — we read the raw bytes *before* parsing, [ADR-0003](ADR-0003-per-provider-ingestion-and-trust-model.md)) and *server-rendered HTML*. `net/http` handles both directly; chi adds only lightweight routing, URL params, and middleware (request id, recovery, timeouts) without imposing a framework's request/response model. Gin/Echo/Fiber bring binding/validation and their own context types that a non-JSON, receiver-shaped service does not need — and Fiber's fasthttp base complicates streaming (SSE) and stdlib-`net/http` interop (which the MCP SDK expects). We hand-author the OpenAPI spec (`docs/specs/openapi.yaml`) precisely because the endpoints are not model-driven.
- **Goroutines host the long-lived parts.** The MCP server, SSE broadcast, pull-adapter consumers, lease reaper, and pruning run as goroutines under one process — no event-loop or worker-manager gymnastics ([ADR-0015](ADR-0015-implementation-language-go.md)).
- **SSE via `http.Flusher`, no library.** Go streams Server-Sent Events with a handler that writes `data:` frames and calls `Flush()`; a broadcast hub fans todo/event updates to subscribed clients. This is a few dozen lines of stdlib, so there is no SSE dependency to vendor.
- **stdlib `html/template` over a third-party engine:** contextual auto-escaping, zero dependency, and it composes with `embed.FS`. Fragment responses for HTMX swaps are just named templates. A compile-time engine (templ) is nice but adds a codegen step we do not need at this screen count.
- **HTMX over a SPA:** the screens are server-rendered tables and status strips. HTMX swaps HTML fragments and, via `htmx-ext-sse`, subscribes DOM elements directly to the `/events` stream — live updates with zero client-side state and no build step. A SPA would add a bundler, a JSON API surface we otherwise do not need, and client/server state duplication.
- **Pico.css over Tailwind/Bootstrap:** Pico is classless — semantic HTML (`<table>`, `<nav>`, `<article>`) is styled out of the box, so templates stay clean and there is no CSS build. A tiny `tokens.css` layer overrides the palette with the project's switchboard-era visual identity — bakelite, brass, operator-cream, oxblood, and patch-cable tones (bakelite-dark by default, operator-cream light), mapped onto Pico's `--pico-*` variables plus trust badge tokens (`signed`/`token`/`open`/`queue`, [ADR-0003](ADR-0003-per-provider-ingestion-and-trust-model.md)). This palette is already committed at [`static/tokens.css`](../../static/tokens.css) and shared with the docs site ([ADR-0000](ADR-0000-project-naming-and-scope.md)). Tailwind needs a build toolchain and litters markup with utility classes; Bootstrap drags in a component/JS framework we would barely use.
- **Inline SVG over icon fonts:** inline `<svg>` themes via `currentColor`, has no flash-of-unstyled-content, is embeddable via `embed.FS`, and is accessible (`role="img"` + `<title>`) in a way glyph fonts are not. The pack split is deliberate: **Simple Icons** carries brand marks (provider identity is meaningful UI on a webhook receiver), **Lucide** carries UI chrome (settings, activity, plug). Not interchangeable, not redundant.

### Consequences

* Good, because the whole UI — templates, vendored HTMX + Pico + `tokens.css`, inline SVGs — is **embedded in the single binary**; there is no Node/bundler step and nothing to serve from disk or a CDN.
* Good, because SSE + fragment swaps make the live screens a handful of templates plus one broadcast goroutine, not a client-side app.
* Good, because reading the raw request body for signature verification ([ADR-0003](ADR-0003-per-provider-ingestion-and-trust-model.md)) is the natural `net/http` idiom.
* Good, because icons theme with the palette for free and stay accessible.
* Bad, because we hand-author the OpenAPI/AsyncAPI specs instead of generating them — accepted; the endpoints are receivers and HTML routes that would not model cleanly, and hand-authoring is the point of a docs-first design.
* Bad, because HTMX is less familiar to a React-first contributor than a SPA — accepted for a single-maintainer service where simplicity wins.
* Bad, because vendoring assets means we own updating them (no CDN auto-refresh) — accepted as the price of offline/CSP-friendly, single-binary operation.

### Confirmation

* `go.mod` lists `chi` (and *not* Gin/Echo/Fiber or a JS build tool); the MCP SDK and `pgx` sit alongside it.
* HTMX core + `htmx-ext-sse`, Pico.css, `tokens.css`, and the inline SVG icons are committed under `static/` and served from an `embed.FS`, not a CDN.
* The `/events` SSE endpoint is a stdlib `net/http` handler using `http.Flusher`; there is no third-party SSE dependency.
* The web UI renders and live-updates with JavaScript limited to the vendored HTMX core + SSE extension.

## Pros and Cons of the Options

### `net/http` + chi (chosen HTTP layer)

* Good, because stdlib `net/http` serves webhook receivers and HTML directly; chi adds routing/middleware without a framework's model.
* Good, because raw-body access for signature verification is idiomatic, and it interoperates cleanly with the Go MCP SDK's `http.Handler` mount and SSE streaming.
* Neutral, because we forgo built-in request binding/validation — irrelevant for receivers validated over raw bytes.

### A full framework — Gin / Echo / Fiber (rejected)

* Good, because batteries-included binding, validation, and helpers *for JSON APIs*.
* Bad, because our endpoints are webhook receivers (raw body + HMAC) and HTML pages — none benefit from binding, and Fiber's fasthttp base complicates SSE streaming and stdlib-`net/http` interop with the MCP SDK.

### HTMX + `htmx-ext-sse` (chosen interactivity)

* Good, because DOM elements subscribe directly to `/events`; live updates need no client state, and there is no build step.
* Bad, because less familiar than React to some contributors — acceptable for this audience.

### SPA framework (rejected)

* Bad, because it forces a JSON API surface, a bundler, and client/server state duplication for screens that are fundamentally server-rendered tables. Overkill.

### Pico.css + `tokens.css` (chosen CSS)

* Good, because classless styling keeps templates semantic and build-free; a tiny token layer handles the palette.
* Bad, because it is opinionated and less granular than utilities — fine, we want defaults, not a design system. Water.css was a near-tie; Pico won on component coverage (forms, tables, nav) for the config/settings screens.

### Tailwind / Bootstrap (rejected)

* Bad (Tailwind), because it needs a build toolchain and fills markup with utility classes for a 4-screen tool.
* Bad (Bootstrap), because it drags in a component/JS framework we would barely touch.

### Inline SVG icons (chosen) vs. icon fonts / Nerd Fonts / Heroicons (rejected)

* Good (inline SVG), because `currentColor` theming, no FOUC, `embed.FS` bundling, and real accessibility semantics; the Lucide/Simple Icons split maps cleanly to chrome vs. brand.
* Bad (icon fonts / Nerd Fonts), because glyph fonts flash unstyled, theme poorly, and are semantically opaque; Nerd Fonts are a terminal-glyph tool, not a web-icon system.
* Bad (Heroicons), because it covers UI chrome but not brand marks — we would still need Simple Icons, so Lucide + Simple Icons is the cleaner two-pack split.

## Architecture Diagram

```mermaid
flowchart TB
  subgraph proc[Single Go binary — net/http + chi]
    direction TB
    subgraph routes[Routes]
      wh[/webhooks/{provider}/]
      ui[UI routes → html/template]
      sse[/events → http.Flusher SSE/]
      mcpm[MCP server mount — Go MCP SDK]
    end
    pipe[[normalize → PostgreSQL → broadcast]]
    workers[[goroutines: pull consumers,<br/>lease reaper, pruning]]
    wh --> pipe
    workers --> pipe
    pipe --> sse
    pipe --> mcpm
    ui -. reads .- pipe
  end
  browser([Browser]) -->|HTMX fragment GET/POST| ui
  browser -->|htmx-ext-sse subscribe| sse
  embed[/embed.FS: vendored HTMX, Pico.css,<br/>tokens.css, inline SVG icons/] --> browser
```

## More Information

* Language & runtime this stack sits on: [ADR-0015](ADR-0015-implementation-language-go.md).
* Icon sourcing: Lucide (<https://lucide.dev/>) for chrome, Simple Icons (<https://simpleicons.org/>) for brands.
* Stack references: `net/http` <https://pkg.go.dev/net/http>, chi <https://github.com/go-chi/chi>, `html/template` <https://pkg.go.dev/html/template>, `embed` <https://pkg.go.dev/embed>, HTMX <https://htmx.org/>, `htmx-ext-sse` <https://github.com/bigskysoftware/htmx-extensions/tree/main/src/sse>, Pico.css <https://picocss.com/>.
* Related: [ADR-0002](ADR-0002-postgres-persistence-and-retention.md) (the PostgreSQL layer the UI reads), [ADR-0005](ADR-0005-mcp-tool-and-resource-contract.md) (the MCP surface sharing this process).
