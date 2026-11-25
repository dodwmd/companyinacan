#!/bin/sh
# Reproducibility gate (epic E20): builds the release archives twice into separate directories and
# fails if any sha256 differs.   scripts/check-reproducible.sh <version> <workdir>
set -eu
version="${1:-}"
work="${2:-}"
[ -n "$version" ] && [ -n "$work" ] || { echo "usage: check-reproducible.sh <version> <workdir>" >&2; exit 2; }
here=$(cd "$(dirname "$0")" && pwd)
sh "$here/build-release.sh" "$version" "$work/a" >/dev/null
sh "$here/build-release.sh" "$version" "$work/b" >/dev/null
if diff -u "$work/a/SHA256SUMS" "$work/b/SHA256SUMS"; then
	echo "check-reproducible: ok ($(wc -l <"$work/a/SHA256SUMS" | tr -d ' ') archives identical)"
else
	echo "check-reproducible: builds differ" >&2
	exit 1
fi
