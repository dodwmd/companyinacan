#!/bin/sh
# Prints (default) or applies (--apply) the branch protection of `main` (epic E20).
#   scripts/gh-protect-main.sh [--apply] [owner/repo]
# A dry run needs no network and no credentials: it prints the exact API call and payload. --apply
# calls the GitHub API with the `gh` CLI and needs a token with repository admin rights. Nothing in CI
# runs this script. The required checks are the job names of .github/workflows/ci.yml (specs/001):
# a contract test keeps this list and ci.yml in step. security.yml, footprint.yml and dev-cluster.yml
# are deliberately NOT required (docs/maintainers.md says why).
# Environment: REVIEWS (approving reviews required, default 0: a one-maintainer project cannot approve
# its own pull requests; raise it when a second maintainer exists), ENFORCE_ADMINS (true|false, default false).
set -eu
apply=0
if [ "${1:-}" = "--apply" ]; then apply=1; shift; fi
repo="${1:-dodwmd/companyinacan}"
REQUIRED_CHECKS="lint unit contract licences dco"
reviews="${REVIEWS:-0}"
admins="${ENFORCE_ADMINS:-false}"
case "$reviews" in '' | *[!0-9]*) echo "REVIEWS must be a number" >&2; exit 2 ;; esac
case "$admins" in true | false) ;; *) echo "ENFORCE_ADMINS must be true or false" >&2; exit 2 ;; esac

contexts=""
for c in $REQUIRED_CHECKS; do contexts="$contexts${contexts:+, }\"$c\""; done
if [ "$reviews" -gt 0 ]; then
	pr="{\"required_approving_review_count\": $reviews, \"dismiss_stale_reviews\": true, \"require_code_owner_reviews\": true}"
else
	pr="null"
fi
payload=$(cat <<EOF
{
  "required_status_checks": {"strict": true, "contexts": [$contexts]},
  "enforce_admins": $admins,
  "required_pull_request_reviews": $pr,
  "restrictions": null,
  "required_linear_history": true,
  "allow_force_pushes": false,
  "allow_deletions": false,
  "required_conversation_resolution": true
}
EOF
)

echo "repository:      $repo"
echo "branch:          main"
echo "required checks: $REQUIRED_CHECKS"
echo "request:         PUT repos/$repo/branches/main/protection"
echo "$payload"
if [ "$apply" -ne 1 ]; then
	echo "(dry run: nothing was sent; pass --apply to apply)"
	exit 0
fi
command -v gh >/dev/null 2>&1 || { echo "gh CLI not installed" >&2; exit 1; }
printf '%s\n' "$payload" | gh api --method PUT "repos/$repo/branches/main/protection" --input -
echo "applied"
