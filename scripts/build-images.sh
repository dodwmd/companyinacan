#!/bin/sh
# Builds every in-house container image (core/modimages/images.txt: can-core, can-static, the module
# images and can-hooks) from this checkout and writes them as tarballs plus an index that
# `can up --images-from` verifies (epics E24, E06d). `can up` ships only what company.yaml enables.
#
#   scripts/build-images.sh [outdir]          default outdir: build/images
#   ENGINE=podman|docker                      default: podman if present, else docker
#   ONLY="can-core can-static can-forms"      build a subset (can-core and can-static are always needed)
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

# The module images get the tag their charts name.
list="$root/core/modimages/images.txt"
version=$(cat "$root/core/modimages/version.txt")
[ -n "$version" ] || { echo "build-images: core/modimages/version.txt is empty" >&2; exit 1; }

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

index='{"schema":1,"images":{'
first=1
built=""
while read -r name dockerfile context; do
	case "$name" in '' | '#'*) continue ;; esac
	if [ -n "${ONLY:-}" ]; then
		case " $ONLY " in *" $name "*) ;; *) continue ;; esac
	fi
	# The tarball carries the full reference: podman would file a short name under localhost/, which
	# the cluster does not match to the name the manifests use.
	case "$name" in
	can-core | can-static) tag="$name:dev" full="docker.io/library/$name:dev" ;;
	*) tag="ghcr.io/dodwmd/$name:$version" full="ghcr.io/dodwmd/$name:$version" ;;
	esac
	echo "build-images: $name -> $tag"
	args=""
	for line in $(DIGEST_RESOLVER="$resolver" sh "$here/resolve-base-digests.sh" "$root/$dockerfile" </dev/null); do
		args="$args --build-arg $line"
	done
	# shellcheck disable=SC2086 # $args is a list of --build-arg NAME=VALUE words without spaces
	"$engine" build -f "$root/$dockerfile" $args --build-arg "VERSION=$version" -t "$full" "$root/$context" </dev/null
	tar="$name.tar"
	rm -f "$out/$tar"
	if [ "$engine" = podman ]; then
		"$engine" save --format docker-archive -o "$out/$tar" "$full" </dev/null
	else
		"$engine" save -o "$out/$tar" "$full" </dev/null
	fi
	sum=$(sha256sum "$out/$tar" | cut -d' ' -f1)
	[ "$first" = 1 ] || index="$index,"
	first=0
	index="$index\"$name\":{\"ref\":\"$tag\",\"file\":\"$tar\",\"sha256\":\"$sum\"}"
	built="$built $name"
done <"$list"
for want in can-core can-static ${ONLY:-}; do
	case " $built " in *" $want "*) ;; *) echo "build-images: $want was not built (ONLY must include can-core and can-static, and name images of $list)" >&2; exit 1 ;; esac
done
printf '%s}}\n' "$index" >"$out/images.json"
echo "build-images: wrote $out/images.json; deploy with: make up IMAGES_FROM=$out"
