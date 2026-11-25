#!/bin/sh
# Release notes from the commit log (epic E20), grouped by conventional-commit prefix.
#   scripts/release-notes.sh <tag> [previous-tag]      markdown on stdout
# previous-tag defaults to the nearest earlier tag, or the first commit when there is none.
# Subjects are treated as data only (never interpreted); merge commits are skipped.
set -eu
tag="${1:-}"
[ -n "$tag" ] || { echo "usage: release-notes.sh <tag> [previous-tag]" >&2; exit 2; }
prev="${2:-$(git describe --tags --abbrev=0 "$tag^" 2>/dev/null || true)}"
if [ -n "$prev" ]; then range="$prev..$tag"; else range="$tag"; fi

subjects=$(git log --no-merges --format='%s' "$range")
section() { # title, extended regex of prefixes
	body=$(printf '%s\n' "$subjects" | grep -E "^($2)(\\([^)]*\\))?!?: " | sed -E 's/^[a-z]+(\([^)]*\))?!?: /- /' || true)
	[ -z "$body" ] || printf '### %s\n\n%s\n\n' "$1" "$body"
}
echo "## $tag"
echo
breaking=$(printf '%s\n' "$subjects" | grep -E '^[a-z]+(\([^)]*\))?!: ' | sed 's/^/- /' || true)
[ -z "$breaking" ] || printf '### Breaking changes\n\n%s\n\n' "$breaking"
section "Features" "feat"
section "Fixes" "fix"
section "Security" "security|sec"
section "Performance" "perf"
section "Documentation" "docs"
section "Build, CI and chores" "build|ci|chore|refactor|test|style"
other=$(printf '%s\n' "$subjects" | grep -vE '^(feat|fix|security|sec|perf|docs|build|ci|chore|refactor|test|style)(\([^)]*\))?!?: ' | grep -v '^$' | sed 's/^/- /' || true)
[ -z "$other" ] || printf '### Other changes\n\n%s\n\n' "$other"
if [ -n "$prev" ]; then echo "Full log: \`$prev...$tag\`"; fi
