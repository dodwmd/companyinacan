#!/bin/sh
# Builds the release binaries (epic E20): static (CGO_ENABLED=0), -trimpath, linux/amd64 and linux/arm64.
#   scripts/build-release.sh <version> <outdir>
# Writes <outdir>/<name>_<version>_linux_<arch>.tar.gz (binary + LICENSE) and <outdir>/bin/ with the raw
# binaries (input for the SBOM), plus <outdir>/SHA256SUMS over the tarballs. Archives are deterministic:
# the build date is the commit time (SOURCE_DATE_EPOCH), tar entries are sorted with fixed owner and mtime,
# and gzip omits its own timestamp, so two builds of one commit are byte-identical (check-reproducible.sh).
# Environment: BINARIES and ARCHES override the lists (used by tests).
set -eu
version="${1:-}"
out="${2:-}"
[ -n "$version" ] && [ -n "$out" ] || { echo "usage: build-release.sh <version> <outdir>" >&2; exit 2; }
here=$(cd "$(dirname "$0")" && pwd)
cd "$here/.."

BINARIES="${BINARIES:-can can-core can-static can-forms can-work can-people can-jobs can-finance can-equity}"
ARCHES="${ARCHES:-amd64 arm64}"
mod=github.com/dodwmd/companyinacan
epoch="${SOURCE_DATE_EPOCH:-$(git log -1 --format=%ct)}"
commit="${COMMIT:-$(git rev-parse --short=12 HEAD)}"
date=$(date -u -d "@$epoch" +%Y-%m-%dT%H:%M:%SZ)

# The -X targets differ per binary (see the Dockerfiles); can-core and can-static carry no version variable.
ldflags_for() {
	case "$1" in
	can) echo "-s -w -X $mod/core/cli.version=$version -X $mod/core/cli.commit=$commit -X $mod/core/cli.date=$date" ;;
	can-forms) echo "-s -w -X $mod/core/forms.Version=$version" ;;
	can-work) echo "-s -w -X $mod/core/work.Version=$version" ;;
	can-people) echo "-s -w -X $mod/core/people.Version=$version" ;;
	can-jobs) echo "-s -w -X $mod/core/jobs.Version=$version" ;;
	can-finance) echo "-s -w -X $mod/core/finance.Version=$version" ;;
	can-equity) echo "-s -w -X $mod/core/equity.Version=$version" ;;
	*) echo "-s -w" ;;
	esac
}

rm -rf "$out"
mkdir -p "$out/bin"
for arch in $ARCHES; do
	for name in $BINARIES; do
		dir="$out/bin/${name}_linux_${arch}"
		mkdir -p "$dir"
		# -buildvcs=false: the VCS stamp would differ between a clean checkout and a dirty one.
		# shellcheck disable=SC2046
		CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -buildvcs=false \
			-ldflags "$(ldflags_for "$name")" -o "$dir/$name" "./cmd/$name"
		cp LICENSE "$dir/LICENSE"
		tar --sort=name --mtime="@$epoch" --owner=0 --group=0 --numeric-owner -C "$dir" -cf - "$name" LICENSE |
			gzip -n -9 >"$out/${name}_${version}_linux_${arch}.tar.gz"
	done
done
(cd "$out" && sha256sum -- *.tar.gz >SHA256SUMS)
echo "build-release: $(wc -l <"$out/SHA256SUMS" | tr -d ' ') archive(s) in $out for $version ($commit, $date)"
