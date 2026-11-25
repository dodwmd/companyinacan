#!/bin/sh
# Prints (default) or applies (--apply) the repository protections of epic E20:
#   1. branch protection of `main`,
#   2. a tag ruleset: only repository admins may create, update or delete `v*` tags,
#   3. the `release` environment: required reviewers and deployments limited to `v*` tags.
#   scripts/gh-protect-main.sh [--apply] [owner/repo]
# A dry run needs no network and no credentials: it prints the exact API calls and payloads. --apply
# calls the GitHub API with the `gh` CLI and needs a token with repository admin rights. Nothing in CI
# runs this script. The required checks are the job names of .github/workflows/ci.yml (specs/001):
# a contract test keeps this list and ci.yml in step. security.yml, footprint.yml and dev-cluster.yml
# are deliberately NOT required (docs/maintainers.md says why).
#
# Why 2 and 3 matter: release.yml checks that a tag is on main, but a pushed tag can carry a commit that
# edits release.yml itself, so that check is not a security boundary. The boundary is who may create
# `v*` tags (2) and the reviewers of the `release` environment (3); the publish jobs also refuse to run
# if the environment has no required reviewers (scripts/check-release-env.sh).
#
# Environment (all optional):
#   REVIEWS            approving reviews required on main (default 0). A one-maintainer project cannot approve its
#                      own pull requests, so any value >= 1 LOCKS OUT merging until a second maintainer exists;
#                      with REVIEWS >= 1 code-owner review (.github/CODEOWNERS) is required as well.
#   LINEAR_HISTORY     1 to require linear history (default off: this repository merges with merge commits).
#   ENFORCE_ADMINS     true|false (default false).
#   RELEASE_REVIEWERS  comma-separated logins that must approve a release (default: the repository owner).
#   PREVENT_SELF_REVIEW true|false. Default true when there are two or more reviewers, false with one: with a
#                      single reviewer, true would make it impossible to release your own tag.
set -eu
apply=0
if [ "${1:-}" = "--apply" ]; then apply=1; shift; fi
repo="${1:-dodwmd/companyinacan}"
REQUIRED_CHECKS="lint unit contract licences dco"
reviews="${REVIEWS:-0}"
admins="${ENFORCE_ADMINS:-false}"
linear="${LINEAR_HISTORY:-0}"
reviewers="${RELEASE_REVIEWERS:-${repo%%/*}}"
case "$reviews" in '' | *[!0-9]*) echo "REVIEWS must be a number" >&2; exit 2 ;; esac
case "$admins" in true | false) ;; *) echo "ENFORCE_ADMINS must be true or false" >&2; exit 2 ;; esac
case "$linear" in 0 | 1) ;; *) echo "LINEAR_HISTORY must be 0 or 1" >&2; exit 2 ;; esac
case "$reviewers" in '' | *[!A-Za-z0-9,-]*) echo "RELEASE_REVIEWERS must be comma-separated GitHub logins" >&2; exit 2 ;; esac
nrev=$(printf '%s' "$reviewers" | tr ',' '\n' | grep -c .)
if [ "$nrev" -ge 2 ]; then default_self=true; else default_self=false; fi
self="${PREVENT_SELF_REVIEW:-$default_self}"
case "$self" in true | false) ;; *) echo "PREVENT_SELF_REVIEW must be true or false" >&2; exit 2 ;; esac

contexts=""
for c in $REQUIRED_CHECKS; do contexts="$contexts${contexts:+, }\"$c\""; done
if [ "$reviews" -gt 0 ]; then
	pr="{\"required_approving_review_count\": $reviews, \"dismiss_stale_reviews\": true, \"require_code_owner_reviews\": true}"
else
	pr="null"
fi
if [ "$linear" = 1 ]; then linear_json=true; else linear_json=false; fi
branch_payload=$(cat <<EOF
{
  "required_status_checks": {"strict": true, "contexts": [$contexts]},
  "enforce_admins": $admins,
  "required_pull_request_reviews": $pr,
  "restrictions": null,
  "required_linear_history": $linear_json,
  "allow_force_pushes": false,
  "allow_deletions": false,
  "required_conversation_resolution": true
}
EOF
)

# actor_id 5 is the built-in "Repository admin" role: only admins (and nobody else) may touch v* tags.
ruleset_payload=$(cat <<'EOF'
{
  "name": "release-tags",
  "target": "tag",
  "enforcement": "active",
  "conditions": {"ref_name": {"include": ["refs/tags/v*"], "exclude": []}},
  "bypass_actors": [{"actor_id": 5, "actor_type": "RepositoryRole", "bypass_mode": "always"}],
  "rules": [{"type": "creation"}, {"type": "update"}, {"type": "deletion"}, {"type": "non_fast_forward"}]
}
EOF
)

# Reviewer ids are looked up with the API when applying; the dry run shows placeholders.
env_payload() { # $1 = JSON array of reviewers
	cat <<EOF
{
  "wait_timer": 0,
  "prevent_self_review": $self,
  "reviewers": $1,
  "deployment_branch_policy": {"protected_branches": false, "custom_branch_policies": true}
}
EOF
}
policy_payload='{"name": "v*", "type": "tag"}'

echo "repository:      $repo"
echo "== 1. branch protection of main"
echo "required checks: $REQUIRED_CHECKS"
echo "request:         PUT repos/$repo/branches/main/protection"
echo "$branch_payload"
if [ "$reviews" -eq 0 ]; then
	echo "note: no approving review is required (REVIEWS=0): a solo maintainer cannot approve their own pull requests."
	echo "      Setting REVIEWS>=1 (with code-owner review) would LOCK OUT merging until a second maintainer exists."
else
	echo "WARNING: REVIEWS=$reviews requires approving reviews and code-owner review: with a single maintainer nobody can merge."
fi
if [ "$linear" = 0 ]; then
	echo "note: linear history is NOT required (this repository merges with merge commits); LINEAR_HISTORY=1 opts in."
fi
echo "== 2. tag ruleset: only repository admins may create, update or delete v* tags"
echo "request:         POST repos/$repo/rulesets"
echo "$ruleset_payload"
echo "== 3. release environment: required reviewers ($reviewers), prevent_self_review=$self, deployments from v* tags only"
echo "request:         PUT repos/$repo/environments/release   then   POST repos/$repo/environments/release/deployment-branch-policies"
env_payload '[{"type": "User", "id": "<resolved from users/<login> when applying>"}]'
echo "$policy_payload"
if [ "$self" = false ]; then
	echo "note: with a single reviewer self-review stays allowed (otherwise the only reviewer could never release); add a second reviewer, then the default becomes true."
fi
echo "after applying, set the repository variable RELEASE_ENV_PROTECTED=true only if the API cannot be read by the workflow token (docs/maintainers.md)."
if [ "$apply" -ne 1 ]; then
	echo "(dry run: nothing was sent; pass --apply to apply)"
	exit 0
fi
command -v gh >/dev/null 2>&1 || { echo "gh CLI not installed" >&2; exit 1; }
printf '%s\n' "$branch_payload" | gh api --method PUT "repos/$repo/branches/main/protection" --input -
printf '%s\n' "$ruleset_payload" | gh api --method POST "repos/$repo/rulesets" --input -
rev_json=""
for login in $(printf '%s' "$reviewers" | tr ',' ' '); do
	id=$(gh api "users/$login" --jq .id)
	rev_json="$rev_json${rev_json:+, }{\"type\": \"User\", \"id\": $id}"
done
env_payload "[$rev_json]" | gh api --method PUT "repos/$repo/environments/release" --input -
printf '%s\n' "$policy_payload" | gh api --method POST "repos/$repo/environments/release/deployment-branch-policies" --input -
echo "applied"
