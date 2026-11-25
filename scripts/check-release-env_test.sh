#!/bin/sh
# Self-test: the publish guard fails closed. Needs jq (preinstalled on GitHub runners).
set -eu
here=$(cd "$(dirname "$0")" && pwd)
command -v jq >/dev/null 2>&1 || { echo "check-release-env_test: jq missing, skipped"; exit 0; }
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
export GITHUB_REPOSITORY=o/r
run() { RELEASE_ENV_JSON="$tmp/$1.json" sh "$here/check-release-env.sh" >/dev/null 2>&1; }

cat >"$tmp/good.json" <<'EOF'
{"name":"release","protection_rules":[{"id":1,"type":"required_reviewers","reviewers":[{"type":"User","reviewer":{"login":"a"}}]}],"deployment_branch_policy":{"protected_branches":false,"custom_branch_policies":true}}
EOF
run good || { echo "FAIL: protected environment rejected"; exit 1; }

# GitHub auto-creates an unprotected environment: no rules at all
echo '{"name":"release","protection_rules":[],"deployment_branch_policy":null}' >"$tmp/bare.json"
if run bare; then echo "FAIL: unprotected environment accepted"; exit 1; fi

# a wait timer is not a reviewer
echo '{"protection_rules":[{"type":"wait_timer","wait_timer":5}],"deployment_branch_policy":{"custom_branch_policies":true}}' >"$tmp/timer.json"
if run timer; then echo "FAIL: wait timer accepted as protection"; exit 1; fi

# reviewers but deployable from any branch or tag
echo '{"protection_rules":[{"type":"required_reviewers","reviewers":[{"type":"User"}]}],"deployment_branch_policy":null}' >"$tmp/anyref.json"
if run anyref; then echo "FAIL: unrestricted deployment refs accepted"; exit 1; fi

# unreadable environment: fails closed, unless a maintainer set the variable
mkdir "$tmp/bin"
printf '#!/bin/sh\nexit 1\n' >"$tmp/bin/gh"
chmod +x "$tmp/bin/gh"
if PATH="$tmp/bin:$PATH" sh "$here/check-release-env.sh" >/dev/null 2>&1; then echo "FAIL: unreadable environment accepted"; exit 1; fi
PATH="$tmp/bin:$PATH" RELEASE_ENV_PROTECTED=true sh "$here/check-release-env.sh" >/dev/null 2>&1 || { echo "FAIL: variable fallback ignored"; exit 1; }
if PATH="$tmp/bin:$PATH" RELEASE_ENV_PROTECTED=yes sh "$here/check-release-env.sh" >/dev/null 2>&1; then echo "FAIL: loose variable value accepted"; exit 1; fi
echo "check-release-env_test: ok"
