#!/usr/bin/env bash
# Generate categorized English release notes for a release tag.
#
# Usage: release_notes.sh <tag> <mode>
#   mode "stable" - the diff base is the nearest ancestor STABLE tag
#                   (vX.Y.Z); commits already shipped in pre-releases are
#                   listed again in the stable release.
#   mode "any"    - the diff base is the nearest ancestor tag of any kind
#                   (used for pre-releases such as v1.2.3-rc.1).
#
# Environment:
#   GITHUB_REPOSITORY - owner/name used for commit links; falls back to
#                       the origin remote for local runs.
#
# The finished notes are printed to stdout; progress goes to stderr.
set -euo pipefail

TAG="${1:?usage: release_notes.sh <tag> <stable|any>}"
MODE="${2:-any}"
REPO="${GITHUB_REPOSITORY:-}"

if [ -z "$REPO" ]; then
  REPO="$(git remote get-url origin 2>/dev/null | sed -E 's#(git@|https://)github\.com[:/]##; s#\.git$##')"
fi
REPO="${REPO:-local/repo}"

# The tag may not exist yet (workflow_dispatch creates it on the current
# commit during the release step); fall back to HEAD for the log range.
if git rev-parse -q --verify "refs/tags/${TAG}^{commit}" >/dev/null 2>&1; then
  TAG_SHA="$(git rev-parse "refs/tags/${TAG}^{commit}")"
else
  TAG_SHA="$(git rev-parse HEAD)"
fi

# Stable tags are plain vX.Y.Z; anything with a semver suffix (v1.2.3-rc.1)
# counts as a pre-release.
is_stable_tag() {
  case "$1" in
    v[0-9]*.[0-9]*.[0-9]*) ;;
    *) return 1 ;;
  esac
  # The glob above is loose ("*" matches "-rc.1" too), so reject anything
  # carrying a semver pre-release suffix explicitly.
  [ "${1#*-}" = "$1" ]
}
# Previous tag: nearest ancestor of the release commit, excluding the
# release tag itself. Stable mode skips pre-release tags so that a stable
# release covers everything since the last stable one.
PREV=''
best_dist=''
# Nearest ancestor tag by topology (commit distance), not by tag date:
# same-second tags make date sorts unreliable, and version sorts rank
# pre-releases (v1.0.0-rc.1) behind their own stable tag (v1.0.0).
# Stable mode only considers stable tags so a stable release covers
# everything since the last stable one.
while IFS= read -r candidate; do
  [ "$candidate" = "$TAG" ] && continue
  if [ "$MODE" = stable ] && ! is_stable_tag "$candidate"; then
    continue
  fi
  git merge-base --is-ancestor "$candidate" "$TAG_SHA" 2>/dev/null || continue
  dist="$(git rev-list --count "$candidate..$TAG_SHA")"
  if [ -z "$best_dist" ] || [ "$dist" -lt "$best_dist" ]; then
    best_dist="$dist"
    PREV="$candidate"
  fi
done < <(git tag)

RANGE_ARGS=("$TAG_SHA")
if [ -n "$PREV" ]; then
  RANGE_ARGS=("$PREV..$TAG_SHA")
  echo "release range: $PREV..$TAG_SHA" >&2
else
  echo "release range: full history" >&2
fi

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# tformat (not format) keeps a trailing newline after the last record;
# without it read hits EOF and silently drops the final entry, which used
# to make the very first commit vanish on first-release history.
while IFS="$(printf '\t')" read -r hash subject; do
  [ -n "${hash:-}" ] || continue

  kind='other'; scope=''; bang=''; summary="$subject"
  # Keep the regex in a variable: parsed as ERE everywhere, immune to the
  # parser quirks around unquoted parens inside [[ =~ ]] on some bashes.
  re='^([a-zA-Z]+)(\(([^)]*)\))?(!)?:[[:space:]]*(.*)$'
  if [[ "$subject" =~ $re ]]; then
    kind="$(printf '%s' "${BASH_REMATCH[1]}" | tr '[:upper:]' '[:lower:]')"
    scope="${BASH_REMATCH[3]:-}"
    bang="${BASH_REMATCH[4]:-}"
    summary="${BASH_REMATCH[5]:-}"
  fi
  # git auto-reverts ("Revert \"feat: ...\"") get their own bucket too.
  case "$subject" in
    'Revert "'*) kind='revert' ;;
  esac

  line="- "
  [ -n "$scope" ] && line+="**$scope**: "
  line+="$summary ([\`$hash\`](https://github.com/$REPO/commit/$hash))"
  printf '%s\n' "$line" >> "$work/$kind"

  # Breaking changes: "!" after the type and/or a BREAKING CHANGE footer
  # in the commit body. awk does the whole scan in one process - no
  # sed|head pipe that could SIGPIPE and, with pipefail on, fail the step.
  body="$(git log -1 --pretty=%b "$hash")"
  note="$(printf '%s\n' "$body" | awk '/^[[:space:]]*BREAKING[ -]CHANGE[[:space:]]*:/ { sub(/^[[:space:]]*BREAKING[ -]CHANGE[[:space:]]*:[[:space:]]*/, ""); print; exit }')"
  if [ -n "$bang" ] || [ -n "$note" ]; then
    printf '%s\n' "$line" >> "$work/breaking"
    [ -n "$note" ] && printf '  %s\n' "$note" >> "$work/breaking"
  fi
done < <(git log --no-merges --pretty=tformat:'%h%x09%s' "${RANGE_ARGS[@]}")

{
  # Breaking changes lead the notes but stay in their original sections
  # as well: impossible to miss, yet the feature list stays complete.
  if [ -s "$work/breaking" ]; then
    printf '## Breaking Changes\n\n'
    cat "$work/breaking"
    printf '\n'
  fi

  emit() {
    [ -s "$work/$1" ] || return 0
    printf '## %s\n\n' "$2"
    cat "$work/$1"
    printf '\n'
  }
  emit feat     'Features'
  emit fix      'Bug Fixes'
  emit perf     'Performance'
  emit refactor 'Refactor'
  emit revert   'Revert'
  emit docs     'Docs'
  emit test     'Tests'
  emit build    'Build'
  emit ci       'CI'
  emit style    'Style'
  emit chore    'Chores'
  # Commits that do not follow Conventional Commits still show up under
  # their own heading - silently dropping them is much harder to audit.
  emit other    'Other Changes'

  if [ ! -s "$work/feat" ] && [ ! -s "$work/fix" ] && [ ! -s "$work/breaking" ] \
    && [ ! -s "$work/other" ] && [ ! -s "$work/perf" ] && [ ! -s "$work/refactor" ] \
    && [ ! -s "$work/revert" ] && [ ! -s "$work/docs" ] && [ ! -s "$work/test" ] \
    && [ ! -s "$work/build" ] && [ ! -s "$work/ci" ] && [ ! -s "$work/style" ] \
    && [ ! -s "$work/chore" ]; then
    printf 'No changes in this release.\n'
  fi

  if [ -n "$PREV" ]; then
    printf '**Full Changelog**: https://github.com/%s/compare/%s...%s\n' "$REPO" "$PREV" "$TAG"
  fi
}
