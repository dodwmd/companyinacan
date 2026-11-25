#!/bin/sh
# Fails if any Dockerfile under deploy/ or any module chart's values.yaml under modules/ still
# carries a REPLACE_WITH_ digest (or tag) placeholder.
# Run by release builds (`make release-check`); development builds may keep placeholders.
set -eu
root="${1:-$(dirname "$0")/..}"
found=$(grep -rn --include='Dockerfile*' 'REPLACE_WITH_' "$root/deploy" || true)
if [ -d "$root/modules" ]; then
	charts=$(grep -rn --include='values.yaml' 'REPLACE_WITH_' "$root/modules" || true)
	found="$found${found:+${charts:+
}}$charts"
fi
if [ -n "$found" ]; then
	echo "unpinned images (resolve the digests, see deploy/can-static/README.md and modules/*/README.md):" >&2
	echo "$found" >&2
	exit 1
fi
echo "image pins ok"
