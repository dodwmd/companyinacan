#!/bin/sh
# The release's gate on image versions (epic E06d): fails when an image's sources changed since the
# version core/modimages/images.txt gives it was published, because that tag is never moved and the
# charts would keep pulling the old image.
#
#   scripts/check-image-versions.sh ghcr.io/OWNER [repository root]
#
# Prints one line per image, "name state digest": `release` (no version of its own), `absent` (not
# published yet) or `published` (published from these sources). The release records the digest as the
# image label dev.companyinacan.source. Anything that cannot be told fails.
# IMAGE_INSPECT replaces `docker buildx imagetools inspect` (test/contract).
set -eu
prefix="${1:-}"
[ -n "$prefix" ] || { echo "usage: check-image-versions.sh <registry/owner> [repository root]" >&2; exit 2; }
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "${2:-$here/..}" && pwd)
inspect="${IMAGE_INSPECT:-docker buildx imagetools inspect}"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

awk '!/^#/ && NF == 4 {print $1, $4}' "$root/core/modimages/images.txt" >"$tmp/list"
bad=0
while read -r name version; do
	if [ "$version" = - ]; then
		echo "$name release -"
		continue
	fi
	digest=$(sh "$here/image-source-digest.sh" "$name" "$root" </dev/null)
	ref="$prefix/$name:$version"
	# shellcheck disable=SC2086 # $inspect is a command with its arguments
	if $inspect "$ref" --format '{{json .Image}}' >"$tmp/image" 2>"$tmp/error" </dev/null; then
		was=$(jq -r '.config.Labels["dev.companyinacan.source"] // empty' "$tmp/image")
		if [ "$was" = "$digest" ]; then
			echo "$name published $digest"
		else
			echo "::error::$ref is published from other sources: $name changed, so give it a new version (scripts/set-image-version.sh $name X.Y.Z, and the module's module.yaml)" >&2
			bad=1
		fi
	elif grep -Eqi 'not found|manifest unknown|name unknown' "$tmp/error"; then
		echo "$name absent $digest"
	else
		echo "::error::cannot tell whether $ref exists: $(cat "$tmp/error")" >&2
		bad=1
	fi
done <"$tmp/list"
exit "$bad"
