#!/bin/sh
# Restore hook (SPEC 6.5 step "restore databases"): GET the snapshot from can-core and put it in
# place on the module's (new, empty) data volume. can-core scales the app to zero first, so no
# process holds the file; stale -wal and -shm files from an earlier life are removed. Same
# environment as backup.sh.
set -eu

: "${CAN_API_URL:?}" "${CAN_MODULE:?}"
data="${STATUS_DATA:-/app/data}"
db="$data/kuma.db"
token_file="${CAN_TOKEN_FILE:-/var/run/secrets/can/token}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# The bearer token goes to curl through a 0600 header file, never through argv (visible in /proc).
umask 077
printf 'Authorization: Bearer %s\n' "$(cat "$token_file")" >"$tmp/auth"

curl --fail --silent --show-error --retry 3 \
	-H @"$tmp/auth" --connect-timeout 10 --max-time 600 \
	-o "$tmp/dump.sqlite3.gz" \
	"$CAN_API_URL/v1/dumps/$CAN_MODULE/kuma.db.gz"
gzip -t "$tmp/dump.sqlite3.gz"
gzip -dc "$tmp/dump.sqlite3.gz" >"$tmp/kuma.db"
[ "$(sqlite3 "$tmp/kuma.db" 'PRAGMA integrity_check;')" = "ok" ] || { echo "restore: snapshot is corrupt" >&2; exit 1; }

# The data volume must really be mounted: writing into the Job's own filesystem would restore
# nothing and report success. (The Job pod needs fsGroup 1000 to write it, contracts/hooks.md.)
mountpoint -q "$data" || { echo "restore: $data is not a mounted volume" >&2; exit 1; }
rm -f "$db-wal" "$db-shm"
cp "$tmp/kuma.db" "$db.restoring"
mv "$db.restoring" "$db"
echo "restore: $CAN_MODULE database restored"
