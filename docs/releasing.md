# Releasing Switchboard

This is the procedure for cutting a release. It exists because the release contract is
only as good as the steps someone actually follows at tag time:
[ADR-0032](adrs/ADR-0032-release-version-reporting-and-upgrade-contract.md) and
[SPEC-0027](openspec/specs/release-contract/spec.md) define what a release must contain,
and this page is the order to do it in.

## When to cut one

Two triggers, and either is sufficient:

- **A security change.** A release within 48 hours of merging any `sec:` (or `sec!:`)
  commit to `main`. This is not a target to negotiate; a fix that is merged and unreleased
  is not a fix for anyone running the published image.
- **Drift.** Whenever `main` holds a user-visible change and the last release is more than
  14 days old.

If the contract is not yet enforced in CI and a security fix needs to ship first, cut the
release by hand with the CHANGELOG and upgrade note written manually and let the
enforcement catch up. Shipping the fix is the priority.

## Before you tag

1. **Confirm `main` is green**, including the image build. A release is cut from a commit,
   and every check that gates `main` must have passed on it. Do not tag a commit whose
   build is red; the tag would publish a version that cannot run.

2. **Move `[Unreleased]` to the new version.** In `CHANGELOG.md`, add a section for the
   version with today's date, move the accumulated entries into it, and leave an empty
   `[Unreleased]` behind. The entries are written by a person from the commit history —
   `git log <last-tag>..origin/main` — not generated from commit subjects, because a
   commit subject describes the change to the person who made it and not the change to
   the person reading the notes.

   ```sh
   git log --oneline "$(git describe --tags --abbrev=0)"..origin/main
   ```

3. **Write the upgrade note** in `docs/guides/15-upgrading.md` if anything in the release
   is breaking. A breaking change is required to have one, and the note has to say what
   breaks, who is affected, and what to do about it — up front, not in a footnote. Name
   every removed environment variable, verb, route or setting with its replacement. If a
   migration in the release cannot be reversed, say so and give the backup command.

4. **Check the release is honest about itself.** Anything the docs describe that is not in
   this release must be marked as such, and no document may carry a marker for unreleased
   behaviour at tag time. This is the release-honest-docs requirement, and it is the one
   that quietly rots: a doc that describes unreleased behaviour is worse than a missing
   doc, because a reader trusts it.

5. **Confirm the release token is provisioned.** The release workflow requires
   `GH_RELEASE_TOKEN` and fails deliberately without it. It is a GitHub PAT with
   `contents:write` on `stump-wtf/switchboard`, and it is a different token from Cairn's.
   A tag that publishes nothing while reporting green is the specific failure this check
   exists to prevent.

## Tag

```sh
git fetch origin
git switch --detach origin/main
git tag -a v0.3.0 -m "v0.3.0"
git push origin v0.3.0
```

Tag the commit you verified, not a local branch tip.

## After you tag

Pushing the tag triggers the `release` workflow, which runs goreleaser, publishes the
GitHub Release and its archives, and asserts the built binary reports the tag rather than
`dev`. Separately, the build workflow publishes the container images.

1. **Watch the release workflow** and confirm it succeeded. If `GH_RELEASE_TOKEN` is
   missing it exits 1 before doing anything, by design.
2. **Confirm the release exists** at
   https://github.com/stump-wtf/switchboard/releases with the CHANGELOG section as its
   notes.
3. **Confirm the images**, by digest rather than by tag name:

   ```sh
   docker buildx imagetools inspect ghcr.io/stump-wtf/switchboard:latest
   docker buildx imagetools inspect ghcr.io/stump-wtf/switchboard:0.3.0
   ```

   The two digests must match, and neither may still be the previous release. `latest`
   only moves on a `v*` tag, so a tag that failed to publish leaves `latest` silently
   pointing at the previous release.

4. **Run the published image, not the workflow status.** The point of the release
   contract is that the version every surface reports is real, and the only way to know is
   to ask a running image:

   ```sh
   docker run --rm ghcr.io/stump-wtf/switchboard:0.3.0 version
   ```

   It must print the tag you pushed. Then confirm the same version on the surfaces a user
   or an agent actually touches: `/healthz`, the web footer, and
   `serverInfo.version` over MCP.

5. **Close the release issue** once every surface has been verified, and add anything the
   run turned up to `docs/releasing.md` if it changed the procedure.

## If a release is wrong

`mode: keep-existing` in `.goreleaser.yaml` means re-running against an existing tag does
not overwrite the notes on a release that already exists, and
`replace_existing_artifacts: true` means a re-run replaces the archives rather than
failing on the first name collision. So a half-finished upload is recovered by re-running
the workflow against the same tag, which is deliberate: recovering a broken release by
deleting and re-pushing the tag means the commit other people have already fetched is no
longer the commit the tag names.

Fix forward. If the released binary is genuinely wrong rather than the release artefacts,
cut the next patch version; do not move the existing tag.
