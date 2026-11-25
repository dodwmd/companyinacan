#!/bin/sh
# Fail-closed guard for the publish jobs (epic E20): refuses to continue unless the `release`
# environment has required reviewers AND restricts deployments to a custom policy (tags). GitHub creates an
# unprotected environment silently when a job names one that does not exist, so the name alone protects nothing.
#   scripts/check-release-env.sh        env: GITHUB_REPOSITORY, GH_TOKEN (the job needs `actions: read`)
# Test hook: RELEASE_ENV_JSON=<file> reads the API answer from a file instead of calling the API.
# If the API cannot be read (403/404) the guard falls back to the repository variable
# RELEASE_ENV_PROTECTED=true, which a maintainer sets ONLY after configuring the environment
# (docs/maintainers.md); anything else fails closed.
set -eu
repo="${GITHUB_REPOSITORY:-}"
json=""
if [ -n "${RELEASE_ENV_JSON:-}" ]; then
	json=$(cat "$RELEASE_ENV_JSON")
elif [ -n "$repo" ] && json=$(gh api "repos/$repo/environments/release" 2>/dev/null); then
	:
else
	json=""
fi

if [ -z "$json" ]; then
	if [ "${RELEASE_ENV_PROTECTED:-}" = true ]; then
		echo "check-release-env: could not read the environment; trusting RELEASE_ENV_PROTECTED=true set by a maintainer" >&2
		exit 0
	fi
	echo "check-release-env: cannot read the 'release' environment and RELEASE_ENV_PROTECTED is not 'true': refusing to publish" >&2
	exit 1
fi

reviewers=$(printf '%s' "$json" | jq '[.protection_rules[]? | select(.type == "required_reviewers") | .reviewers[]?] | length')
custom=$(printf '%s' "$json" | jq '.deployment_branch_policy.custom_branch_policies // false')
if [ "$reviewers" -lt 1 ]; then
	echo "check-release-env: the 'release' environment has no required reviewers: refusing to publish (docs/maintainers.md)" >&2
	exit 1
fi
if [ "$custom" != true ]; then
	echo "check-release-env: the 'release' environment does not restrict deployments to a custom (tag) policy: refusing to publish" >&2
	exit 1
fi
echo "check-release-env: ok ($reviewers required reviewer(s), custom deployment policy)"
