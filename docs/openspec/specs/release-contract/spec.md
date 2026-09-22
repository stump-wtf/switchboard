---
status: draft
date: 2026-09-22
implements: [ADR-0032]
requires: [SPEC-0005, SPEC-0012, SPEC-0014]
related: [SPEC-0023]
---

# SPEC-0027: Release, Version Reporting and Upgrade Contract

## Overview

Switchboard reports one build-stamped version on every surface that a human or an agent touches.
Each release ships a human-written CHANGELOG section. Each breaking change ships an upgrade note
and a startup warning for any configuration it retired. The docs say which release they describe,
and mark what is not released yet. See
[ADR-0032](../../../adrs/ADR-0032-release-version-reporting-and-upgrade-contract.md).

This spec also defines the `v0.3.0` release as the first release under the contract. It is the
first to report its own version over MCP, and the first with a CHANGELOG and an upgrade guide for
#291.

## Requirements

### REQ-1: Build Information

A single package, `internal/buildinfo`, MUST expose `Version`, `Commit` and `Date`. These MUST be
set at build time with `-X github.com/stump-wtf/switchboard/internal/buildinfo.<Var>=…` by the
Makefile, `deploy/docker/Dockerfile` and `.goreleaser.yaml`. `main.version` MUST be removed.

When no ldflags were applied, `buildinfo` MUST fall back to `runtime/debug.ReadBuildInfo()`:

* `Version` MUST be the main module's version when it is not `(devel)`, otherwise `dev`;
* `Commit` and `Date` MUST come from the `vcs.revision` and `vcs.time` settings when present,
  otherwise be empty.

Packages other than `internal/buildinfo` MUST NOT hold a version literal for Switchboard itself. A test MUST fail if a string
literal matching `^v?\d+\.\d+\.\d+` is assigned to an identifier containing `version` in
`internal/mcp` or `internal/web`. Protocol versions, such as the A2A protocol version, are exempt
by name.

#### Scenario: Release build

- **WHEN** goreleaser builds tag `v0.3.0`
- **THEN** `buildinfo.Version` is `v0.3.0`, `Commit` is the tagged commit, and `Date` is its commit
  date

#### Scenario: go install

- **WHEN** a user runs `go install github.com/stump-wtf/switchboard/cmd/switchboard@v0.3.0` with no
  ldflags
- **THEN** `switchboard version` reports `v0.3.0`

#### Scenario: Plain local build

- **WHEN** a developer runs `go build ./cmd/switchboard` in a clean checkout
- **THEN** `Version` is `dev` and `Commit` is the checkout's revision

### REQ-2: MCP Server Version and Session Instructions

The MCP `initialize` result MUST carry `serverInfo.version = buildinfo.Version`.

The session instructions MUST begin with one line, before the existing doorbell contract text:
`switchboard <Version> (built <YYYY-MM-DD>)`, where the date is `Date` formatted as
`YYYY-MM-DD`. When `Date` is unknown, the parenthetical MUST be omitted.

#### Scenario: Client sees the real version

- **WHEN** a client initializes against a `v0.3.0` server
- **THEN** `serverInfo.version` is `v0.3.0`, and the instructions' first line is
  `switchboard v0.3.0 (built 2026-09-2x)`

### REQ-3: Staleness Warning

When `buildinfo.Date` is known, `Version` is not `dev`, and `Date` is more than 90 days before the
server's current time, the session instructions MUST add, directly after the version line:

> This Switchboard build is more than 90 days old. Features described in current docs may be
> missing. Tell your human to check for an upgrade.

The web footer and `switchboard doctor` MUST show an equivalent notice. The check MUST be offline:
no network access is required.

#### Scenario: Old build

- **GIVEN** a server whose `Date` is 120 days ago
- **WHEN** a session initializes
- **THEN** the instructions contain the staleness line, and the footer shows the notice

#### Scenario: Dev build exempt

- **GIVEN** a `dev` build with a `Date` 200 days ago
- **WHEN** a session initializes
- **THEN** no staleness line appears

### REQ-4: Opt-In Release Check

When the operator sets `SWITCHBOARD_RELEASE_CHECK=1`, and not otherwise, the server:

* MUST fetch `https://api.github.com/repos/stump-wtf/switchboard/tags?per_page=100` at most once
  every 24 hours, plus once at startup. It MUST follow the `Link: rel="next"` pagination, up to 10
  pages, because the endpoint is paged and unsorted, and a single page cannot guarantee the highest
  tag;
* MUST send the request with no credentials, a `user-agent: switchboard/<Version>` header, and no
  instance data;
* MUST cache the highest semver tag.

When the cached tag is greater than `Version`, the session instructions, the footer and
`switchboard doctor` MUST say `<tag> is available`. A failed check MUST be logged at debug level,
and MUST change no output. Without the setting, the server MUST make no request to any release
endpoint.

#### Scenario: Newer release available

- **GIVEN** `SWITCHBOARD_RELEASE_CHECK=1` on a `v0.3.0` server, and a public tag `v0.4.0`
- **WHEN** a session initializes after the check has run
- **THEN** the instructions say `v0.4.0 is available`

#### Scenario: Default makes no request

- **GIVEN** the setting is absent
- **WHEN** the server runs for 48 hours
- **THEN** it makes no request to `api.github.com`

### REQ-5: Health Endpoint

`GET /healthz` MUST stay public and unauthenticated. On success it MUST answer `200` with the
plain-text body `ok <Version>\n`. When the request's `Accept` header prefers `application/json`, it
MUST answer `{"status": "ok", "version", "commit", "date"}`. On a database failure it MUST keep
answering `503` with no version information.

When `SWITCHBOARD_HIDE_VERSION=1` is set, the body MUST be `ok\n` (or `{"status": "ok"}`), and the
footer MUST omit the version. MCP, the CLI and `/metrics` MUST still carry it, because they are
authenticated.

#### Scenario: Probe compatibility

- **WHEN** a load balancer checks that `/healthz` returns `200` and a body starting with `ok`
- **THEN** the check passes on both old and new servers

#### Scenario: Hidden version

- **GIVEN** `SWITCHBOARD_HIDE_VERSION=1`
- **WHEN** an anonymous client requests `/healthz` with `Accept: application/json`
- **THEN** the body is `{"status": "ok"}`

### REQ-6: Web Footer

The layout footer (`internal/web/templates/layout.html`, `role="contentinfo"`) MUST show
`Version`, linked to that version's CHANGELOG section. The short commit MUST be available as
accessible hover text. The footer MUST show the REQ-3 and REQ-4 notices when they apply.

#### Scenario: Footer shows the version

- **WHEN** a signed-in human loads any page of a `v0.3.0` server
- **THEN** the footer shows `v0.3.0`, linked to the `v0.3.0` CHANGELOG section

### REQ-7: CLI Version Reporting

`switchboard version` MUST print the CLI's `Version`, `Commit` and `Date`. When the CLI holds live
credentials, it MUST also print the server's version from `/healthz` (JSON). `switchboard status`
MUST show both versions. When the two differ in major or minor version, `status` and `doctor` MUST
print a warning naming both. `--json` MUST emit `{"cli": {…}, "server": {…} | null}`.

#### Scenario: Skew warning

- **GIVEN** a `v0.4.1` CLI logged in to a `v0.3.0` server
- **WHEN** the human runs `switchboard status`
- **THEN** the output warns that the CLI is `v0.4.1` and the server is `v0.3.0`

### REQ-8: Build Info Metric

The SPEC-0023 registry MUST gain `switchboard_build_info{version,commit} 1`. A version and a
commit are bounded per process, so they are acceptable labels under SPEC-0023 REQ-5.

#### Scenario: Scrape shows the build

- **WHEN** Prometheus scrapes a `v0.3.0` server
- **THEN** `switchboard_build_info{version="v0.3.0",commit="…"} 1` is present

### REQ-9: CHANGELOG

`CHANGELOG.md` MUST exist at the repository root in Keep a Changelog 1.1 format, with an
`## [Unreleased]` section and, per release, `## [vX.Y.Z] - YYYY-MM-DD`, using only the headings
Added, Changed, Fixed, Security and **Breaking**.

A CI job named `changelog` MUST run on every pull request. It MUST fail when:

* the PR title's type is `feat`, `fix`, `sec` or `perf`, or the title contains `!:`; **and**
* the diff does not add at least one line under `## [Unreleased]`; **and**
* the PR does not carry the `no-changelog` label.

The job MUST be a required status check on `main`. Renovate PRs (`chore(deps)`) and `docs`,
`toil`, `test` and `chore` PRs MUST pass without a CHANGELOG change.

At release, the `[Unreleased]` entries MUST move to the new version's section, and the release's
notes (goreleaser `release.body` or equivalent) MUST be that section's text.

#### Scenario: Feature without an entry

- **WHEN** a PR titled `feat(mcp): add ack_doorbell` changes no line under `[Unreleased]`
- **THEN** the `changelog` check fails and names the missing entry

#### Scenario: Dependency bump

- **WHEN** Renovate opens `chore(deps): update module …`
- **THEN** the `changelog` check passes

### REQ-10: Breaking Changes and Upgrade Notes

A change MUST be treated as **breaking** when it:

* removes or renames an environment variable, an MCP verb, field or error code, an API route, or a
  CLI command or flag;
* changes a default that alters behaviour for an existing deployment;
* adds a migration that cannot be reversed (it drops or deletes data).

A breaking PR MUST add:

* a `### Breaking` entry under `[Unreleased]`;
* a section in `docs/guides/15-upgrading.md` under a heading `## Upgrading to <next version>`
  (`Unreleased` until the release), stating what breaks, who is affected, the steps to take, and
  how to verify.

An irreversible migration MUST be named in that section with a "back up first" instruction and the
`pg_dump` command.

A CI job named `upgrade-note` MUST fail a PR whose title contains `!:`, or whose diff adds a file
under `internal/db/migrations/` containing `DROP TABLE`, `DROP COLUMN` or `DELETE FROM`, unless
the PR also changes `docs/guides/15-upgrading.md` and adds a `### Breaking` CHANGELOG entry.

#### Scenario: Breaking PR without a note

- **WHEN** a PR titled `sec!: remove X` changes neither `upgrading.md` nor the Breaking section
- **THEN** the `upgrade-note` check fails

#### Scenario: Irreversible migration

- **WHEN** a PR adds a migration containing `DROP TABLE adapters`
- **THEN** the `upgrade-note` check requires an `upgrading.md` section, which names the migration
  and gives the backup command

### REQ-11: Retired Configuration Warnings

The server MUST hold a table of retired environment variables, each with the version that retired
it and the URL of its upgrade note. At startup, for each retired variable that is set (non-empty),
it MUST log one `WARN` with the variable's name (never its value), the retiring version, and the
URL. It MUST keep starting.

The table MUST initially contain `SWITCHBOARD_GITHUB_SECRET`, `SWITCHBOARD_GITEA_SECRET`,
`SWITCHBOARD_STRIPE_SECRET`, `SWITCHBOARD_SLACK_SECRET` and
`SWITCHBOARD_LEGACY_RECEIVER_ENDPOINT_ID`, all retired in `v0.3.0` (#291). Every breaking PR that
retires a variable MUST add it to the table.

#### Scenario: Old secret still set

- **GIVEN** `SWITCHBOARD_GITEA_SECRET` is set in the environment of a `v0.3.0` server
- **WHEN** the server starts
- **THEN** one `WARN` names `SWITCHBOARD_GITEA_SECRET`, `v0.3.0` and the upgrade-guide URL, the
  secret's value appears nowhere in the logs, and the server starts

### REQ-12: Release-Honest Docs

Every docs-site page MUST show a banner, rendered at build time, that names the latest release tag
and the commit the site was built from. A build from an untagged commit MUST say that the docs
track `main`, and that features marked Unreleased are not in the latest release.

A docs section that describes behaviour added after the latest tag MUST carry an `:::unreleased`
admonition, which renders as "Unreleased: not in `<latest tag>`". The release procedure MUST
rewrite every `:::unreleased` marker to a "Since `<version>`" badge. The docs build MUST fail when it builds
a tagged commit that still contains `:::unreleased`.

The docs workflow MUST also build (without publishing) on every pull request that touches `docs/`
or `docs-site/`, so a broken page or a broken marker fails before merge.

#### Scenario: Docs from main

- **WHEN** the docs are built from `main`, 12 commits after `v0.3.0`
- **THEN** every page's banner names `v0.3.0` and the build commit, and says the docs track `main`

#### Scenario: Leftover marker at release

- **WHEN** a tagged build contains an `:::unreleased` marker
- **THEN** the docs build fails and names the file

### REQ-13: Release Verification

The release workflow MUST assert, for the built linux/amd64 binary:

* that `switchboard version` output contains the tag (the existing check);
* that a server started against a throwaway database answers `/healthz` with `ok <tag>`;
* that an MCP `initialize` against it returns `serverInfo.version` equal to the tag.

Each assertion MUST fail when the assertion itself cannot run, for example when the binary cannot
execute or the server does not start.

#### Scenario: Stamp silently missed

- **WHEN** a release build's ldflags path is wrong, so `Version` stays `dev`
- **THEN** the release workflow fails before publishing

### REQ-14: Release Cadence and the v0.3.0 Release

A release MUST be cut within 48 hours of merging any `sec` change, and SHOULD be cut whenever
`main` holds a user-visible change and the last release is more than 14 days old.

`v0.3.0` MUST be the first release under this contract. It MUST include:

* `CHANGELOG.md`, with `v0.1.0`, `v0.2.0` and `v0.3.0` sections;
* #291 under Breaking;
* the ring-on-connect, delivery-id and board fixes under Added and Fixed;
* a `docs/guides/15-upgrading.md` section for `v0.3.0` covering:
  * the four ignored `SWITCHBOARD_*_SECRET` variables and `SWITCHBOARD_LEGACY_RECEIVER_ENDPOINT_ID`;
  * migration `0021_drop_adapters` being irreversible, with the backup step;
  * moving every sender to `create_webhook`, with a before and after;
  * the public `ghcr.io/stump-wtf/switchboard:latest` tag now carrying these fixes. It moves only
    on `v*` tags, so until `v0.3.0` it is `v0.2.0`.

#### Scenario: v0.3.0 is complete

- **WHEN** `v0.3.0` is tagged
- **THEN** the release notes are the CHANGELOG `v0.3.0` section, the upgrade guide has a `v0.3.0`
  section, and the image and binaries report `v0.3.0` over MCP and `/healthz`

### REQ-15: Error Handling Standards

A failure to read build info MUST fall back as REQ-1 describes, and MUST NOT fail startup. A
release-check failure MUST be logged at debug level with the error wrapped, and MUST NOT change
any output (REQ-4). Retired-variable warnings MUST NOT log values. Nothing in this spec may fail a
request because version information is unavailable.

#### Scenario: Release check times out

- **GIVEN** `SWITCHBOARD_RELEASE_CHECK=1` and `api.github.com` unreachable
- **WHEN** the daily check runs
- **THEN** one debug line is logged, and the instructions and footer are unchanged

## Security Requirements

### Authentication

| Surface | Auth | Description |
|---|---|---|
| `GET /healthz` | Public | Load-balancer and uptime probes must reach it unauthenticated. The version is public by design, and hidden by `SWITCHBOARD_HIDE_VERSION` |
| MCP `initialize` and session instructions | Required | Endpoint credential (SPEC-0007) |
| Web footer | Required | Signed-in human (every layout page is behind `auth.RequireHuman`). The landing page footer follows `SWITCHBOARD_HIDE_VERSION` |
| `GET /metrics` build-info series | Required | SPEC-0023 scrape credential |

### Rate Limiting

`/healthz` stays outside the per-IP limiters, as it is today, for probes. Its body stays constant
and small. The release check makes at most one outbound request per 24 hours, plus one at startup.

### Security Headers

Every surface keeps the existing `secureHeaders` middleware.

### Request Body Size Limits

No new request bodies. The release check MUST read at most 1 MiB of the tags response.

### CSRF Protection

No state-changing surface is added.

### Redirect Validation

The release check MUST NOT follow redirects to a host other than `api.github.com`. Footer links MUST
be same-origin docs links, or the public release URL.

### Outbound Calls

The only outbound call this spec adds is the opt-in release check. It MUST use HTTPS, carry no
credentials, include no tenant or instance identifiers, and time out after 10 seconds.
