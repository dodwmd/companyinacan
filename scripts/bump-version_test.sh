#!/bin/sh
# Self-test for bump-version.sh using a throwaway repository.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
cd "$tmp"
git init -q -b main .
git config user.name T
git config user.email t@example.com
git config commit.gpgsign false
git config tag.gpgsign false
git commit -q --allow-empty -m "init"

sh "$here/bump-version.sh" minor | grep -qx 'next:    v0.1.0' || { echo "FAIL: first minor"; exit 1; }
sh "$here/bump-version.sh" v1.2.3 --tag >/dev/null
git rev-parse -q --verify refs/tags/v1.2.3 >/dev/null || { echo "FAIL: tag not created"; exit 1; }
[ "$(git cat-file -t v1.2.3)" = tag ] || { echo "FAIL: tag is not annotated"; exit 1; }
sh "$here/bump-version.sh" patch | grep -qx 'next:    v1.2.4' || { echo "FAIL: patch"; exit 1; }
sh "$here/bump-version.sh" minor | grep -qx 'next:    v1.3.0' || { echo "FAIL: minor"; exit 1; }
sh "$here/bump-version.sh" major | grep -qx 'next:    v2.0.0' || { echo "FAIL: major"; exit 1; }
if sh "$here/bump-version.sh" v1.2.3 >/dev/null 2>&1; then echo "FAIL: existing tag accepted"; exit 1; fi
if sh "$here/bump-version.sh" 1.2 >/dev/null 2>&1; then echo "FAIL: bad version accepted"; exit 1; fi
# a pre-release tag does not move the base of the next bump
sh "$here/bump-version.sh" v9.0.0-rc.1 --tag >/dev/null
sh "$here/bump-version.sh" patch | grep -qx 'current: v1.2.3' || { echo "FAIL: pre-release counted as latest"; exit 1; }
# refuses off main and with a dirty tree
git checkout -q -b topic
if sh "$here/bump-version.sh" patch --tag >/dev/null 2>&1; then echo "FAIL: tagged off main"; exit 1; fi
git checkout -q main
echo x >dirty.txt
if sh "$here/bump-version.sh" patch --tag >/dev/null 2>&1; then echo "FAIL: tagged a dirty tree"; exit 1; fi
echo "bump-version_test: ok"
