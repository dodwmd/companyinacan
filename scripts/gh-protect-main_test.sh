#!/bin/sh
# Self-test: the default is a dry run that never calls gh; --apply calls it once with the payload.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir "$tmp/bin"
cat >"$tmp/bin/gh" <<'EOF'
#!/bin/sh
echo "$*" >>"$GH_LOG"
cat >>"$GH_LOG"
EOF
chmod +x "$tmp/bin/gh"
export GH_LOG="$tmp/gh.log"
PATH="$tmp/bin:$PATH"
export PATH

out=$(sh "$here/gh-protect-main.sh" owner/repo)
[ ! -e "$GH_LOG" ] || { echo "FAIL: dry run called gh"; exit 1; }
echo "$out" | grep -q 'required checks: lint unit contract licences dco' || { echo "FAIL: checks not printed"; exit 1; }
echo "$out" | grep -q '"contexts": \["lint", "unit", "contract", "licences", "dco"\]' || { echo "FAIL: payload contexts"; exit 1; }
echo "$out" | grep -q 'dry run' || { echo "FAIL: not labelled a dry run"; exit 1; }

sh "$here/gh-protect-main.sh" --apply owner/repo >/dev/null
grep -q 'api --method PUT repos/owner/repo/branches/main/protection' "$GH_LOG" || { echo "FAIL: apply did not call the API"; exit 1; }

if REVIEWS=x sh "$here/gh-protect-main.sh" >/dev/null 2>&1; then echo "FAIL: bad REVIEWS accepted"; exit 1; fi
REVIEWS=1 sh "$here/gh-protect-main.sh" | grep -q '"required_approving_review_count": 1' || { echo "FAIL: REVIEWS ignored"; exit 1; }
echo "gh-protect-main_test: ok"
