#!/bin/sh
# Resolves the REPLACE_WITH_ base-image digest placeholders of a Dockerfile (epic E20).
#   scripts/resolve-base-digests.sh deploy/finance/Dockerfile
# prints one `--build-arg`-ready NAME=VALUE line per placeholder ARG, for example
#   GO_IMAGE_DIGEST=sha256:<64 hex>
#   GO_IMAGE=golang:1.27@sha256:<64 hex>
# so a release build passes each line as `--build-arg NAME=VALUE` (see release.yml).
# The digest of a reference is looked up with `docker buildx imagetools inspect` (the pinned
# docker/setup-buildx-action provides it in CI). DIGEST_RESOLVER overrides the lookup command; it is
# called with the image reference and must print a sha256:<64 hex> digest (used by the self-test).
# The lookup queries the registry for the CURRENT digest of the tag: that is the point of a release
# pin, and it is recorded in the release (docs/releasing.md) so the build can be audited.
set -eu
dockerfile="${1:-}"
[ -f "$dockerfile" ] || { echo "usage: resolve-base-digests.sh <Dockerfile>" >&2; exit 2; }

resolve() { # ref -> digest
	if [ -n "${DIGEST_RESOLVER:-}" ]; then
		d=$($DIGEST_RESOLVER "$1")
	else
		d=$(docker buildx imagetools inspect "$1" --format '{{.Manifest.Digest}}')
	fi
	case "$d" in
	sha256:????????????????????????????????????????????????????????????????) ;;
	*) echo "resolve-base-digests: no valid digest for $1 (got: $d)" >&2; return 1 ;;
	esac
	printf '%s' "$d"
}

# name<TAB>reference<TAB>full|digest for every ARG whose default is a placeholder.
plan=$(awk '
	/^ARG [A-Za-z_][A-Za-z0-9_]*=/ {
		line = $0; sub(/^ARG /, "", line)
		n = index(line, "="); name = substr(line, 1, n - 1); val = substr(line, n + 1)
		if (val ~ /REPLACE_WITH_/) { ph[name] = val; order[++cnt] = name } else def[name] = val
	}
	/^FROM / { from[++nf] = $2 }
	function expand(s,   v) {
		while (match(s, /\$\{[A-Za-z_][A-Za-z0-9_]*\}/)) {
			v = substr(s, RSTART + 2, RLENGTH - 3)
			s = substr(s, 1, RSTART - 1) def[v] substr(s, RSTART + RLENGTH)
		}
		return s
	}
	END {
		for (i = 1; i <= cnt; i++) {
			name = order[i]; val = ph[name]
			if (val ~ /@sha256:/) { ref = val; sub(/@.*/, "", ref); print name "\t" ref "\tfull"; continue }
			ref = ""
			for (j = 1; j <= nf; j++) {
				at = "@${" name "}"
				k = index(from[j], at)
				if (k > 0) { ref = expand(substr(from[j], 1, k - 1)); break }
			}
			if (ref == "") { print name "\t\tunresolvable"; continue }
			print name "\t" ref "\tdigest"
		}
	}' "$dockerfile")

[ -n "$plan" ] || { echo "resolve-base-digests: no placeholders in $dockerfile" >&2; exit 0; }
tab=$(printf '\t')
rc=0
while IFS="$tab" read -r name ref kind; do
	if [ "$kind" = unresolvable ] || [ -z "$ref" ]; then
		echo "resolve-base-digests: cannot find the image of ARG $name in $dockerfile" >&2
		rc=1
		continue
	fi
	if d=$(resolve "$ref"); then
		if [ "$kind" = full ]; then echo "$name=$ref@$d"; else echo "$name=$d"; fi
	else
		rc=1
	fi
done <<EOF
$plan
EOF
exit "$rc"
