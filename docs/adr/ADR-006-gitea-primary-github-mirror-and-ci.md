---
status: proposed
date: 2026-07-05
decision-makers: Joe Stump
related: [ADR-000, ADR-001, ADR-004, ADR-015]
---

# ADR-006: Gitea-Primary / GitHub-Mirror Repository and CI Approach

## Context and Problem Statement

`switchboard` must be hosted and continuously checked following the established StumpCloud conventions, not as a one-off. Those conventions were confirmed by inspecting the existing Gitea instance (`gitea.stump.rocks`): Gitea is the canonical source of truth with GitHub as a push-mirror; workflows run on `act_runner` labeled `ubuntu-latest`; and the "comprehensive" test gate is duplicated on the GitHub mirror. switchboard is written in **Go** ([ADR-015](ADR-015-implementation-language-go.md)), so the CI is a **Go** lint/vet/security/test gate — and there is a direct in-house precedent: **`joestump/reduit` is a Go repo whose CI is already green on this `act_runner`**, so the Go toolchain runs here without the provisioning gymnastics a Python gate would need. One house assumption is worth stating: **there is no shared `ci-actions` monorepo** (the house pattern for a shared action is a Docker container action pinned to a git tag and referenced by full URL, e.g. `stumpcloud/garage-pages-deploy@v1`). So: where does the repo live, how is the mirror wired, and how is the Go lint/vet/security/test CI structured?

## Decision Drivers

* **House convention over novelty.** Gitea-primary + GitHub-mirror is load-bearing across StumpCloud (reduit, dotfiles, docker). Match it.
* **A real Go CI gate.** Gitea Actions CI carries the Go gate — `gofmt`, `go vet`, `golangci-lint`, `govulncheck`, `go test`, plus Gitleaks and Semgrep. That gate lives on Gitea.
* **Reproducible locally.** House rule (reduit `CLAUDE.md`): the CI gate must be runnable locally (`make ci` / `make lint`) before opening a PR.
* **Fast gate vs. comprehensive gate.** House split: Gitea `act_runner` is more contended, so it runs the fast + security gate and the build; the GitHub mirror runs the comprehensive matrix. Keep both, in lockstep.
* **No secrets in the repo.** CI includes Gitleaks so a stray secret fails the build, backstopping [ADR-004](ADR-004-secrets-management-openbao-approle.md).
* **Green from day one.** This bootstrap has no application code yet; CI must pass on the essentially-empty module so the follow-up session starts from green.
* **Follow the Go precedent.** `reduit` (Go) already shows how a Go gate runs green on this runner; inherit that shape rather than reinventing it.

## Considered Options

* **Hosting:** (A) **Gitea-primary + GitHub push-mirror** *(chosen, house standard)*; (B) GitHub-primary; (C) Gitea-only.
* **CI placement:** (A) Gitea-only CI; (B) GitHub-only CI; (C) **Gitea fast+security gate + GitHub comprehensive/matrix gate, kept in lockstep** *(chosen)*.
* **Shared vs. inline jobs:** (A) consume a shared `ci-actions` repo (does not exist); (B) **inline the jobs now, note future extraction into a tagged Docker action / reusable workflow** *(chosen)*; (C) build the shared CI-actions repo first (out of scope).
* **Local reproducibility:** (A) **Makefile targets (`make ci`, `make lint`, `make fmt`, `make build`) + a `.pre-commit-config.yaml`** *(chosen)*; (B) CI-only, no local mirror.

## Decision Outcome

Chosen: **Gitea-primary + GitHub push-mirror (A)**, **dual CI with a Gitea fast+security gate and a GitHub comprehensive matrix gate kept in manual lockstep (C)**, **inline jobs with a noted extraction path (B)**, and **Makefile + pre-commit for local reproducibility (A)**.

### Hosting and mirror

* **Primary:** `https://gitea.stump.rocks/joestump/switchboard` — canonical, where issues/PRs/CI live.
* **Mirror:** `https://github.com/joestump/switchboard` — a **Gitea push-mirror**, configured as a **Gitea repo setting** (Settings → Mirror Settings, or `POST /repos/joestump/switchboard/push_mirrors`), authenticated with a GitHub PAT stored in Gitea. It is **not** a file in the repo and **not** a workflow that pushes — matching reduit. The mirror is set up once, out-of-band; this ADR records that it must exist and how.
* **Tracker/branch/PR conventions** (house-wide, from reduit `CLAUDE.md`): tracker is Gitea; branches `feat/{n}-{slug}`, `fix/{n}-{slug}`, `chore/…`, `docs/…`, `ci/…`; PR title = issue title, body includes `Closes #N`, target `main`; squash-merge; lifecycle labels `queued → in-progress → in-review → merged`.

### CI structure

Two workflow trees, deliberately kept in lockstep by hand (Dependabot only watches `.github/`, so pinned versions in `.gitea/` are bumped manually — a documented house caveat):

**`.gitea/workflows/ci.yaml` — fast + security gate.** Triggers on push to `main` and on pull requests; `permissions: contents: read`; concurrency group `${{ gitea.workflow }}-${{ gitea.ref }}` with `cancel-in-progress: true`; `runs-on: ubuntu-latest`. A **single sequential job** (reduit's house pattern — the `act_runner` is contended, so one job) running **on the host runner** (not in a `container:` — see the runner note below). It sets up Go via `actions/setup-go` (pinned toolchain, matching reduit) and executes: `gofmt -l` (fail on unformatted) → `go vet ./...` → **`golangci-lint`** → **`govulncheck`** → **Gitleaks** (pinned release binary) → **Semgrep** (scoped rulesets) → `go test ./...` → `go build` (produce the static binary as a smoke check).

**`.github/workflows/ci.yml` — comprehensive gate on the mirror.** The same checks, restructured as parallel jobs plus a **Go version matrix** (current `stable` and previous `oldstable`) so toolchain-version breakage is caught on the less-contended GitHub hosted runners, per the house "canonical comprehensive gate lives on GitHub" convention.

Runner label is `ubuntu-latest` on both (the only `act_runner` label on this instance).

### Runner notes

* **Go runs on the host runner, not in a `container:`.** No existing StumpCloud repo uses `container:` — reduit and dotfiles both run on the host image, and this `act_runner` cannot execute the Node-based `actions/checkout` inside an arbitrary container. Go needs no interpreter/venv provisioning: `actions/setup-go` (or the host Go) on the host image is exactly how reduit stays green here, so switchboard follows it. The resulting artifact is a single static binary — nothing to install at runtime.
* **Gitleaks runs from a pinned release binary, not a `docker://` action-step** (the dotfiles house pattern), scanned with `--no-git`. The GitHub job (GitHub-hosted runner) keeps the `docker://ghcr.io/gitleaks/gitleaks` pinned image, which works there.
* **Semgrep is scoped to code rulesets** (`p/golang`, `p/security-audit`) on both hosts rather than `--config auto`. `auto` additionally enforces SHA-pinned action refs, a policy the StumpCloud convention deliberately does not follow (it pins actions to *tags* and lets Dependabot bump them). Scoping semgrep to code keeps it a meaningful SAST gate on the application without imposing a CI-pinning policy that conflicts with house style.

Note also: this Gitea instance does **not** expose the Actions runs/jobs/logs API (`/api/v1/.../actions/runs` and the log endpoints 404); only `/api/v1/.../actions/tasks` is available, and it reports **job-level** status only. Diagnosing a failed Gitea run means either reading logs in the web UI or splitting checks into one-job-each so the failing check is identifiable from job status — there is no log retrieval via API/MCP.

### Why inline, not shared

No shared `ci-actions` repo exists on this Gitea, and the one real shared action (`garage-pages-deploy`) is a single-purpose Docker action, not a CI library. So the jobs are inlined into this repo's workflows. The path to later extraction is recorded: if this Go gate is reused by a second Go repo, promote it into a tagged Docker action (or reusable workflow) referenced by full URL `https://gitea.stump.rocks/stumpcloud/<repo>@vN`, matching the house pattern. Not built now.

### Local reproducibility

A `Makefile` exposes `make fmt` (`gofmt -w`), `make lint` (`go vet` + `golangci-lint`), `make test` (`go test ./...`), `make build` (compile the binary with embedded assets), and `make ci` (the whole gate) so the CI is runnable before opening a PR — the house expectation. A `.pre-commit-config.yaml` runs `gofmt` + `go vet` on commit locally. Both mirror the CI job set so local == CI.

### Consequences

* Good, because the repo slots into StumpCloud conventions with zero surprises — same host model, same runner label, same mirror mechanism, same tracker/branch rules, and the same Go-on-host CI shape reduit already proves green.
* Good, because the security gate (Gitleaks, Semgrep, `govulncheck`) runs on every push, backstopping the no-secrets-in-repo rule of [ADR-004](ADR-004-secrets-management-openbao-approle.md).
* Good, because local `make ci` + pre-commit make the gate reproducible before PR, reducing red-CI churn.
* Good, because the empty bootstrap passes CI, so the code session starts from green — and a Go gate has no interpreter-provisioning fragility to get there.
* Bad, because two workflow trees must be kept in lockstep by hand (Dependabot blind spot on `.gitea/`) — mitigated by keeping the job sets identical and documenting the caveat in workflow headers, exactly as reduit does.
* Bad, because inlining postpones a shared CI-actions library — acceptable; premature extraction for a single consumer would be speculative.
* Bad, because the push-mirror needs a one-time out-of-band setup (GitHub PAT in Gitea) that is not captured in code — inherent to Gitea's mirror feature; documented here and in the README.

### Confirmation

* The repo exists at `gitea.stump.rocks/joestump/switchboard` and the GitHub mirror at `github.com/joestump/switchboard` receives pushes.
* `.gitea/workflows/ci.yaml` and `.github/workflows/ci.yml` exist, use `runs-on: ubuntu-latest`, and run the `gofmt`/`go vet`/`golangci-lint`/`govulncheck`/Gitleaks/Semgrep/`go test`/`go build` job set; both are green on the bootstrap commit.
* `make ci` runs the same gate locally and passes; `.pre-commit-config.yaml` runs `gofmt` + `go vet`.
* `LICENSE` is MIT with `Copyright (c) 2026 Joe Stump`.

## Pros and Cons of the Options

### Hosting: Gitea-primary + GitHub-mirror (chosen) vs. GitHub-primary vs. Gitea-only

* Good (chosen), because it is the established house model — canonical self-hosted source, GitHub as backup/reach.
* Bad (GitHub-primary), because it inverts the house convention and moves the source of truth off the homelab.
* Bad (Gitea-only), because it drops the off-site backup and public-reachability the mirror provides.

### CI placement: dual gate (chosen) vs. Gitea-only vs. GitHub-only

* Good (dual), because the fast/security gate runs close to the source while the comprehensive matrix runs on the less-contended mirror — the house split.
* Bad (Gitea-only), because `act_runner` contention makes a broad matrix slow, and it drops the mirror's comprehensive gate.
* Bad (GitHub-only), because it leaves the canonical host uncheck-gated.

### Shared vs. inline jobs: inline now (chosen) vs. consume shared vs. build shared first

* Good (inline), because it ships a working gate today without inventing infrastructure.
* Bad (consume shared), because the shared repo does not exist — not an option.
* Bad (build shared first), because standing up a CI-actions library for a single consumer is premature and out of scope.

### Local reproducibility: Makefile + pre-commit (chosen) vs. CI-only

* Good (chosen), because contributors catch failures before pushing; matches the house `make ci` expectation.
* Bad (CI-only), because every check requires a push and burns contended runner time on avoidable failures.

## Architecture Diagram

```mermaid
flowchart TB
  dev([Developer]) -->|push / PR| gitea[(Gitea PRIMARY<br/>gitea.stump.rocks/joestump/switchboard)]
  gitea -->|.gitea/workflows/ci.yaml<br/>runs-on: ubuntu-latest| gate1[[Fast + Security gate<br/>gofmt · go vet · golangci-lint · govulncheck<br/>Gitleaks · Semgrep · go test · go build]]
  gitea -->|Gitea push-mirror<br/>repo setting + GitHub PAT| gh[(GitHub MIRROR<br/>github.com/joestump/switchboard)]
  gh -->|.github/workflows/ci.yml| gate2[[Comprehensive gate<br/>same suite × Go stable / oldstable]]
  gitea -. Dependabot watches ONLY .github;<br/>bump .gitea pins by hand .-> gh
  subgraph local[Local reproducibility]
    mk[make ci / make lint / make fmt / make build]
    pc[.pre-commit-config.yaml]
  end
  dev --- local
  classDef prim fill:#dfe,stroke:#090
  class gitea prim
```

## More Information

* Conventions confirmed by inspecting `joestump/reduit` (Go — the CI precedent this follows), `joestump/dotfiles` (Shell), and `stumpcloud/{docker,garage-pages-deploy}` on `gitea.stump.rocks`. There is no `stumpcloud/ci-actions` repo; the gate is authored inline here.
* Runner label `ubuntu-latest`, Gitleaks pinned Docker image, and Trivy image-scan (reusable house patterns; Trivy applies if a container image is later built).
* Push-mirror mechanism: Gitea Settings → Mirror Settings / `push_mirrors` API with a GitHub PAT — a one-time setting, not a repo file.
* Language & toolchain: [ADR-015](ADR-015-implementation-language-go.md). Related: [ADR-001](ADR-001-web-stack-go-htmx-pico.md) (what the `go test`/`go build` jobs exercise), [ADR-004](ADR-004-secrets-management-openbao-approle.md) (Gitleaks backstops no-secrets-in-repo).
