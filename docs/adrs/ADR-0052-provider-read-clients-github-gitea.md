---
status: proposed
date: 2026-10-24
decision-makers: Proposed by joestump-agent, pending Joe
extends: [ADR-0022]
related: [ADR-0003, ADR-0014, ADR-0020, ADR-0051]
---

# ADR-0052: Provider Read Clients Behind One Interface — Read What The Tracker Holds, Without Reviving Ingest

**Status note:** this ADR is **proposed**. It ships with an implementation that already landed in
[PR #619](https://gitea.stump.rocks/stump.wtf/switchboard/pulls/619) so the choices can be argued
against working code rather than prose; nothing here is settled until Joe accepts it. It records
[issue #613](https://gitea.stump.rocks/stump.wtf/switchboard/issues/613), part B of the epic #587
split, and the interface that part's implementation exposes.

## Context and Problem Statement

The reconcile pass (epic #587, child #616) has to diff what a provider holds against what this
server recorded, and re-drive what never arrived. To do that it must read the tracker. Two trackers
matter in this deployment, and their listing APIs disagree in exactly the ways a caller does not
want to branch on:

- GitHub serves one listing for a repository — issues *and* pull requests in it, the pull requests
  flagged by a `pull_request` field — and paginates through an RFC 5988 `Link: rel="next"` header.
- Gitea lists issues and pulls from two separate endpoints, paginates by `page`/`limit`, and serves
  pull requests on the issue listing as well (rows flagged `is_pull`), so a naive union lists every
  pull request twice.

Meanwhile this repo's trust history points the other way. Migration 0021 removed the provider
registry and its pull adapters on purpose: ADR-0022 made todo ownership endpoint-scoped, and
instance-wide pull ingestion is gone (ADR-0014 superseded, ADR-0020 superseded). A read path that
looks like the old adapters — a registry, a credential store, an ingest loop — would undo that
decision. So the question this ADR answers is narrow: what does a *caller* — an endpoint-scoped
reconcile pass, not the server — get to read the provider with, and what must that read not look
like?

## Decision

**Give the caller one `Reader` interface over one read operation, one implementation per provider,
no credentials, and no wiring.**

- **One interface, one operation.** `Reader` exposes `ListRecentlyUpdated(ctx, repo, since,
  cursor)`, returning a `Page` of `WorkItem`s newest first plus an opaque `NextCursor` that is `""`
  when the walk is done. A zero `since` lists without a date floor; a non-zero `since` keeps items
  updated at or after it, and because pages are ordered newest first, a page that reaches past
  `since` ends the walk. That single walk is all a reconcile diff needs — "what changed since I last
  looked". There is deliberately no second operation: no item fetch, no mutation verb. One more read
  is one more branch the caller will grow around, and a write would be the old pull adapter wearing
  a new name.
- **Both providers, one shape.**
  - `NewGitHub(baseURL, opts...)` — default base `https://api.github.com`; a GitHub Enterprise
    Server caller passes its own. It walks `GET /repos/{owner}/{name}/issues?state=all&sort=updated
    &direction=desc`, classifies a row as `KindPullRequest` when it carries a `pull_request`
    object, and pages via the `Link` header; a `rel="next"` target is accepted only when it points
    back at the configured base.
  - `NewGitea(baseURL, opts...)` — self-hosted, so there is no default base; the caller passes its
    API root. It pages the issue and pull streams in parallel, merges the two into one page ordered
    by `updated_at` (stable), and tracks the walk in the cursor as a pair of stream page numbers.
    A PR-shaped row (`is_pull`) surfacing in the issue stream is skipped, so each pull request
    comes from the pull stream exactly once.
- **`WorkItem` carries what the listing carries, verbatim.** `Kind`, `Number`, `Title`, `State`
  (the provider's own string, not normalized), `Draft` (set only for pull requests), `HTMLURL`,
  and `UpdatedAt`, falling back to `created_at` when the provider omitted `updated_at`. A reconcile
  caller compares this against its own recorded state; it is not a second source of truth.
- **The error paths a reconcile caller branches on are sentinel-matched.** A non-2xx answer comes
  back as a `*StatusError` that `errors.Is`-matches `ErrForbidden` (403), `ErrNotFound` (404), or
  `ErrRateLimited` (429, with `RetryAfter` parsed from the header); any other status carries its
  code and matches no sentinel. A cursor that decodes badly or points outside the configured base is
  refused with `ErrInvalidCursor` before a request goes out, and a caller constructs cursors by no
  other means than the `NextCursor` the same client, the same repository and the same `since`
  handed back.
- **The package owns no credential.** Switchboard does not hold provider tokens, and this does not
  change: an authenticating caller injects its token through the transport of the `http.Client` it
  passes via `WithHTTPClient`, which the package never reads, stores, or logs. The base URL is a
  constructor argument — validated to http/https with no embedded credential, query, or fragment —
  and it is also what makes `httptest` the natural test double: every implementation is tested
  against a local `httptest` server that records the path and query, so the suites pin the
  pagination walks, the merged-stream ordering, the PR dedup, the `since` cutoff (past-`since` pages
  are never fetched), the 403/404/429/500 paths, foreign-cursor and garbage-cursor rejection, repo
  path-segment validation, and the request/query shape, without a test ever dialing a real provider
  or a default.
- **Nothing is wired.** No store import, no ingest import, no config, no default dialed from
  production code. ADR-0022 stands untouched: these are plain read clients an endpoint-scoped
  caller will own, not revived pull ingestion, and the trust posture of a delivery synthesized from
  a list result stays #614's question.
- **A new provider is one file in `internal/provider`**: a struct wrapping the shared `httpBase`, a
  constructor that validates its base URL, the one `Reader` method built on the shared `repoURL`
  and `getJSON` helpers (cursor-encoding included), and an `httptest` suite. `httpBase` is
  deliberately unexported: a provider that cannot be served by the same HTTP discipline — opaque
  cursors, refused foreign link targets, bounded bodies, sentinel statuses — belongs in its own
  package with its own argument.

## Considered Options

- **Revive the provider registry and pull adapters (ADR-0014, ADR-0020).** Rejected: that machinery
  was removed because endpoint-scoped ownership (ADR-0022) made instance-wide pull ingestion the
  wrong trust posture. A reader the caller owns and configures keeps the door closed.
- **Per-provider interfaces, with the caller branching on provider type.** Rejected: the caller's
  need is one — walk what changed since `since`. One interface with one method keeps the caller
  provider-agnostic; per-provider interfaces push each provider's cursor shape (a link URL vs a page
  pair) into the caller.
- **Expose the provider's own cursor shape to the caller.** Rejected: the Gitea cursor is a pair of
  page numbers, the GitHub one is a URL. Publishing either couples the caller to the implementation
  and gives the cursor a lifetime the walk cannot guarantee; an opaque token, refused when it did
  not come back from the same client, is cheaper.
- **The client reads a credential and authenticates on the caller's behalf.** Rejected: it would put
  a provider token in this server's memory and config — precisely the credential path the reconcile
  design has to earn separately (#587). Transport injection keeps the package secret-free.
- **Read through the search endpoints with an updated-after filter instead of the repository
  listings.** Rejected: search is a different rate-limit class with different ordering and no stable
  walk contract, and it does not guarantee the floor is applied identically; the listings are the
  stable, pageable surface on both providers.

## Consequences

**Gains.** The reconcile pass (#616) gets a provider-agnostic read with an honest error model: 403
means the token drifted, 404 means the repository moved, 429 carries a `Retry-After` the caller can
sleep on. The httptest suites pin the behaviors that are easy to regress — merged-stream ordering,
PR dedup, the `since` cutoff, cursor rejection — without a live provider. Nothing new is stored, so
there is no new retention question, and no credential enters the server.

**Costs and limits.** A walk is only as bounded as the `since` the caller passes: a zero-`since`
`ListRecentlyUpdated` walks to the end of the stream. The `since` floor is enforced client-side in
both implementations — pages are newest first, and the walk (or a Gitea stream) ends as soon as a
fetched page reaches past it — with the floor additionally pushed into GitHub's API and Gitea's
issue query. The Gitea merge fetches both streams per call, and a stream's "exhausted" call is
count-based: a page that returns a full page is assumed to have a successor until one does not.
`State` is verbatim — the caller, not this package, knows what "closed" means on each provider. A
new provider must live in `internal/provider` to reuse the shared HTTP discipline, and a provider
whose API cannot honor that discipline needs its own package and its own argument.

**Out of scope, on purpose.** No ingest, no server, no config. Whether a todo synthesized from a
list result is trusted is a connection-config question (#614), not one this package answers; and
the credential path the reconcile caller will need is #587's to design.
