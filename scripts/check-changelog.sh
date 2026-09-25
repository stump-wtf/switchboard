#!/usr/bin/env bash
# check-changelog.sh — the CHANGELOG and upgrade-note rules for a pull request (SPEC-0027 REQ-9,
# REQ-10, REQ-11; release-contract design.md "CHANGELOG enforcement is a small, local script").
#
# Usage: scripts/check-changelog.sh changelog|upgrade-note|all
#
# Inputs, from the environment (CI passes them down from the calling workflow, because a reusable
# Gitea workflow loses the event payload):
#   PR_TITLE    the pull request title, e.g. "feat(mcp): add ack_doorbell"      (required)
#   PR_LABELS   comma-separated label names, e.g. "feature,no-changelog"         (optional)
#   BASE_REF    the ref the PR merges into                        (default: origin/main)
#
# Rules:
#   changelog     feat, fix, sec and perf titles, any "!:" title, and any PR that adds a destructive
#                 migration must add a line under "## [Unreleased]" in CHANGELOG.md. docs, toil,
#                 test, chore and Renovate's chore(deps) pass without one. The no-changelog label
#                 waives this check, visibly.
#   upgrade-note  a "!:" title, or a new migration containing DROP TABLE, DROP COLUMN or DELETE FROM,
#                 must change docs/guides/15-upgrading.md AND add an entry under "### Breaking" in
#                 [Unreleased]. No label waives it.
#
# Exit status: 0 when every requested check passes, 1 when one fails, 2 on a usage error.
set -euo pipefail

CHANGELOG=CHANGELOG.md
UPGRADING=docs/guides/15-upgrading.md
MIGRATIONS=internal/db/migrations/

check="${1:-all}"
case "$check" in
  changelog | upgrade-note | all) ;;
  *) echo "usage: $0 changelog|upgrade-note|all" >&2; exit 2 ;;
esac
title="${PR_TITLE:?PR_TITLE is required}"
labels=",${PR_LABELS:-},"
base="${BASE_REF:-origin/main}"
range="$base...HEAD"

# The conventional-commit type ("feat" from "feat(mcp)!: …") and whether the title marks a break.
type=$(printf '%s' "$title" | sed -E 's/^([A-Za-z]+).*/\1/' | tr '[:upper:]' '[:lower:]')
breaking=false
if printf '%s' "$title" | grep -Eq '^[A-Za-z]+(\([^)]*\))?!:'; then breaking=true; fi

# New migrations that destroy data make a change breaking whatever its title says.
destructive=""
while IFS= read -r f; do
  [ -n "$f" ] || continue
  if git show "HEAD:$f" | grep -Eiq 'DROP[[:space:]]+TABLE|DROP[[:space:]]+COLUMN|DELETE[[:space:]]+FROM'; then
    destructive="$destructive $f"
  fi
done < <(git diff --name-only --diff-filter=A "$range" -- "$MIGRATIONS")

# added_lines_in START END: true when the diff adds a non-blank CHANGELOG line whose new-file line
# number falls in [START, END].
added_lines_in() {
  git diff -U0 "$range" -- "$CHANGELOG" | awk -v lo="$1" -v hi="$2" '
    /^@@/ { split($3, a, /[+,]/); n = a[2]; next }
    /^\+\+\+/ { next }
    /^\+/ { if (n >= lo && n <= hi && $0 !~ /^\+[[:space:]]*$/) found = 1; n++; next }
    /^ / { n++ }
    END { exit found ? 0 : 1 }'
}

# section_range HEADING_REGEX LEVEL [WITHIN_START WITHIN_END]: prints "start end" line numbers of the
# section whose heading matches, in the HEAD CHANGELOG. It ends before the next heading of the same
# or a higher level. Prints nothing when the heading is absent. The regex travels through the
# environment because awk -v would eat its backslashes.
section_range() {
  git show "HEAD:$CHANGELOG" 2>/dev/null | SECTION_RE="$1" awk -v lvl="$2" -v lo="${3:-1}" -v hi="${4:-999999}" '
    NR < lo || NR > hi { next }
    start && match($0, /^#+ /) && RLENGTH - 1 <= lvl { print start, NR - 1; done = 1; exit }
    !start && $0 ~ ENVIRON["SECTION_RE"] { start = NR }
    END { if (start && !done) print start, (hi < NR ? hi : NR) }'
}

has_unreleased_entry() {
  local lo hi
  read -r lo hi < <(section_range '^## \[Unreleased\]' 2) || return 1
  added_lines_in "$lo" "$hi"
}

has_breaking_entry() {
  local ulo uhi lo hi
  read -r ulo uhi < <(section_range '^## \[Unreleased\]' 2) || return 1
  read -r lo hi < <(section_range '^### Breaking' 3 "$ulo" "$uhi") || return 1
  added_lines_in "$lo" "$hi"
}

changes_upgrading() {
  git diff --name-only "$range" -- "$UPGRADING" | grep -q .
}

failed=0

if [ "$check" = changelog ] || [ "$check" = all ]; then
  needs=false
  case "$type" in feat | fix | sec | perf) needs=true ;; esac
  if $breaking || [ -n "$destructive" ]; then needs=true; fi
  if ! $needs; then
    echo "changelog: ok — a \"$type\" PR needs no CHANGELOG entry"
  elif [[ "$labels" == *",no-changelog,"* ]]; then
    echo "changelog: waived by the no-changelog label"
  elif has_unreleased_entry; then
    echo "changelog: ok — found a new line under ## [Unreleased]"
  else
    cat >&2 <<EOF
changelog: FAIL — "$title" needs a CHANGELOG entry.
  Add at least one line under "## [Unreleased]" in $CHANGELOG, in the section that fits:
  Added, Changed, Fixed, Security, or Breaking (SPEC-0027 REQ-9). One bullet per change, written
  for the operator who upgrades. If this change truly needs none, add the no-changelog label; that
  waiver is visible in review.
EOF
    failed=1
  fi
fi

if [ "$check" = upgrade-note ] || [ "$check" = all ]; then
  if ! $breaking && [ -z "$destructive" ]; then
    echo "upgrade-note: ok — not a breaking change"
  else
    why="the title marks a breaking change (\"!:\")"
    [ -n "$destructive" ] && why="it adds a migration that destroys data:$destructive"
    missing=""
    changes_upgrading || missing="$missing
  - a section in $UPGRADING under \"## Upgrading to <next version>\" (\"Unreleased\" until the
    release): what breaks, who is affected, the steps to take, and how to verify it worked"
    has_breaking_entry || missing="$missing
  - an entry under \"### Breaking\" in the [Unreleased] section of $CHANGELOG"
    if [ -z "$missing" ]; then
      echo "upgrade-note: ok — the upgrade guide and a ### Breaking entry are both in this PR"
    else
      cat >&2 <<EOF
upgrade-note: FAIL — this PR is breaking because $why.
It needs (SPEC-0027 REQ-10; no label waives this):$missing
An irreversible migration must be named there with a "back up first" step and the pg_dump command.
Pre-1.0, remove a superseded surface outright in this same change, with no alias, shim or
retired-variable warning, and name both the removed surface and its replacement in the Breaking
entry and the upgrade note (REQ-11).
EOF
      failed=1
    fi
  fi
fi

exit "$failed"
