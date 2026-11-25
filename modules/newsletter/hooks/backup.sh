#!/bin/sh
# Backup hook (SPEC 6.5): pg_dump the Listmonk database and PUT it to can-core, which writes it
# into the host dumps directory. Runs as a Job in this module's namespace with the hooks image
# (deploy/hooks) and these variables, injected by can-core:
#   CAN_API_URL  CAN_MODULE  [CAN_TOKEN_FILE]   module API and projected audience-`can` token
#   DB_HOST DB_PORT DB_NAME DB_USER DB_PASSWORD  from the module's `can-database` Secret
# The dump endpoint path is provisional until epic E08 fixes it (contracts/hooks.md).
set -eu

: "${CAN_API_URL:?}" "${CAN_MODULE:?}" "${DB_HOST:?}" "${DB_NAME:?}" "${DB_USER:?}" "${DB_PASSWORD:?}"
token_file="${CAN_TOKEN_FILE:-/var/run/secrets/can/token}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# The bearer token goes to curl through a 0600 header file, never through argv (visible in /proc).
umask 077
printf 'Authorization: Bearer %s\n' "$(cat "$token_file")" >"$tmp/auth"

# pg_dump takes a consistent snapshot (one transaction) without blocking the live app.
PGPASSWORD="$DB_PASSWORD" pg_dump \
	--host="$DB_HOST" --port="${DB_PORT:-5432}" --username="$DB_USER" \
	--no-owner --no-privileges --clean --if-exists \
	"$DB_NAME" >"$tmp/dump.sql"
# (not piped: POSIX sh has no pipefail, and a failed dump must fail the Job)
gzip -c "$tmp/dump.sql" >"$tmp/dump.sql.gz"

# An empty or truncated dump must never replace a good one.
[ "$(wc -c <"$tmp/dump.sql.gz")" -gt 100 ] || { echo "backup: dump is implausibly small" >&2; exit 1; }
gzip -t "$tmp/dump.sql.gz"

curl --fail --silent --show-error --retry 3 \
	-X PUT -H @"$tmp/auth" --connect-timeout 10 --max-time 600 \
	--data-binary @"$tmp/dump.sql.gz" \
	"$CAN_API_URL/v1/dumps/$CAN_MODULE/database.sql.gz"
echo "backup: $CAN_MODULE database dumped"
