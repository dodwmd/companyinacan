#!/bin/sh
# In-repo secret scanner (epic E20): no third-party service, no token. Scans the files git tracks
# (or, outside a git work tree, every file under the directory) for well-known credential shapes.
# Findings can be silenced ONLY by a line in scripts/secret-allowlist.txt ("<path-glob> <reason>", whole
# file) or by the marker `scan-secrets:allow <reason>` on the offending line itself.
# Usage: scripts/scan-secrets.sh [dir]     exit 0 clean, 1 findings, 2 usage error.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
target="${1:-$here/..}"
allow="${SECRET_ALLOWLIST:-$here/secret-allowlist.txt}"
[ -d "$target" ] || { echo "scan-secrets: not a directory: $target" >&2; exit 2; }

# name<TAB>extended regex, one per line.
patterns=$(printf '%s\n' \
	'private-key	-----BEGIN ([A-Z]+ )?PRIVATE KEY-----' \
	'aws-access-key	AKIA[0-9A-Z]{16}' \
	'github-token	gh[pousr]_[A-Za-z0-9]{36,}' \
	'github-fine-grained	github_pat_[A-Za-z0-9_]{50,}' \
	'slack-token	xox[abprs]-[A-Za-z0-9-]{10,}' \
	'google-api-key	AIza[0-9A-Za-z_-]{35}' \
	'stripe-live-key	sk_live_[0-9a-zA-Z]{20,}' \
	'generic-assignment	(password|passwd|secret|api[_-]?key|token)[A-Za-z_]*[[:space:]]*[:=][[:space:]]*["'"'"'][A-Za-z0-9/+=]{32,}["'"'"']')

# The file list goes through a temp file so names with spaces survive (NUL-separated, then one per
# line; a newline inside a file name is not supported).
list=$(mktemp)
trap 'rm -f "$list"' EXIT
if git -C "$target" rev-parse --git-dir >/dev/null 2>&1; then
	git -C "$target" ls-files -z | tr '\0' '\n' >"$list"
else
	(cd "$target" && find . -type f -not -path './.git/*' -print0 | tr '\0' '\n' | sed 's|^\./||') >"$list"
fi

allowed() { # $1 path
	[ -f "$allow" ] || return 1
	while IFS= read -r line; do
		case "$line" in '' | '#'*) continue ;; esac
		glob=${line%%[[:space:]]*}
		# shellcheck disable=SC2254
		case "$1" in $glob) return 0 ;; esac
	done <"$allow"
	return 1
}

found=0
tab=$(printf '\t')
while IFS= read -r f; do
	[ -f "$target/$f" ] || continue
	case "$f" in *.png | *.jpg | *.gif | *.ico | *.woff2 | *.gz | *.zip | go.sum) continue ;; esac
	allowed "$f" && continue
	while IFS="$tab" read -r name re; do
		# Report path, line number and rule only, never the matched text.
		# A line carrying the marker `scan-secrets:allow` (with a reason) is a reviewed fake.
		hits=$(grep -nIE -e "$re" "$target/$f" 2>/dev/null | grep -v 'scan-secrets:allow' | cut -d: -f1 || true)
		if [ -n "$hits" ]; then
			found=1
			for n in $hits; do echo "$f:$n: [$name]"; done
		fi
	done <<EOF
$patterns
EOF
done <"$list"
if [ "$found" -ne 0 ]; then
	echo "scan-secrets: possible secrets found (matches are not printed). Remove them and rotate if real, or allowlist a public fake with a reason in scripts/secret-allowlist.txt" >&2
	exit 1
fi
echo "scan-secrets: clean"
