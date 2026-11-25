#!/bin/sh
# Self-test: the SBOM of a real, freshly built binary is CycloneDX-shaped and deterministic.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
(cd "$here/.." && CGO_ENABLED=0 go build -trimpath -buildvcs=false -o "$tmp/can" ./cmd/can)
export SOURCE_DATE_EPOCH=1700000000
sh "$here/gen-sbom.sh" "$tmp/can" v1.2.3 >"$tmp/a.json"
sh "$here/gen-sbom.sh" "$tmp/can" v1.2.3 >"$tmp/b.json"
cmp -s "$tmp/a.json" "$tmp/b.json" || { echo "FAIL: SBOM not deterministic"; exit 1; }
grep -q '"bomFormat": "CycloneDX"' "$tmp/a.json" || { echo "FAIL: not CycloneDX"; exit 1; }
grep -q '"purl": "pkg:golang/github.com/spf13/cobra@' "$tmp/a.json" || { echo "FAIL: cobra missing"; exit 1; }
grep -q '"version": "v1.2.3"' "$tmp/a.json" || { echo "FAIL: version not set"; exit 1; }
grep -q '"name": "go", "version": "go1\.' "$tmp/a.json" || { echo "FAIL: toolchain component missing"; exit 1; }
if grep -E '"purl": "[^"]*\+' "$tmp/a.json"; then echo "FAIL: unencoded + in a purl"; exit 1; fi
if command -v python3 >/dev/null 2>&1; then
	python3 -I -c 'import json,sys; d=json.load(open(sys.argv[1])); assert d["components"]' "$tmp/a.json" || { echo "FAIL: invalid JSON"; exit 1; }
fi
echo "gen-sbom_test: ok"
