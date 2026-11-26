#!/bin/sh
# A digest of everything of this repository that goes into one in-house image (epic E06d), so that a
# release can tell whether an image changed since its version was published.
#
#   scripts/image-source-digest.sh can-work [repository root]      prints 64 hex characters
#
# For a Go image: the files (Go and embedded) of every package of this module its command links, the
# versions of the other modules it links, the go directive and the Dockerfile without comments. For
# can-hooks, which compiles nothing: the Dockerfile without comments (it copies no files;
# test/contract/module_images_test.go holds the Dockerfiles to these shapes). The base images are
# not part of it: their digests are resolved at release time.
set -eu
name="${1:-}"
root=$(cd "${2:-$(dirname "$0")/..}" && pwd -P)
dockerfile=$(awk -v n="$name" '!/^#/ && $1 == n {print $2}' "$root/core/modimages/images.txt")
[ -n "$dockerfile" ] || { echo "usage: image-source-digest.sh <image of core/modimages/images.txt>" >&2; exit 2; }
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

grep -v '^[[:space:]]*#' "$root/$dockerfile" | grep -v '^[[:space:]]*$' >"$tmp/dockerfile"
cp "$tmp/dockerfile" "$tmp/input"

cmd=$(grep -o '\./cmd/[a-z-]*' "$tmp/dockerfile" | sort -u)
if [ -n "$cmd" ]; then
	grep -E '^(go|toolchain) ' "$root/go.mod" >>"$tmp/input"
	# shellcheck disable=SC2016 # a Go template
	format='{{$d := .Dir}}{{with .Module}}{{if .Main}}{{range $.GoFiles}}F {{$d}}/{{.}}{{"\n"}}{{end}}{{range $.SFiles}}F {{$d}}/{{.}}{{"\n"}}{{end}}{{range $.EmbedFiles}}F {{$d}}/{{.}}{{"\n"}}{{end}}{{else}}M {{.Path}} {{.Version}}{{with .Replace}} {{.Path}} {{.Version}}{{end}}{{"\n"}}{{end}}{{end}}'
	(cd "$root" && GOWORK=off GOFLAGS=-mod=readonly GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go list -deps -f "$format" "$cmd") >"$tmp/deps"
	grep '^M ' "$tmp/deps" | LC_ALL=C sort -u >>"$tmp/input"
	sed -n 's/^F //p' "$tmp/deps" | LC_ALL=C sort -u >"$tmp/files"
	[ -s "$tmp/files" ] || { echo "image-source-digest: $name: no source files found" >&2; exit 1; }
	while IFS= read -r f; do
		sum=$(sha256sum "$f" | cut -d' ' -f1)
		echo "F ${f#"$root"/} $sum" >>"$tmp/input"
	done <"$tmp/files"
fi
sha256sum "$tmp/input" | cut -d' ' -f1
