#!/bin/sh
# Sets the version of one in-house image everywhere it is written (epic E06d): the source of truth
# core/modimages/images.txt, the charts that pull it, the hooks image the manifests name and the
# references in the workflows. test/contract/module_images_test.go fails when one disagrees.
#
#   scripts/set-image-version.sh can-work X.Y.Z[-pre] [repository root]
#
# Also bump the `version` in the module's module.yaml, so that an installed module is upgraded
# (docs/releasing.md).
set -eu
name="${1:-}"
v="${2:-}"
root="${3:-$(cd "$(dirname "$0")/.." && pwd)}"
list="$root/core/modimages/images.txt"
usage() { echo "usage: set-image-version.sh <image of $list with a version> X.Y.Z[-pre] (no leading v)" >&2; exit 2; }
echo "$v" | grep -Eq '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.]+)?$' || usage
# shellcheck disable=SC2016 # $1 and $4 are awk's
awk -v n="$name" '!/^#/ && $1 == n && $4 != "-" {found = 1} END {exit !found}' "$list" || usage

edit() { # file, then a command that writes the new content to stdout
	f=$1
	shift
	"$@" "$f" >"$f.new"
	cat "$f.new" >"$f" # keeps the mode of the file
	rm -f "$f.new"
}

# shellcheck disable=SC2016 # $1 and $4 are awk's
edit "$list" awk -v n="$name" -v v="$v" '!/^#/ && $1 == n {$4 = v} {print}'

for values in "$root"/modules/*/chart/values.yaml; do
	grep -Eq "repository:[ \t]*ghcr\.io/dodwmd/${name}[ \t]*\$" "$values" || continue
	# The `tag:` that follows the image's `repository:`.
	# shellcheck disable=SC2016 # $2 is awk's
	edit "$values" awk -v v="$v" -v repo="ghcr.io/dodwmd/$name" '
		/^[ \t]*repository:/ { mine = ($2 == repo) }
		/^[ \t]*tag:/ && mine { sub(/tag:[ \t]*[^ \t#]+/, "tag: " v); mine = 0 }
		{ print }'
	chart="${values%values.yaml}Chart.yaml"
	[ ! -f "$chart" ] || edit "$chart" sed -E "s/^appVersion:.*/appVersion: \"$v\"/"
done

re="(ghcr\.io/dodwmd/${name}):[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?"
for f in "$root"/modules/*/module.yaml "$root"/.github/workflows/*.yml; do
	[ -f "$f" ] || continue
	grep -Eq "$re" "$f" || continue
	edit "$f" sed -E "s#$re#\1:$v#g"
done
echo "$name is version $v; bump the module's version in module.yaml too"
