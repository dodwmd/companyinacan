#!/bin/sh
# Self-test: the default is a dry run that never calls gh; --apply makes the four calls.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir "$tmp/bin"
cat >"$tmp/bin/gh" <<'EOF'
#!/bin/sh
echo "$*" >>"$GH_LOG"
case "$*" in
*--input*) cat >>"$GH_LOG" ;;
*) echo 4242 ;;
esac
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
echo "$out" | grep -q '"required_linear_history": false' || { echo "FAIL: linear history must be opt-in"; exit 1; }
echo "$out" | grep -q 'LOCK OUT merging' || { echo "FAIL: review lock-out not explained"; exit 1; }
echo "$out" | grep -q '"required_pull_request_reviews": null' || { echo "FAIL: default must require no reviews"; exit 1; }
echo "$out" | grep -q '"include": \["refs/tags/v\*"\]' || { echo "FAIL: tag ruleset missing"; exit 1; }
echo "$out" | grep -q '"custom_branch_policies": true' || { echo "FAIL: environment tag policy missing"; exit 1; }
echo "$out" | grep -q '"prevent_self_review": false' || { echo "FAIL: single reviewer must not forbid self review"; exit 1; }

sh "$here/gh-protect-main.sh" --apply owner/repo >/dev/null
for want in 'api --method PUT repos/owner/repo/branches/main/protection' \
	'api --method POST repos/owner/repo/rulesets' \
	'api users/owner' \
	'api --method PUT repos/owner/repo/environments/release' \
	'api --method POST repos/owner/repo/environments/release/deployment-branch-policies'; do
	grep -q -- "$want" "$GH_LOG" || { echo "FAIL: apply did not call: $want"; exit 1; }
done
grep -q '"id": 4242' "$GH_LOG" || { echo "FAIL: resolved reviewer id missing"; exit 1; }

if REVIEWS=x sh "$here/gh-protect-main.sh" >/dev/null 2>&1; then echo "FAIL: bad REVIEWS accepted"; exit 1; fi
if RELEASE_REVIEWERS='a b' sh "$here/gh-protect-main.sh" >/dev/null 2>&1; then echo "FAIL: bad reviewer list accepted"; exit 1; fi
REVIEWS=1 sh "$here/gh-protect-main.sh" | grep -q '"required_approving_review_count": 1' || { echo "FAIL: REVIEWS ignored"; exit 1; }
LINEAR_HISTORY=1 sh "$here/gh-protect-main.sh" | grep -q '"required_linear_history": true' || { echo "FAIL: LINEAR_HISTORY ignored"; exit 1; }
RELEASE_REVIEWERS=a,b sh "$here/gh-protect-main.sh" | grep -q '"prevent_self_review": true' || { echo "FAIL: two reviewers should prevent self review"; exit 1; }
echo "gh-protect-main_test: ok"
