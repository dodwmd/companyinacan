#!/bin/sh
# Backup hook (SPEC 6.5): take a consistent SQLite snapshot of the Uptime Kuma database and PUT
# it to can-core, which writes it into the host dumps directory. SQLite files on PVCs are
# excluded from the raw PVC backup, so this dump is the only copy that restic sees.
# Runs as a Job in this module's namespace with the hooks image (deploy/hooks), the module's
# data volume mounted at STATUS_DATA (default /app/data), and:
#   CAN_API_URL  CAN_MODULE  [CAN_TOKEN_FILE]   module API and projected audience-`can` token
# The dump endpoint path is provisional until epic E08 fixes it (contracts/hooks.md).
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

[ -f "$db" ] || { echo "backup: $db not found (is the data volume mounted?)" >&2; exit 1; }

# .backup uses SQLite's online backup API: consistent even while Uptime Kuma writes (WAL).
sqlite3 "$db" ".timeout 10000" ".backup '$tmp/kuma.db'"
[ "$(sqlite3 "$tmp/kuma.db" 'PRAGMA integrity_check;')" = "ok" ] || { echo "backup: integrity check failed" >&2; exit 1; }
gzip -c "$tmp/kuma.db" >"$tmp/dump.sqlite3.gz"

curl --fail --silent --show-error --retry 3 \
	-X PUT -H @"$tmp/auth" --connect-timeout 10 --max-time 600 \
	--data-binary @"$tmp/dump.sqlite3.gz" \
	"$CAN_API_URL/v1/dumps/$CAN_MODULE/kuma.db.gz"
echo "backup: $CAN_MODULE database dumped"
