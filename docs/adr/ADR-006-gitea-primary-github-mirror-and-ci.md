---
status: proposed
date: 2026-07-05
decision-makers: Joe Stump
related: [ADR-000, ADR-001, ADR-004]
---

# ADR-006: Gitea-Primary / GitHub-Mirror Repository and CI Approach

## Context and Problem Statement

`webhook-mcp` must be hosted and continuously checked following the established StumpCloud conventions, not as a one-off. Those conventions were confirmed by inspecting the existing Gitea instance (`gitea.stump.rocks`): Gitea is the canonical source of truth with GitHub as a push-mirror; workflows run on `act_runner` labeled `ubuntu-latest`; and the "comprehensive" test gate is duplicated on the GitHub mirror. Two brief assumptions turned out to be false and shape this decision: **there is no shared `ci-actions` monorepo** (the house pattern for a shared action is a Docker container action pinned to a git tag and referenced by full URL, e.g. `stumpcloud/garage-pages-deploy@v1`), and **no existing Python repo** exists to inherit ruff/mypy/bandit config from — this is the first. So: where does the repo live, how is the mirror wired, and how is the Python lint/type/security/test CI structured given there is nothing to inherit?

## Decision Drivers

* **House convention over novelty.** Gitea-primary + GitHub-mirror is load-bearing across StumpCloud (reduit, dotfiles, docker). Match it.
* **The brief's explicit CI ask.** Brief §5.3 wants Gitea Actions CI carrying Python jobs for ruff, mypy, bandit, pip-audit, Gitleaks, and Semgrep. That gate lives on Gitea.
* **Reproducible locally.** House rule (reduit `CLAUDE.md`): the CI gate must be runnable locally (`make ci` / `make lint`) before opening a PR.
* **Fast gate vs. comprehensive gate.** House split: Gitea `act_runner` is more contended, so it runs the fast + security gate and any build; the GitHub mirror runs the comprehensive matrix. Keep both, in lockstep.
* **No secrets in the repo.** CI includes Gitleaks so a stray secret fails the build, backstopping [ADR-004](ADR-004-secrets-management-openbao-approle.md).
* **Green from day one.** This bootstrap has no application code yet; CI must pass on the essentially-empty repo so the follow-up session starts from green.
* **No shared CI-actions repo to lean on.** Since none exists, jobs are inlined; the pattern for later extraction (a tagged Docker action referenced by full URL) is noted but not built now.

## Considered Options

* **Hosting:** (A) **Gitea-primary + GitHub push-mirror** *(chosen, house standard)*; (B) GitHub-primary; (C) Gitea-only.
* **CI placement:** (A) Gitea-only CI; (B) GitHub-only CI; (C) **Gitea fast+security gate + GitHub comprehensive/matrix gate, kept in lockstep** *(chosen)*.
* **Shared vs. inline jobs:** (A) consume a shared `ci-actions` repo (does not exist); (B) **inline the jobs now, note future extraction into a tagged Docker action** *(chosen)*; (C) build the shared CI-actions repo first (out of scope this session).
* **Local reproducibility:** (A) **Makefile targets (`make ci`, `make lint`, `make fmt`) + a `.pre-commit-config.yaml`** *(chosen)*; (B) CI-only, no local mirror.

## Decision Outcome

Chosen: **Gitea-primary + GitHub push-mirror (A)**, **dual CI with a Gitea fast+security gate and a GitHub comprehensive matrix gate kept in manual lockstep (C)**, **inline jobs with a noted extraction path (B)**, and **Makefile + pre-commit for local reproducibility (A)**.

### Hosting and mirror

* **Primary:** `https://gitea.stump.rocks/joestump/webhook-mcp` — canonical, where issues/PRs/CI live.
* **Mirror:** `https://github.com/joestump/webhook-mcp` — a **Gitea push-mirror**, configured as a **Gitea repo setting** (Settings → Mirror Settings, or `POST /repos/joestump/webhook-mcp/push_mirrors`), authenticated with a GitHub PAT stored in Gitea. It is **not** a file in the repo and **not** a workflow that pushes — matching reduit. The mirror is set up once, out-of-band; this ADR records that it must exist and how.
* **Tracker/branch/PR conventions** (house-wide, from reduit `CLAUDE.md`): tracker is Gitea; branches `feat/{n}-{slug}`, `fix/{n}-{slug}`, `chore/…`, `docs/…`, `ci/…`; PR title = issue title, body includes `Closes #N`, target `main`; squash-merge; lifecycle labels `queued → in-progress → in-review → merged`.

### CI structure

Two workflow trees, deliberately kept in lockstep by hand (Dependabot only watches `.github/`, so pinned versions in `.gitea/` are bumped manually — a documented house caveat):

**`.gitea/workflows/ci.yaml` — fast + security gate (the brief's §5.3 gate).** Triggers on push to `main` and on pull requests; `permissions: contents: read`; concurrency group `${{ gitea.workflow }}-${{ gitea.ref }}` with `cancel-in-progress: true`; `runs-on: ubuntu-latest`. A **single sequential job** (reduit's house pattern — the `act_runner` is contended, so one job) running **on the host runner** (not in a `container:` — see the runner constraints below), which installs **Python 3.12 + venv via `sudo apt-get`** (the dotfiles house pattern) and, inside that venv, executes: `ruff check` + `ruff format --check` → `mypy` → `bandit` → `pip-audit` → **Gitleaks** (pinned release binary) → **Semgrep** (scoped rulesets) → `pytest`.

**`.github/workflows/ci.yml` — comprehensive gate on the mirror.** The same checks, restructured as parallel jobs plus a **Python version matrix** (3.12 and 3.13) so cross-version breakage is caught on the less-contended GitHub hosted runners, per the house "canonical comprehensive gate lives on GitHub" convention.

Runner label is `ubuntu-latest` on both (the only `act_runner` label on this instance).

### Runner constraints discovered (why the two workflows provision differently)

Getting to a green Gitea run surfaced several `act_runner` realities that shaped the final workflow. These are the *only* places the two trees legitimately diverge; the *checks* they run are identical.

* **`actions/setup-python` is not reliable on this `act_runner`.** The first run failed all jobs within ~20s at the Python-setup step, while the identical checks passed on the GitHub matrix. So the Gitea workflow does not use `setup-python`; it installs Python 3.12 via `sudo apt-get install python3 python3-venv` on the host image — exactly how the house `dotfiles` repo provisions Python (it `apt-get install`s `python3` and relies on the runner's 3.12). A probe job confirmed the host `python3` is ≥ 3.12.
* **`container:` jobs do not work on this `act_runner`.** Pinning the interpreter via `container: python:3.12-slim-bookworm` was tried and failed: a per-check diagnostic split showed **every** containerized job failing, because the runner cannot execute the Node-based `actions/checkout` inside an arbitrary container. No existing StumpCloud repo uses `container:` — reduit and dotfiles both run on the host image. The Gitea job therefore runs on the host and isolates its Python in a **venv** (`python3 -m venv .venv`), which also sidesteps Debian/Ubuntu's PEP 668 "externally-managed-environment" guard.
* **Gitleaks runs from a pinned release binary, not a `docker://` action-step** (the dotfiles house pattern), scanned with `--no-git`. The GitHub job (GitHub-hosted runner) keeps the `docker://ghcr.io/gitleaks/gitleaks` pinned image, which works there.
* **Semgrep is scoped to code rulesets** (`p/python`, `p/security-audit`) on both hosts rather than `--config auto`. `auto` additionally enforces SHA-pinned action refs, a policy the StumpCloud convention deliberately does not follow (it pins actions to *tags* and lets Dependabot bump them — see the lockstep note above). Scoping semgrep to code keeps it a meaningful SAST gate on the application without imposing a CI-pinning policy that conflicts with house style.

Note also: this Gitea instance does **not** expose the Actions runs/jobs/logs API (`/api/v1/.../actions/runs` and the log endpoints 404); only `/api/v1/.../actions/tasks` is available, and it reports **job-level** status only. Diagnosing a failed Gitea run means either reading logs in the web UI or splitting checks into one-job-each so the failing check is identifiable from job status — there is no log retrieval via API/MCP.

### Why inline, not shared

The brief says "using the shared `ci-actions` repo pattern where applicable" — but no such repo exists on this Gitea, and the one real shared action (`garage-pages-deploy`) is a single-purpose Docker action, not a CI library. So the jobs are inlined into this repo's workflows. The path to later extraction is recorded: if these Python jobs are reused by a second Python repo, promote them into a tagged Docker action (or reusable workflow) referenced by full URL `https://gitea.stump.rocks/stumpcloud/<repo>@vN`, matching the house pattern. Not built this session.

### Local reproducibility

A `Makefile` exposes `make fmt` (ruff format), `make lint` (ruff + mypy + bandit), `make test` (pytest), and `make ci` (the whole gate) so the CI is runnable before opening a PR — the house expectation. A `.pre-commit-config.yaml` runs ruff + mypy on commit locally (brief §5.4). Both mirror the CI job set so local == CI.

### Consequences

* Good, because the repo slots into StumpCloud conventions with zero surprises — same host model, same runner label, same mirror mechanism, same tracker/branch rules.
* Good, because the security gate (Gitleaks, Semgrep, bandit, pip-audit) runs on every push, backstopping the no-secrets-in-repo rule of [ADR-004](ADR-004-secrets-management-openbao-approle.md).
* Good, because local `make ci` + pre-commit make the gate reproducible before PR, reducing red-CI churn.
* Good, because the empty bootstrap passes CI, so the code session starts from green.
* Bad, because two workflow trees must be kept in lockstep by hand (Dependabot blind spot on `.gitea/`) — mitigated by keeping the job sets identical and documenting the caveat in workflow headers, exactly as reduit does.
* Bad, because inlining postpones a shared CI-actions library — acceptable; there is only one Python repo today, and premature extraction would be speculative.
* Bad, because the push-mirror needs a one-time out-of-band setup (GitHub PAT in Gitea) that is not captured in code — inherent to Gitea's mirror feature; documented here and in the README.

### Confirmation

* The repo exists at `gitea.stump.rocks/joestump/webhook-mcp` and the GitHub mirror at `github.com/joestump/webhook-mcp` receives pushes.
* `.gitea/workflows/ci.yaml` and `.github/workflows/ci.yml` exist, use `runs-on: ubuntu-latest`, and run the ruff/mypy/bandit/pip-audit/Gitleaks/Semgrep/pytest job set; both are green on the bootstrap commit.
* `make ci` runs the same gate locally and passes; `.pre-commit-config.yaml` runs ruff + mypy.
* `LICENSE` is MIT with `Copyright (c) 2026 Joe Stump`.

## Pros and Cons of the Options

### Hosting: Gitea-primary + GitHub-mirror (chosen) vs. GitHub-primary vs. Gitea-only

* Good (chosen), because it is the established house model — canonical self-hosted source, GitHub as backup/reach.
* Bad (GitHub-primary), because it inverts the house convention and moves the source of truth off the homelab.
* Bad (Gitea-only), because it drops the off-site backup and public-reachability the mirror provides.

### CI placement: dual gate (chosen) vs. Gitea-only vs. GitHub-only

* Good (dual), because the fast/security gate runs close to the source while the comprehensive matrix runs on the less-contended mirror — the house split.
* Bad (Gitea-only), because `act_runner` contention makes a broad matrix slow, and it drops the mirror's comprehensive gate.
* Bad (GitHub-only), because it violates the brief's explicit "Gitea Actions CI with the Python jobs" requirement and leaves the canonical host uncheck-gated.

### Shared vs. inline jobs: inline now (chosen) vs. consume shared vs. build shared first

* Good (inline), because it ships a working gate today without inventing infrastructure.
* Bad (consume shared), because the shared repo does not exist — not an option.
* Bad (build shared first), because standing up a CI-actions library for a single consumer is premature and out of scope for this session.

### Local reproducibility: Makefile + pre-commit (chosen) vs. CI-only

* Good (chosen), because contributors catch failures before pushing; matches the house `make ci` expectation.
* Bad (CI-only), because every check requires a push and burns contended runner time on avoidable failures.

## Architecture Diagram

```mermaid
flowchart TB
  dev([Developer]) -->|push / PR| gitea[(Gitea PRIMARY<br/>gitea.stump.rocks/joestump/webhook-mcp)]
  gitea -->|.gitea/workflows/ci.yaml<br/>runs-on: ubuntu-latest| gate1[[Fast + Security gate<br/>ruff · mypy · bandit · pip-audit<br/>Gitleaks · Semgrep · pytest]]
  gitea -->|Gitea push-mirror<br/>repo setting + GitHub PAT| gh[(GitHub MIRROR<br/>github.com/joestump/webhook-mcp)]
  gh -->|.github/workflows/ci.yml| gate2[[Comprehensive gate<br/>same suite × Python 3.12 / 3.13]]
  gitea -. Dependabot watches ONLY .github;<br/>bump .gitea pins by hand .-> gh
  subgraph local[Local reproducibility]
    mk[make ci / make lint / make fmt]
    pc[.pre-commit-config.yaml]
  end
  dev --- local
  classDef prim fill:#dfe,stroke:#090
  class gitea prim
```

## More Information

* Conventions confirmed by inspecting `joestump/reduit` (Go), `joestump/dotfiles` (Shell), and `stumpcloud/{docker,garage-pages-deploy}` on `gitea.stump.rocks`. There is no `stumpcloud/ci-actions` repo and no prior Python repo; this is the first, so its tool config is authored fresh here.
* Runner label `ubuntu-latest`, Gitleaks pinned Docker image, and Trivy image-scan (not used here — no container image in MVP) are the reusable house security patterns.
* Push-mirror mechanism: Gitea Settings → Mirror Settings / `push_mirrors` API with a GitHub PAT — a one-time setting, not a repo file.
* Related: [ADR-001](ADR-001-web-stack-starlette-htmx-pico.md) (what the `tests` job exercises), [ADR-004](ADR-004-secrets-management-openbao-approle.md) (Gitleaks backstops no-secrets-in-repo).
