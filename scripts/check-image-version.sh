#!/bin/sh
# Refuses to release a tag the charts do not name (epic E06d): the release pushes images tagged
# X.Y.Z for tag vX.Y.Z, and the charts pull core/modimages/version.txt.
#
#   scripts/check-image-version.sh vX.Y.Z[-pre] [repository root]
set -eu
tag="${1:-}"
root="${2:-$(cd "$(dirname "$0")/.." && pwd)}"
[ -n "$tag" ] || { echo "usage: check-image-version.sh vX.Y.Z[-pre]" >&2; exit 2; }
have=$(cat "$root/core/modimages/version.txt")
if [ "$tag" != "v$have" ]; then
	echo "the module charts name images of version $have, but the tag is $tag: run scripts/set-image-version.sh ${tag#v}, merge that, then tag (docs/releasing.md)" >&2
	exit 1
fi
echo "image version $have matches $tag"
