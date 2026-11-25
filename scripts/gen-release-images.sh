#!/bin/sh
# Writes deploy/release-images.json, the manifest `can up --release-manifest` reads (epic E24):
# every image the stack needs, referenced by digest, never by tag.
#
#   scripts/gen-release-images.sh vX.Y.Z ghcr.io/dodwmd can-core=sha256:<64 hex> can-static=sha256:... can-forms=sha256:...
#
# The release pipeline calls this after publish-images, with the digests the registry reported for
# the images it pushed and signed, and attaches the result to the release. A hand-edited or
# tag-only manifest is refused by `can up`, so this script refuses the same inputs early.
set -eu
version="${1:-}"
registry="${2:-}"
[ -n "$version" ] && [ -n "$registry" ] || { echo "usage: gen-release-images.sh <version> <registry> name=sha256:<digest>..." >&2; exit 2; }
shift 2
printf '%s' "$version" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$' || { echo "gen-release-images: $version is not a version tag" >&2; exit 2; }
printf '%s' "$registry" | grep -Eq '^[a-z0-9][a-z0-9./_-]*$' || { echo "gen-release-images: invalid registry $registry" >&2; exit 2; }

body=""
seen=""
for pair in "$@"; do
	name=${pair%%=*}
	digest=${pair#*=}
	case "$name" in can-core | can-static | can-forms) ;; *) echo "gen-release-images: unknown image $name" >&2; exit 2 ;; esac
	printf '%s' "$digest" | grep -Eq '^sha256:[0-9a-f]{64}$' || { echo "gen-release-images: $name digest is not sha256:<64 hex>" >&2; exit 2; }
	[ -z "$body" ] || body="$body,"
	body="$body\"$name\":{\"ref\":\"$registry/$name@$digest\"}"
	seen="$seen $name"
done
for need in can-core can-static can-forms; do
	case "$seen" in *" $need"*) ;; *) echo "gen-release-images: missing $need" >&2; exit 2 ;; esac
done
printf '{"schema":1,"version":"%s","images":{%s}}\n' "$version" "$body"
