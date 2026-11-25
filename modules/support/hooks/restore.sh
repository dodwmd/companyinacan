#!/bin/sh
# Restore hook (SPEC 6.5 step "restore databases"): GET the dump from can-core and load it into
# the freshly created, empty database. Same environment as backup.sh. The dump carries
# DROP TABLE IF EXISTS, so re-running over a partly restored database is safe.
set -eu

: "${CAN_API_URL:?}" "${CAN_MODULE:?}" "${DB_HOST:?}" "${DB_NAME:?}" "${DB_USER:?}" "${DB_PASSWORD:?}"
token_file="${CAN_TOKEN_FILE:-/var/run/secrets/can/token}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

curl --fail --silent --show-error --retry 3 \
	-H "Authorization: Bearer $(cat "$token_file")" \
	-o "$tmp/dump.sql.gz" \
	"$CAN_API_URL/v1/dumps/$CAN_MODULE/database.sql.gz"
gzip -t "$tmp/dump.sql.gz"

gzip -dc "$tmp/dump.sql.gz" | MYSQL_PWD="$DB_PASSWORD" mariadb \
	--host="$DB_HOST" --port="${DB_PORT:-3306}" --user="$DB_USER" "$DB_NAME"
echo "restore: $CAN_MODULE database restored"
