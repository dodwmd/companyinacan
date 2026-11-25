#!/bin/sh
# Self-test for check-dco.sh using a throwaway repository.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
cd "$tmp"
git init -q -b main .
git config user.name "Test Author"
git config user.email "author@example.com"
git config commit.gpgsign false
git commit -q --allow-empty -m "base"
base=$(git rev-parse HEAD)

git commit -q --allow-empty -s -m "signed commit"
sh "$here/check-dco.sh" "$base" HEAD >/dev/null || { echo "FAIL: signed commit rejected"; exit 1; }

git commit -q --allow-empty -m "unsigned commit"
if sh "$here/check-dco.sh" "$base" HEAD >/dev/null 2>"$tmp/err"; then
  echo "FAIL: unsigned commit accepted"; exit 1
fi
grep -q "git commit --amend -s" "$tmp/err" || { echo "FAIL: remedy missing from message"; exit 1; }

git commit -q --amend --allow-empty -m "forged

Signed-off-by: Someone Else <other@example.org>"
if sh "$here/check-dco.sh" "$base" HEAD >/dev/null 2>&1; then
  echo "FAIL: sign-off by a different person accepted"; exit 1
fi
echo "check-dco_test: ok"
