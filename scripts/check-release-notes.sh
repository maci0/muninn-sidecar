#!/usr/bin/env bash
# Release-contract gate: the tag and the changelog must agree. `make build`
# stamps the version from `git describe`, so the tag is the only place the
# number lives, and a release published without matching notes cannot be
# corrected afterwards. This check runs before the tag is pushed and again on
# the tag push, so a mismatch is a failed CI run rather than a permanent hole
# in the history.
#
# Usage:
#   scripts/check-release-notes.sh            check the invariants that hold on
#                                            any commit, plus the release rules
#                                            when HEAD carries a tag
#   scripts/check-release-notes.sh v0.5.0     check a specific tag's rules
#
# CHANGELOG=<path> points the check at another file (the test uses fixtures).
# Exits non-zero with one line per problem.

set -euo pipefail

root="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
changelog="${CHANGELOG:-$root/CHANGELOG.md}"
tag="${1:-}"

[ -f "$changelog" ] || { echo "release-notes: no $changelog" >&2; exit 1; }

status=0
fail() { echo "release-notes: $*" >&2; status=1; }

# Every released version the changelog claims, in file order, from the
# "## [0.4.4] - <date>" headings. `sort -V` is the version-aware order, so
# 0.10.0 sorts above 0.9.0 and a string sort cannot hide a misordered file.
versions=$(sed -n 's/^## \[\([0-9][0-9.]*\)]\( — .*\)\?$/\1/p' "$changelog")
[ -n "$versions" ] || fail "no released version headings in $(basename "$changelog")"

[ "$versions" = "$(printf '%s\n' "$versions" | sort -Vr)" ] ||
	fail "released sections are not in descending version order"

# Every version that has a heading needs a link, and every link needs a
# heading: a heading without a link renders as dead text, a link without a
# heading points at a release the notes never describe.
for v in $versions; do
	grep -q "^\[$v\]: " "$changelog" ||
		fail "no link for [$v] at the bottom of $(basename "$changelog")"
done
while read -r v; do
	printf '%s\n' "$versions" | grep -qx "$v" ||
		fail "link [$v] has no released section in $(basename "$changelog")"
done < <(sed -n 's/^\[\([0-9][0-9.]*\)]: .*/\1/p' "$changelog")

# The release procedure leaves an empty [Unreleased] above the new section, so
# a file whose top section is a dated one has lost the landing spot for the
# next entry.
head -1 "$changelog" | grep -q '^# Changelog$' ||
	fail "$(basename "$changelog") does not open with '# Changelog'"
grep -q '^## \[Unreleased\]' "$changelog" ||
	fail "$(basename "$changelog") has no '## [Unreleased]' section"

if [ -z "$tag" ]; then
	# An untagged tree has no version to check against, so the file-level
	# invariants above are the whole answer. Say so rather than exiting
	# silently, or a green run reads as a checked release.
	tag=$(git -C "$root" describe --tags --exact-match HEAD 2>/dev/null || true)
	if [ -z "$tag" ]; then
		echo "release-notes: no tag at HEAD, release rules skipped"
		exit "$status"
	fi
fi

case "$tag" in
v[0-9]*.[0-9]*.[0-9]*) ;;
*) fail "tag '$tag' is not a vMAJOR.MINOR.PATCH tag" ;;
esac
version=${tag#v}

grep -q "^## \[$version\] — " "$changelog" ||
	fail "no '## [$version] - <date>' section for tag $tag; release notes are part of the release"
grep -q "^\[$version\]: " "$changelog" ||
	fail "no '[$version]: <url>' link for tag $tag"

exit "$status"
