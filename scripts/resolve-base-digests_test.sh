#!/bin/sh
# Self-test: a fake resolver stands in for the registry, so this runs offline.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
root="$here/.."
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
cat >"$tmp/resolver" <<'EOF'
#!/bin/sh
# Fake digest: sha256 of the reference text.
printf 'sha256:%s\n' "$(printf '%s' "$1" | sha256sum | cut -d' ' -f1)"
EOF
chmod +x "$tmp/resolver"
export DIGEST_RESOLVER="$tmp/resolver"
want() { printf 'sha256:%s' "$(printf '%s' "$1" | sha256sum | cut -d' ' -f1)"; }

out=$(sh "$here/resolve-base-digests.sh" "$root/deploy/finance/Dockerfile")
echo "$out" | grep -qx "GO_IMAGE_DIGEST=$(want golang:1.27)" || { echo "FAIL: golang digest: $out"; exit 1; }
echo "$out" | grep -qx "DISTROLESS_DIGEST=$(want gcr.io/distroless/static-debian12:nonroot)" || { echo "FAIL: distroless digest: $out"; exit 1; }

out=$(sh "$here/resolve-base-digests.sh" "$root/deploy/can-core/Dockerfile")
echo "$out" | grep -qx "GO_IMAGE=golang:1.27@$(want golang:1.27)" || { echo "FAIL: can-core GO_IMAGE: $out"; exit 1; }
echo "$out" | grep -qx "RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot@$(want gcr.io/distroless/static-debian12:nonroot)" || { echo "FAIL: can-core RUNTIME_IMAGE: $out"; exit 1; }

out=$(sh "$here/resolve-base-digests.sh" "$root/deploy/hooks/Dockerfile")
echo "$out" | grep -qx "ALPINE_DIGEST=$(want alpine:3.22)" || { echo "FAIL: alpine digest: $out"; exit 1; }

# every deploy Dockerfile resolves completely
for f in "$root"/deploy/*/Dockerfile; do
	sh "$here/resolve-base-digests.sh" "$f" >/dev/null || { echo "FAIL: $f not fully resolvable"; exit 1; }
done

# a resolver that returns garbage is an error, never a silent empty pin
printf '#!/bin/sh\necho oops\n' >"$tmp/bad"
chmod +x "$tmp/bad"
if DIGEST_RESOLVER="$tmp/bad" sh "$here/resolve-base-digests.sh" "$root/deploy/hooks/Dockerfile" >/dev/null 2>&1; then
	echo "FAIL: invalid digest accepted"; exit 1
fi
echo "resolve-base-digests_test: ok"
