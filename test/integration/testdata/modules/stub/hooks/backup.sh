#!/bin/sh
# Test backup hook of the stub module (E08b): archive a file from the module's data volume and PUT
# it to can-core through the dump API, with the pod's projected audience-`can` token.
set -eu

: "${CAN_API_URL:?}" "${CAN_MODULE:?}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
umask 077
printf 'Authorization: Bearer %s\n' "$(cat /var/run/secrets/can/token)" >"$tmp/auth"

# The stub declares no database, so its volume must be mounted, read-only.
grep -q " /data " /proc/mounts || { echo "no volume at /data" >&2; exit 1; }
if touch /data/probe 2>/dev/null; then echo "the volume is writable" >&2; exit 1; fi
echo "stub-data-from-the-hook" >"$tmp/probe"
tar -C "$tmp" -czf "$tmp/probe.tar.gz" probe

# A new pod's NetworkPolicy rules are programmed a moment after it starts: retry refused connections.
curl --fail --silent --show-error --retry 10 --retry-delay 2 --retry-all-errors \
	--connect-timeout 5 --max-time 60 -X PUT -H @"$tmp/auth" \
	-T "$tmp/probe.tar.gz" "$CAN_API_URL/v1/dumps/$CAN_MODULE/probe.tar.gz"
echo "stub dumped"
