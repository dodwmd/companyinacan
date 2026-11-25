#!/bin/sh
# Self-test for gen-release-images.sh.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
d=sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
fail() { echo "FAIL: $1" >&2; exit 1; }

out=$(sh "$here/gen-release-images.sh" v0.1.0 ghcr.io/dodwmd can-core=$d can-static=$d can-forms=$d)
case "$out" in *"\"ref\":\"ghcr.io/dodwmd/can-core@$d\""*) ;; *) fail "ref missing: $out" ;; esac
case "$out" in *'"version":"v0.1.0"'*) ;; *) fail "version missing" ;; esac

for bad in "can-core=sha256:abc" "can-core=latest" "evil=$d"; do
	if sh "$here/gen-release-images.sh" v0.1.0 ghcr.io/dodwmd "$bad" can-static=$d can-forms=$d >/dev/null 2>&1; then
		fail "accepted $bad"
	fi
done
if sh "$here/gen-release-images.sh" v0.1.0 ghcr.io/dodwmd can-core=$d >/dev/null 2>&1; then fail "accepted a partial set"; fi
if sh "$here/gen-release-images.sh" 0.1.0 ghcr.io/dodwmd can-core=$d can-static=$d can-forms=$d >/dev/null 2>&1; then fail "accepted a version without v"; fi
echo "gen-release-images_test: ok"
