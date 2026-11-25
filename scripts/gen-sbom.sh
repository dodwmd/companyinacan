#!/bin/sh
# CycloneDX 1.5 SBOM (JSON) for one Go binary, from the module list the toolchain embeds in it
# (`go version -m`), with no third-party tool to trust (epic E20).
#   scripts/gen-sbom.sh <binary> [version] > name.cdx.json
# The document is deterministic for a given binary and SOURCE_DATE_EPOCH. Components are Go modules
# (purl pkg:golang/...); the `go:h1` property is the go.sum hash of the module. Replaced modules are
# recorded at their replacement version. This lists what is compiled in; it does not list the base
# image layers (the images are distroless static, which carry no package database).
set -eu
bin="${1:-}"
version="${2:-}"
[ -f "$bin" ] || { echo "usage: gen-sbom.sh <binary> [version]" >&2; exit 2; }
epoch="${SOURCE_DATE_EPOCH:-0}"
ts=$(date -u -d "@$epoch" +%Y-%m-%dT%H:%M:%SZ)
info=$(go version -m "$bin")
sum=$(printf '%s' "$info" | sha256sum | cut -c1-32)
serial="urn:uuid:$(echo "$sum" | cut -c1-8)-$(echo "$sum" | cut -c9-12)-$(echo "$sum" | cut -c13-16)-$(echo "$sum" | cut -c17-20)-$(echo "$sum" | cut -c21-32)"

printf '%s\n' "$info" | awk -v serial="$serial" -v ts="$ts" -v ver="$version" '
	function q(s) { gsub(/\\/, "\\\\", s); gsub(/"/, "\\\"", s); return "\"" s "\"" }
	# purl: the module path is lowercased and "+" in the version is percent-encoded (v1.0.0+incompatible).
	function purl(path, v,   e) { e = v; gsub(/\+/, "%2B", e); return "pkg:golang/" tolower(path) "@" e }
	function comp(path, v, h1,   out) {
		out = "    {\"type\": \"library\", \"bom-ref\": " q(purl(path, v)) ", \"name\": " q(path) ", \"version\": " q(v)
		out = out ", \"purl\": " q(purl(path, v))
		if (h1 != "") out = out ", \"properties\": [{\"name\": \"go:h1\", \"value\": " q(h1) "}]"
		return out "}"
	}
	BEGIN { FS = "\t"; n = 0 }
	NR == 1 { tc = $0; sub(/^.*: /, "", tc) }
	$2 == "path" { main = $3 }
	$2 == "mod" { modpath = $3; modver = $4 }
	$2 == "dep" { n++; dp[n] = $3; dv[n] = $4; dh[n] = $5 }
	$2 == "=>" && n > 0 { dp[n] = $3; dv[n] = $4; dh[n] = $5 }
	END {
		if (ver == "") ver = modver
		printf "{\n  \"bomFormat\": \"CycloneDX\",\n  \"specVersion\": \"1.5\",\n  \"serialNumber\": %s,\n  \"version\": 1,\n", q(serial)
		printf "  \"metadata\": {\n    \"timestamp\": %s,\n    \"tools\": {\"components\": [{\"type\": \"application\", \"name\": \"scripts/gen-sbom.sh\"}]},\n", q(ts)
		printf "    \"component\": {\"type\": \"application\", \"bom-ref\": %s, \"name\": %s, \"version\": %s, \"licenses\": [{\"license\": {\"id\": \"Apache-2.0\"}}]}\n  },\n", q(main), q(main), q(ver)
		printf "  \"components\": [\n"
		for (i = 1; i <= n; i++) printf "%s,\n", comp(dp[i], dv[i], dh[i])
		# The Go toolchain that built the binary: needed to reproduce it.
		printf "    {\"type\": \"platform\", \"bom-ref\": \"toolchain\", \"name\": \"go\", \"version\": %s}\n", q(tc)
		printf "  ]\n}\n"
	}'
