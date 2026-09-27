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
# "## [0.4.4] - <date>" headings.
versions=$(sed -n 's/^## \[\([0-9][0-9.]*\)]\( — .*\)\?$/\1/p' "$changelog")
[ -n "$versions" ] || fail "no released version headings in $(basename "$changelog")"

# The order check compares each version to the one above it, component by
# component, numerically: 0.10.0 is above 0.9.0, which neither a string sort
# nor a plain `sort -V` gives portably. `sort -V` is GNU-only, and BSD sort
# (every macOS in the release matrix) rejects it, which turned this check into
# a silent pass-by-error on the platform the check exists to protect. A
# missing field counts as 0, so 0.5 and 0.5.0 compare equal.
misordered=$(printf '%s\n' "$versions" | awk '
function vcmp(a, b,   na, nb, i, x, y) {
	na = split(a, x, "."); nb = split(b, y, ".")
	for (i = 1; i <= na || i <= nb; i++) {
		va = (i <= na) ? x[i] + 0 : 0
		vb = (i <= nb) ? y[i] + 0 : 0
		if (va != vb) return (va > vb) ? 1 : -1
	}
	return 0
}
NR == 1 { prev = $0; next }
{ if (vcmp(prev, $0) < 0) { print prev " before " $0; exit } prev = $0 }
')
[ -z "$misordered" ] ||
	fail "released sections are not in descending version order ($misordered)"

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

# Each section carries one heading per impact group, the headings run in the
# order a reader scans them in, and no two entries in a section state the same
# fix. The [Unreleased] block had eight headings and ten duplicated entries
# before this was checked, all of which a tag would have shipped as that
# release's notes: a reader scanning for the fixes found the same fix twice
# and the groups in an order that put additions last.
# `Breaking:` is stripped from the key so the same fix written up once with the
# marker and once without it is still one entry, and the survivor has to be the
# one carrying the marker.
while IFS= read -r problem; do
	[ -n "$problem" ] && fail "$problem"
done < <(awk '
	BEGIN {
		n = split("Added Changed Removed Fixed Security Validated", order, " ")
		for (i = 1; i <= n; i++) rank[order[i]] = i
	}
	/^## \[/ {
		section = $0
		sub(/^## /, "", section)
		rank_seen = 0
		delete group_seen
		delete entry_seen
		next
	}
	/^### / {
		group = substr($0, 5)
		if (!(group in rank)) {
			print section ": unknown impact group " group
			status = 1
			next
		}
		if (group in group_seen) {
			print section ": a second " group " group; each impact group appears once"
			status = 1
		}
		group_seen[group] = 1
		if (rank[group] < rank_seen) {
			print section ": " group " runs after a group below it; the order is " order[1] ", " order[2] ", " order[3] ", " order[4] ", " order[5] ", " order[6]
			status = 1
		}
		if (rank[group] > rank_seen) rank_seen = rank[group]
		next
	}
	/^- \*\*/ {
		title = $0
		sub(/^- \*\*(Breaking: )?/, "", title)
		sub(/\*\*.*$/, "", title)
		if (title in entry_seen) {
			print section ": two entries state the same fix (" title ")"
			status = 1
		}
		entry_seen[title] = 1
	}
	END { exit status }
' "$changelog")

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
