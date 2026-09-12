---
status: accepted
date: 2026-09-12
decision-makers: [joestump]
related: [ADR-0002]
---

# ADR-0026: Ship the CI and Deploy Workflows Unchanged When the Repo Goes Public

## Context and Problem Statement

Switchboard is being open-sourced under MIT. Everything in the repository becomes readable by
anyone, including `.gitea/workflows/`, which describes how the project is actually built and
deployed.

Those workflows name real infrastructure. A sweep of the tree found roughly thirty references
across four files:

- `build.yaml` pushes the container image to a named registry host, and its deploy job targets a
  named host, checks out a private playbook repository, and runs an Ansible runner action.
- `docs.yaml` does the same for the documentation image.
- `aibot.yml` and `gitleaks.yaml` pull their actions from that same forge instance.

Also disclosed, in passing: the deploy target's `ssh_hosts` value, and several Vault policy names
appearing in explanatory comments.

The question this ADR settles is whether any of that should change before the repository is made
public.

**The split that makes this a decision rather than a cleanup task:**

| Kind | Count | Disposition |
| --- | --- | --- |
| A comment naming the registry, in a file readers are told to open | 1 | Fixed separately |
| Functional references in `.gitea/workflows/` | ~30 | Accepted, unchanged |

The single cosmetic case was treated differently for one specific reason, and the reasoning does
**not** generalise: the self-hosting guide instructs readers to open `deploy/docker/Dockerfile`,
which makes that file a publicly-read artifact rather than CI configuration. Its header comment
pointed at a host no outside reader can resolve, so it was replaced with generic wording plus a
pointer to the guide. Nothing in the workflows has that property.

The ~30 remaining references cannot be edited away. Rewriting the hostnames breaks the build;
deleting the workflows removes the build. There is no edit that makes the question disappear,
which is why it required a ruling instead of a patch.

## Decision Drivers

* **A map is not a credential.** None of the disclosed values is a secret, and none is exploitable
  on its own. They describe where things live, not how to get in.
* **Publishing CI is normal.** Serious open-source projects ship their pipelines. A reader
  evaluating whether to trust or self-host this software benefits from seeing how it is built,
  scanned, and deployed.
* **The decision is irreversible, so it must be made before publication.** Git history retains
  these references even if the files are scrubbed afterwards. A cleanup commit changes what `main`
  looks like; it does not change what `git log -p` contains.
* **Consistency with the history ruling.** Author emails and internal hostnames in existing commits
  were already accepted as-is, with no rewrite and no squash. Scrubbing the workflows while leaving
  history untouched would buy nothing and cost the build.
* **A future reader must not mistake this for an oversight.** Internal hostnames sitting in a public
  repository look like a mistake. Without a record, someone eventually "fixes" it and breaks the
  pipeline, or proposes a history rewrite to undo something that was chosen deliberately.

## Considered Options

* **(A) Accept the disclosure and ship the workflows unchanged.** *(chosen)*
* **(B) Restructure the pipeline before flipping.** Move the private build and deploy workflows into
  a separate private repository; keep only public-safe workflows (test, lint, secret scan) in the
  public one. *Rejected:* it is real cross-repo work, it splits the build across two places for
  every future contributor, and it buys little while history still carries the same references.
* **(C) Rewrite history before the first public push.** *Rejected, and largely foreclosed already:*
  the separate ruling on git history was to flip as-is. Rewriting history to remove a hostname would
  contradict that decision, invalidate every existing clone and SHA reference, and still leave the
  functional references in the working tree.

## Decision Outcome

Chosen option: **(A)**. Switchboard's `.gitea/workflows/` go public unchanged.

The following become permanently public, in the files and in history: the container registry
hostname, the deploy target hostname and its `ssh_hosts` value, Vault policy names appearing in
comments, and the Ansible runner dependency along with the private playbook repository it checks
out.

This was raised with a deadline attached — before publication, while the choice still existed — and
ruled on deliberately rather than allowed to happen by default.

### Consequences

* **Good:** the build stays in one place, contributors can read the real pipeline, and no work is
  spent on a migration that history would undercut anyway.
* **Bad:** the deployment topology is public and permanently so.
* **Neutral:** operational security for these hosts rests where it already did — on authentication
  and network boundaries, not on their names being unknown.

### What this forecloses

**Do not undo this later.** It is irreversible once the flip happens, and attempting to reverse it
afterwards achieves nothing while breaking things:

* **Do not edit `.gitea/workflows/` on public-hygiene grounds.** Not a hostname, not a comment. A
  future reader finding internal hostnames there has found a decision, not a defect.
* **Do not propose a history rewrite** to remove these references. The values are already public at
  that point, and the rewrite invalidates every clone and every SHA anyone has cited.

The exception is ordinary change: if the pipeline genuinely moves to different infrastructure, the
workflows follow it. That is maintenance, not hygiene.

### What still needs doing at the flip

This decision removes the blocker to publishing; it does not mean everything about publication has
been verified. One item is outstanding and cannot be closed before the flip:

**The self-hosting guide's source-acquisition step is unverified by construction.** It documents how
a reader obtains the source, against a public location that does not resolve while the repository is
private. Nobody can test it today. The moment the repository is public, someone must run the clone
*as an outside reader would* — not from a machine that already holds the source, and not by reading
the instructions and judging them plausible. Until that happens, the install path is documented but
unproven, and it should not be described as working.

The same caution applies to anything else whose correctness depends on the repository being
reachable: a green docs build proves the page renders, not that the commands on it succeed for
someone who has never seen this network.

### How this decision was made

This ADR began as an open question. A sweep of the tree during documentation work surfaced the ~30
references, the finding was escalated rather than acted on, and it was filed as an issue requesting
a ruling — explicitly not a proposal, with three options listed unranked and none recommended. Joe
ruled to accept. The issue was then rewritten as a record of the ruling it received, and this ADR
supersedes it as the durable home.

That sequence is recorded because how a decision was reached is often the load-bearing part: it
tells a future reader what was considered, what was not, and that the outcome was chosen rather
than defaulted into. A decision record that reads as though it had always been a decision record
conceals exactly that.

### Related

* Switchboard issue #233 on the private Gitea tracker holds the original analysis and the ruling.
  It is closed as a record; this ADR is the durable version. The issue is not linkable from a public
  artifact, since that tracker is not reachable outside the network.
* ADR-0002 (PostgreSQL as the persistence layer) is the standing record for how this project holds
  secrets: switchboard-minted credentials are stored hashed, and provider secrets are injected from
  deployment config rather than persisted. `internal/config/config.go` cites it as governing for
  `SWITCHBOARD_SECRET_ENCRYPTION_KEY`, though ADR-0002's own text does not describe the at-rest
  encryption envelope for self-managed webhook signing secrets. Whether an absent key should remain
  silent is an open question tracked separately. Either way it concerns a different class of data
  and is unaffected by this decision.
