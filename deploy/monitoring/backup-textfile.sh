#!/bin/sh
# Writes can_backup_last_success_timestamp_seconds for the node_exporter textfile collector, from
# `can backup status --output json`. Run it from the host timer that runs the backup (or cron, every
# 15 minutes), for example:
#
#   backup-textfile.sh /var/lib/node_exporter/textfile/can_backup.prom
#
# The CanBackupStale alert (alerts.yml) fires when the value is older than 36 hours. If there is no
# snapshot or the status command fails, no series is written, and `absent()` rules / the missing-series
# state tell you something is wrong; the script never writes a fresh timestamp it did not read.
set -eu
out="${1:?usage: backup-textfile.sh <path-to-.prom-file>}"
status="$(can backup status --output json 2>/dev/null)" || status=""
# latest_at is RFC 3339; the zero time means "no snapshot".
latest="$(printf '%s' "$status" | sed -n 's/.*"latest_at":"\([^"]*\)".*/\1/p')"
tmp="$(mktemp "${out}.XXXXXX")"
trap 'rm -f "$tmp"' EXIT
if [ -n "$latest" ] && ts="$(date -d "$latest" +%s 2>/dev/null)" && [ "$ts" -gt 0 ]; then
  {
    echo "# HELP can_backup_last_success_timestamp_seconds Unix time of the newest backup snapshot."
    echo "# TYPE can_backup_last_success_timestamp_seconds gauge"
    echo "can_backup_last_success_timestamp_seconds $ts"
  } >"$tmp"
else
  : >"$tmp"
fi
chmod 0644 "$tmp"
mv "$tmp" "$out"
trap - EXIT
