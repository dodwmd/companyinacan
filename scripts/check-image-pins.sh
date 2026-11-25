#!/bin/sh
# Fails if any Dockerfile under deploy/ still carries a REPLACE_WITH_ digest placeholder.
# Run by release builds (`make release-check`); development builds may keep placeholders.
set -eu
root="${1:-$(dirname "$0")/..}"
found=$(grep -rn --include='Dockerfile*' 'REPLACE_WITH_' "$root/deploy" || true)
if [ -n "$found" ]; then
	echo "unpinned base images (resolve the digests, see deploy/can-static/README.md):" >&2
	echo "$found" >&2
	exit 1
fi
echo "image pins ok"
