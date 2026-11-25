#!/bin/sh
# Restore hook (SPEC 6.5 step "restore databases"): GET the snapshot from can-core and put it in
# place on the module's (new, empty) data volume. can-core scales the app to zero first, so no
# process holds the file; stale -wal and -shm files from an earlier life are removed. Same
# environment as backup.sh.
set -eu

: "${CAN_API_URL:?}" "${CAN_MODULE:?}"
data="${GOATCOUNTER_DATA:-/data}"
db="$data/goatcounter.sqlite3"
token_file="${CAN_TOKEN_FILE:-/var/run/secrets/can/token}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

curl --fail --silent --show-error --retry 3 \
	-H "Authorization: Bearer $(cat "$token_file")" \
	-o "$tmp/dump.sqlite3.gz" \
	"$CAN_API_URL/v1/dumps/$CAN_MODULE/goatcounter.sqlite3.gz"
gzip -t "$tmp/dump.sqlite3.gz"
gzip -dc "$tmp/dump.sqlite3.gz" >"$tmp/goatcounter.sqlite3"
[ "$(sqlite3 "$tmp/goatcounter.sqlite3" 'PRAGMA integrity_check;')" = "ok" ] || { echo "restore: snapshot is corrupt" >&2; exit 1; }

rm -f "$db-wal" "$db-shm"
cp "$tmp/goatcounter.sqlite3" "$db.restoring"
mv "$db.restoring" "$db"
echo "restore: $CAN_MODULE database restored"
