#!/bin/sh
# Verify every non-merge commit in base..head carries a Signed-off-by trailer
# matching the commit author (by email or name). Usage: check-dco.sh <base> <head>
set -eu

if [ $# -ne 2 ]; then
  echo "usage: $0 <base-ref> <head-ref>" >&2
  exit 2
fi
base=$1
head=$2
bad=0

for c in $(git rev-list --no-merges "$base..$head"); do
  name=$(git log -1 --format=%an "$c")
  email=$(git log -1 --format=%ae "$c")
  trailers=$(git log -1 --format=%B "$c" | grep -E '^Signed-off-by: .+ <.+>[[:space:]]*$' || true)
  if [ -z "$trailers" ] || { ! printf '%s\n' "$trailers" | grep -qF "<$email>" && ! printf '%s\n' "$trailers" | grep -qF "Signed-off-by: $name <"; }; then
    echo "DCO: commit $(git log -1 --format='%h %s' "$c") has no Signed-off-by matching its author ($name <$email>)" >&2
    bad=1
  fi
done

if [ "$bad" -ne 0 ]; then
  cat >&2 <<'MSG'

Every commit must be signed off to certify the Developer Certificate of Origin (https://developercertificate.org).
Fix with:   git commit --amend -s            (last commit)
       or:  git rebase --signoff <base>      (several commits), then force-push your branch.
MSG
  exit 1
fi
echo "DCO: all commits are signed off"
