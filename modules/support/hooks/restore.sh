#!/bin/sh
# Restore hook (SPEC 6.5 step "restore databases"): GET the dump from can-core and load it into
# the freshly created, empty database. Same environment as backup.sh. The dump carries
# DROP ... IF EXISTS (pg_dump --clean --if-exists), and psql stops at the first error so a
# half-applied restore is never reported as success.
set -eu

: "${CAN_API_URL:?}" "${CAN_MODULE:?}" "${DB_HOST:?}" "${DB_NAME:?}" "${DB_USER:?}" "${DB_PASSWORD:?}"
token_file="${CAN_TOKEN_FILE:-/var/run/secrets/can/token}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# The bearer token goes to curl through a 0600 header file, never through argv (visible in /proc).
umask 077
printf 'Authorization: Bearer %s\n' "$(cat "$token_file")" >"$tmp/auth"

curl --fail --silent --show-error --retry 3 \
	-H @"$tmp/auth" --connect-timeout 10 --max-time 600 \
	-o "$tmp/dump.sql.gz" \
	"$CAN_API_URL/v1/dumps/$CAN_MODULE/database.sql.gz"
gzip -t "$tmp/dump.sql.gz"

gzip -dc "$tmp/dump.sql.gz" | PGPASSWORD="$DB_PASSWORD" psql \
	--host="$DB_HOST" --port="${DB_PORT:-5432}" --username="$DB_USER" --dbname="$DB_NAME" \
	--set=ON_ERROR_STOP=1 --quiet --single-transaction
echo "restore: $CAN_MODULE database restored"
