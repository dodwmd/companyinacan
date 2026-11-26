#!/bin/sh
# Test restore hook of the stub module (specs/042-restore): download the dump through the dump API
# (only a can-core in restore mode serves it) and unpack it onto the module's volume.
set -eu

: "${CAN_API_URL:?}" "${CAN_MODULE:?}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
umask 077
printf 'Authorization: Bearer %s\n' "$(cat /var/run/secrets/can/token)" >"$tmp/auth"

curl --fail --silent --show-error --retry 10 --retry-delay 2 --retry-all-errors \
	--connect-timeout 5 --max-time 60 -H @"$tmp/auth" \
	-o "$tmp/probe.tar.gz" "$CAN_API_URL/v1/dumps/$CAN_MODULE/probe.tar.gz"
grep -q " /data " /proc/mounts || { echo "no volume at /data" >&2; exit 1; }
umask 022
rm -rf /data/restored-by-hook
mkdir /data/restored-by-hook
tar -C /data/restored-by-hook -xzf "$tmp/probe.tar.gz"
echo "stub restored"
