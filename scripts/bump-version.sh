#!/bin/sh
# Computes the next release version and, only with --tag, creates the annotated tag locally (epic E20).
#   scripts/bump-version.sh <major|minor|patch|vX.Y.Z[-rc.N]> [--tag]
# The version of the code is the git tag (the Makefile and the release workflow derive everything from
# it), so there is no version file to edit. This script NEVER pushes: pushing the tag is what starts
# the release workflow, and that is a deliberate, separate step (docs/releasing.md).
# --tag requires a clean work tree on main (BUMP_ALLOW_BRANCH=1 relaxes the branch check for tests).
# SIGN=1 creates a signed tag (git tag -s) instead of an annotated one.
set -eu
what="${1:-}"
dotag=0
[ "${2:-}" = "--tag" ] && dotag=1
[ -n "$what" ] || { echo "usage: bump-version.sh <major|minor|patch|vX.Y.Z> [--tag]" >&2; exit 2; }

# Latest final release tag (pre-releases are ignored when bumping).
latest=$(git tag --list 'v[0-9]*.[0-9]*.[0-9]*' | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -n 1 || true)
[ -n "$latest" ] || latest=v0.0.0
cur=${latest#v}
major=${cur%%.*}
rest=${cur#*.}
minor=${rest%%.*}
patch=${rest#*.}

case "$what" in
major) next="v$((major + 1)).0.0" ;;
minor) next="v$major.$((minor + 1)).0" ;;
patch) next="v$major.$minor.$((patch + 1))" ;;
v*)
	echo "$what" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$' || { echo "bump-version: not a semantic version: $what" >&2; exit 2; }
	next="$what"
	;;
*) echo "bump-version: expected major, minor, patch or vX.Y.Z, got: $what" >&2; exit 2 ;;
esac

if git rev-parse -q --verify "refs/tags/$next" >/dev/null; then
	echo "bump-version: tag $next already exists" >&2
	exit 1
fi
echo "current: $latest"
echo "next:    $next"
[ "$dotag" -eq 1 ] || { echo "(dry run: add --tag to create the tag locally; nothing is pushed)"; exit 0; }

branch=$(git rev-parse --abbrev-ref HEAD)
if [ "$branch" != main ] && [ "${BUMP_ALLOW_BRANCH:-}" != 1 ]; then
	echo "bump-version: release tags are cut from main (on $branch)" >&2
	exit 1
fi
if [ -n "$(git status --porcelain)" ]; then
	echo "bump-version: the work tree is not clean" >&2
	exit 1
fi
if [ "${SIGN:-}" = 1 ]; then
	git tag -s "$next" -m "Release $next"
else
	git tag -a "$next" -m "Release $next"
fi
echo "created tag $next locally. Review it, then publish with:"
echo "  git push origin $next"
