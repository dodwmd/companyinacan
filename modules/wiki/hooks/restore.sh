#!/bin/sh
# Restore hook (SPEC 6.5 step "restore databases"): GET the storage archive from can-core and unpack
# it onto the module's (new, empty) data volume, replacing whatever is there. can-core scales the app
# to zero first, so nothing writes while the files are replaced. Same environment as backup.sh.
set -eu

: "${CAN_API_URL:?}" "${CAN_MODULE:?}"
data="${WIKI_DATA:-/storage}"
token_file="${CAN_TOKEN_FILE:-/var/run/secrets/can/token}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# The bearer token goes to curl through a 0600 header file, never through argv (visible in /proc).
umask 077
printf 'Authorization: Bearer %s\n' "$(cat "$token_file")" >"$tmp/auth"

curl --fail --silent --show-error --retry 3 \
	-H @"$tmp/auth" --connect-timeout 10 --max-time 600 \
	-o "$tmp/storage.tar.gz" \
	"$CAN_API_URL/v1/dumps/$CAN_MODULE/storage.tar.gz"
gzip -t "$tmp/storage.tar.gz"
tar -tzf "$tmp/storage.tar.gz" >/dev/null

# The data volume must really be mounted: unpacking into the Job's own filesystem would restore
# nothing and report success. (The Job pod needs fsGroup 1000 to write it, contracts/hooks.md.)
mountpoint -q "$data" || { echo "restore: $data is not a mounted volume" >&2; exit 1; }
# replace-all: stale files from an earlier life must not survive next to the restored ones
find "$data" -mindepth 1 -maxdepth 1 -exec rm -rf {} +
tar -C "$data" -xzf "$tmp/storage.tar.gz"
echo "restore: $CAN_MODULE storage restored"
