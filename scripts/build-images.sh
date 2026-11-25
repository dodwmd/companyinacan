#!/bin/sh
# Builds the three container images `can up` deploys (can-core, can-static, can-forms) from this
# checkout and writes them as tarballs plus an index that `can up --images-from` verifies (epic E24).
#
#   scripts/build-images.sh [outdir]          default outdir: build/images
#   ENGINE=podman|docker                      default: podman if present, else docker
#
# This is the DEV path for a source checkout: the images are built from your working tree, and the
# base images are resolved to the digests the registry serves right now (scripts/resolve-base-digests.sh),
# so the build runs without editing the committed Dockerfiles, which keep release placeholders.
# Release images are different: they are built and pinned by the release pipeline (docs/releasing.md)
# and referenced by digest from deploy/release-images.json. Nothing here pushes anywhere.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/.." && pwd)
out="${1:-build/images}"

engine="${ENGINE:-}"
if [ -z "$engine" ]; then
	if command -v podman >/dev/null 2>&1; then engine=podman
	elif command -v docker >/dev/null 2>&1; then engine=docker
	else echo "build-images: neither podman nor docker is installed" >&2; exit 1
	fi
fi
command -v "$engine" >/dev/null 2>&1 || { echo "build-images: $engine is not installed" >&2; exit 1; }

# The forms chart expects this exact repository:tag (modules/forms/chart/values.yaml).
forms_tag=$(awk '/^  tag:/ {print $2; exit}' "$root/modules/forms/chart/values.yaml")
[ -n "$forms_tag" ] || { echo "build-images: cannot read the forms image tag from the chart" >&2; exit 1; }

mkdir -p "$out"
rm -f "$out/images.json"

# Resolve a base image reference to its current digest with the chosen engine (docker buildx is
# what scripts/resolve-base-digests.sh uses by default; podman has no buildx).
resolver="$out/.resolve-digest.sh"
cat >"$resolver" <<EOF
#!/bin/sh
set -eu
$engine pull -q "\$1" >/dev/null
$engine image inspect --format '{{index .RepoDigests 0}}' "\$1" | cut -d@ -f2
EOF
chmod 0755 "$resolver"
trap 'rm -f "$resolver"' EXIT

# name<TAB>Dockerfile<TAB>tag
images="can-core	deploy/can-core/Dockerfile	can-core:dev
can-static	deploy/can-static/Dockerfile	can-static:dev
can-forms	deploy/forms/Dockerfile	ghcr.io/dodwmd/can-forms:$forms_tag"

tab=$(printf '\t')
index='{"schema":1,"images":{'
first=1
while IFS="$tab" read -r name dockerfile tag; do
	echo "build-images: $name -> $tag"
	args=""
	for line in $(DIGEST_RESOLVER="$resolver" sh "$here/resolve-base-digests.sh" "$root/$dockerfile"); do
		args="$args --build-arg $line"
	done
	# shellcheck disable=SC2086 # $args is a list of --build-arg NAME=VALUE words without spaces
	"$engine" build -f "$root/$dockerfile" $args -t "$tag" "$root"
	tar="$name.tar"
	if [ "$engine" = podman ]; then
		"$engine" save --format docker-archive -o "$out/$tar" "$tag"
	else
		"$engine" save -o "$out/$tar" "$tag"
	fi
	sum=$(sha256sum "$out/$tar" | cut -d' ' -f1)
	[ "$first" = 1 ] || index="$index,"
	first=0
	index="$index\"$name\":{\"ref\":\"$tag\",\"file\":\"$tar\",\"sha256\":\"$sum\"}"
done <<EOF
$images
EOF
printf '%s}}\n' "$index" >"$out/images.json"
echo "build-images: wrote $out/images.json; deploy with: make up IMAGES_FROM=$out"
