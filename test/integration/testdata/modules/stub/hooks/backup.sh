#!/bin/sh
# Test backup hook of the stub module (E08b): archive a file from the module's data volume and PUT
# it to can-core through the dump API, with the pod's projected audience-`can` token.
set -eu

: "${CAN_API_URL:?}" "${CAN_MODULE:?}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
umask 077
printf 'Authorization: Bearer %s\n' "$(cat /var/run/secrets/can/token)" >"$tmp/auth"

echo "stub-data-from-the-volume" >/data/probe
tar -C /data -czf "$tmp/probe.tar.gz" probe

# A new pod's NetworkPolicy rules are programmed a moment after it starts: retry refused connections.
curl --fail --silent --show-error --retry 10 --retry-delay 2 --retry-all-errors \
	--connect-timeout 5 --max-time 60 -X PUT -H @"$tmp/auth" \
	--data-binary @"$tmp/probe.tar.gz" "$CAN_API_URL/v1/dumps/$CAN_MODULE/probe.tar.gz"
echo "stub dumped"
