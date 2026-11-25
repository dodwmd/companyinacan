#!/bin/sh
# Runs shellcheck on every shell script tracked by git (epic E20). --require makes a missing shellcheck an
# error (CI); without it the check is skipped with a notice so contributors need no extra tools.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
cd "$here/.."
require=0
[ "${1:-}" = "--require" ] && require=1
if ! command -v shellcheck >/dev/null 2>&1; then
	if [ "$require" -eq 1 ]; then echo "lint-shell: shellcheck not installed" >&2; exit 1; fi
	echo "lint-shell: shellcheck not installed, skipping"
	exit 0
fi
files=$(git ls-files '*.sh')
[ -n "$files" ] || { echo "lint-shell: no scripts"; exit 0; }
# shellcheck disable=SC2086
shellcheck -x $files
echo "lint-shell: ok ($(echo "$files" | wc -l | tr -d ' ') scripts)"
