#!/bin/sh
# Sets the version of the in-house images everywhere it is written (epic E06d): the source of truth
# core/modimages/version.txt, the module charts, the hooks image the manifests name and the image
# references in the workflows. test/contract/module_images_test.go fails when one disagrees.
#
#   scripts/set-image-version.sh X.Y.Z[-pre] [repository root]
#
# Run it, review the diff and merge it BEFORE tagging vX.Y.Z: the release workflow refuses to publish
# a tag that differs, because the charts would name images that do not exist (docs/releasing.md).
set -eu
v="${1:-}"
root="${2:-$(cd "$(dirname "$0")/.." && pwd)}"
echo "$v" | grep -Eq '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$' || { echo "usage: set-image-version.sh X.Y.Z[-pre] (no leading v)" >&2; exit 2; }

list="$root/core/modimages/images.txt"
names=$(awk '!/^#/ && NF == 3 {print $1}' "$list" | tr '\n' ' ')
echo "$v" >"$root/core/modimages/version.txt"

edit() { # file, then a command that writes the new content to stdout
	f=$1
	shift
	"$@" "$f" >"$f.new"
	cat "$f.new" >"$f" # keeps the mode of the file
	rm -f "$f.new"
}

for values in "$root"/modules/*/chart/values.yaml; do
	# The `tag:` that follows an in-house `repository:`.
	# shellcheck disable=SC2016 # $2 and $0 are awk's
	edit "$values" awk -v v="$v" -v names="$names" '
		BEGIN { n = split(names, a, " "); for (i = 1; i <= n; i++) ours["ghcr.io/dodwmd/" a[i]] = 1 }
		/^[ \t]*repository:/ { mine = ($2 in ours) }
		/^[ \t]*tag:/ && mine { sub(/tag:[ \t]*[^ \t#]+/, "tag: " v); mine = 0 }
		{ print }'
	chart="${values%values.yaml}Chart.yaml"
	for n in $names; do
		if grep -Eq "repository:[ \t]*ghcr\.io/dodwmd/${n}[ \t]*\$" "$values" && [ -f "$chart" ]; then
			edit "$chart" sed -E "s/^appVersion:.*/appVersion: \"$v\"/"
		fi
	done
done

# `ghcr.io/dodwmd/can-x:X.Y.Z` (or `$name:X.Y.Z` in a workflow). Adapter images are not built here.
alternatives=$(printf '%s' "$names" | tr ' ' '\n' | grep . | tr '\n' '|')
re="(ghcr\.io/dodwmd/(${alternatives}\\\$[a-z]+)):[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?"
for f in "$root"/modules/*/module.yaml "$root"/.github/workflows/*.yml; do
	[ -f "$f" ] || continue
	grep -Eq "$re" "$f" || continue
	edit "$f" sed -E "s#$re#\1:$v#g"
done
echo "image version is $v"
