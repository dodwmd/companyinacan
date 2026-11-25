#!/bin/sh
# Writes backup metrics for the node_exporter textfile collector, from `can backup status --output json`.
# Run it from the host timer that runs the backup (or cron, every 15 minutes), for example:
#
#   backup-textfile.sh /var/lib/node_exporter/textfile/can_backup.prom
#
# Series written:
#   can_backup_last_success_timestamp_seconds  newest snapshot time; KEPT from the previous run when this
#                                              run cannot read one, so a failing status command does not
#                                              make the series disappear (CanBackupStale keeps working)
#   can_backup_last_run_ok                     1 when this run read a snapshot, 0 otherwise
#
# The script never writes a timestamp it did not read. If there has never been a snapshot, only
# can_backup_last_run_ok 0 is written and the CanBackupMissing alert fires on the absent timestamp.
set -eu
out="${1:?usage: backup-textfile.sh <path-to-.prom-file>}"
status="$(can backup status --output json 2>/dev/null)" || status=""
# latest_at is RFC 3339; the zero time means "no snapshot".
latest="$(printf '%s' "$status" | sed -n 's/.*"latest_at":"\([^"]*\)".*/\1/p')"
ts=""
ok=0
if [ -n "$latest" ] && t="$(date -d "$latest" +%s 2>/dev/null)" && [ "$t" -gt 0 ]; then
  ts="$t"
  ok=1
elif [ -r "$out" ]; then
  ts="$(sed -n 's/^can_backup_last_success_timestamp_seconds \([0-9][0-9]*\)$/\1/p' "$out" | head -n 1)"
fi
tmp="$(mktemp "${out}.XXXXXX")"
trap 'rm -f "$tmp"' EXIT
{
  if [ -n "$ts" ]; then
    echo "# HELP can_backup_last_success_timestamp_seconds Unix time of the newest backup snapshot (last good value is kept)."
    echo "# TYPE can_backup_last_success_timestamp_seconds gauge"
    echo "can_backup_last_success_timestamp_seconds $ts"
  fi
  echo "# HELP can_backup_last_run_ok 1 when the last status check read a snapshot, 0 when it could not."
  echo "# TYPE can_backup_last_run_ok gauge"
  echo "can_backup_last_run_ok $ok"
} >"$tmp"
chmod 0644 "$tmp"
mv "$tmp" "$out"
trap - EXIT
