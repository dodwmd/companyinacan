#!/bin/sh
# Self-test for release-notes.sh using a throwaway repository.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
cd "$tmp"
git init -q -b main .
git config user.name T
git config user.email t@example.com
git config commit.gpgsign false
git commit -q --allow-empty -m "chore: init"
git tag v0.1.0
git commit -q --allow-empty -m "feat(forms): add a thing"
git commit -q --allow-empty -m "fix: repair \$(touch pwned) \`id\`"
git commit -q --allow-empty -m "feat!: break it"
git commit -q --allow-empty -m "Random message"
git commit -q --allow-empty -m "security: patch"
git tag v0.2.0
out=$(sh "$here/release-notes.sh" v0.2.0)
echo "$out" | grep -q '^### Features' || { echo "FAIL: no features"; exit 1; }
echo "$out" | grep -q -- '- add a thing' || { echo "FAIL: scope not stripped"; exit 1; }
echo "$out" | grep -q '^### Breaking changes' || { echo "FAIL: breaking missing"; exit 1; }
echo "$out" | grep -q '^### Security' || { echo "FAIL: security missing"; exit 1; }
echo "$out" | grep -q -- '- Random message' || { echo "FAIL: other missing"; exit 1; }
echo "$out" | grep -q 'v0.1.0...v0.2.0' || { echo "FAIL: range missing"; exit 1; }
if echo "$out" | grep -q 'init'; then echo "FAIL: previous release commit leaked"; exit 1; fi
[ ! -e pwned ] || { echo "FAIL: subject was executed"; exit 1; }
# first release: no previous tag
sh "$here/release-notes.sh" v0.1.0 | grep -q -- '- init' || { echo "FAIL: first release"; exit 1; }
echo "release-notes_test: ok"
