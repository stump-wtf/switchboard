---
status: proposed
date: 2026-10-25
decision-makers: Proposed by joestump-agent, pending Joe
extends: [ADR-0022]
related: [ADR-0014, ADR-0020, ADR-0051, ADR-0052]
---

# ADR-0053: Provider Read Clients Behind One Interface — The Read Half of the Reconcile Pass

**Status note:** this ADR is **proposed**. It records code that already shipped in PR #619 (part B
of the epic #587 split, issue #613) so the design can be argued against working code rather than
prose; nothing here is settled until Joe accepts it.

## Context and Problem Statement

Switchboard's intake is webhook-push only: a delivery that never arrives — a `429` from a producer
that does not retry, a lost event — leaves a gap no queue state can show. ADR-0051 made that gap
visible (per-source pre-intake reject counts); it cannot make the gap go away. Recovering the
missing work needs the other half of the #587 split, the endpoint-scoped reconcile pass (part D,
#616): read back what the provider holds and re-drive what this server never recorded.

That pass has one prerequisite the repo deliberately lacks. Migration 0021 removed the provider
registry and its pull adapters on purpose (ADR-0014 superseded, ADR-0020 retired; ADR-0022: todo
ownership is endpoint-scoped). There is no read path back into a provider at all. PR #619 therefore
adds one — and only one: read clients for GitHub and Gitea behind a single interface, tested
against `httptest` servers, wired to nothing, owning no credentials.

## Decision

**Provide `internal/provider`: one `Reader` interface that lists recently-updated issues and pull
requests from one provider, with GitHub and Gitea implementations that are interchangeable behind
it.**

- **The interface is one method**: `Reader.ListRecentlyUpdated(ctx, repo, since, cursor) (*Page,
  error)`. `since` zero lists without a date floor; a non-zero `since` keeps items updated at or
  after it, and a page whose oldest item is older than `since` ends the walk (pages are ordered
  newest first). `cursor` is `""` for the first page and then the `NextCursor` the same
  implementation returned.
- **A listed item is `WorkItem`**: `Kind` (`issue` / `pull_request`), `Number`, `Title`, the
  provider's own `State` verbatim, `Draft`, `HTMLURL`, `UpdatedAt`. A `Page` is its newest-first
  `Items` plus an opaque `NextCursor` that is `""` once the walk is done.
- **Errors are explicit**: non-2xx answers come back as a `*StatusError` carrying the provider,
  op, status code and (for 429) the `Retry-After` duration; `errors.Is` matches the sentinels
  `ErrForbidden` (403), `ErrNotFound` (404) and `ErrRateLimited` (429) so a caller branches on the
  situation without inspecting codes. A cursor that decodes badly or points at a different origin
  than the configured base is refused with `ErrInvalidCursor` before any request goes out.
- **GitHub** walks the repository issue listing, which carries pull requests too (detected by the
  `pull_request` field), and pages via the `Link: rel=next` target — which is refused unless it
  points back at the configured base URL. `since` is passed to the API; a page that reaches past
  `since` ends the walk without fetching the next one.
- **Gitea** lists issues and pulls from separate endpoints, so the client pages both streams in
  parallel and merges them by `updated_at`; its cursor is an opaque base64 pair of the two streams'
  page numbers (0 meaning that stream is spent). PR-shaped rows surfacing in the issue stream are
  skipped, so each pull request is yielded exactly once, from the pull stream.
- **No credentials live in the package.** A caller that authenticates does so with an `http.Client`
  whose transport injects its own token (`WithHTTPClient`); the package never reads, stores, or
  logs it. The base URL is always an explicit constructor argument: `newHTTPBase` accepts only an
  absolute `http`/`https` URL with a host and no credentials, query, or fragment, so neither
  `NewGitHub` nor `NewGitea` fills in a default (a GitHub Enterprise Server caller passes its own
  root, and a Gitea caller always names its own host), and no test dials anything beyond an
  `httptest` server.
- **Adding a provider** means one implementation of the single-method `Reader` over the package's
  shared `httpBase` (validated base URL, the two options, page size clamped to [1, 100] with a
  default of 50), plus an `httptest`-only test suite. The provider-specific shape of the API —
  one combined list versus parallel streams, how drafts and pull requests are marked, what the
  cursor encodes — stays inside that implementation; everything the reconcile pass sees is the
  interface, the cursor, and the sentinels.

## Considered Options

- **Revive the pull adapters.** Rejected: ADR-0022 endpoint-scoped todo ownership is why migration
  0021 removed the registry and pull adapters. These clients are read-only, endpoint-scoped, and
  import neither `store` nor `ingest`; nothing here wires them into a delivery path.
- **Per-provider interfaces instead of one `Reader`.** Rejected: the reconcile pass wants to branch
  on one shape — pages, an opaque cursor, three error sentinels — not on provider. The Gitea
  two-stream walk is exactly the kind of provider quirk the interface should hide.
- **Credential fields on the clients.** Rejected: a token on a struct is a token the package
  touches, and the trust posture of a delivery synthesized from a list result is #614's
  connection-config question. The client only receives an `http.Client`; it does not know whether
  one is authenticated.
- **Server-side or database-backed walk state.** Rejected: a reconcile walk is a transient read by
  one caller; the state belongs in the caller's cursor, and an opaque, per-client, base-checked
  cursor keeps it leak-free and testable.

## Consequences

**Gains.** The reconcile pass (#616) has a single read surface per provider, with the errors it
needs to behave: 429 backs off by `RetryAfter`, 403/404 abort the walk for that repo, and a
foreign cursor is refused locally instead of becoming a request to a third-party host. Provider
quirks (Gitea's split streams, GitHub's PR-in-issue-list) are implementation detail behind one
cursor, and a new provider is one file plus tests with no live access needed.

**Costs and limits.** The read clients are unwired by design: nothing in the tree calls
`ListRecentlyUpdated`, so they ship with zero runtime effect until the reconcile pass lands (the
package itself is imported by `internal/config` and `internal/store` since #620 landed its
connection-config helpers there, but no delivery path reaches the read path). The
walk's `since` bound is only as good as the provider's update ordering; a `state=all` listing
covers open and closed alike, and a closed-and-reopened item is only "recently updated" by what
the provider reports. No item the client returns is trusted: whether a delivery synthesized from
it is admitted is decided by the connection-config child (#614), not by this package.

**Implied requirement changes.** None applied: no spec text changes in this PR. A reconcile spec
(part D) will build on this interface and the sentinels when it is written; this ADR settles the
shape that shape reads against.
