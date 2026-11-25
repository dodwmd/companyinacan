#!/usr/bin/env bash
# Print a footprint report (from measure.sh) as a Markdown table, with the run's metadata.
# Usage: summary.sh OUTDIR   (reads OUTDIR/report.json and OUTDIR/meta.txt; needs jq)
set -euo pipefail

dir=${1:?usage: summary.sh OUTDIR}
echo "### $(jq -r .label "$dir/report.json")"
echo
echo '```'
cat "$dir/meta.txt"
echo '```'
echo
echo "| Measure (MiB, median / max over $(jq -r .samples "$dir/report.json") samples) | median | max |"
echo "|---|---|---|"
jq -r '
  [["whole node container (working set)", .node],
   ["pods, sum of working sets", .pods_total],
   ["node minus pods (k3s, containerd, shims, kernel)", .system],
   ["k3s server process RSS", .k3s_server_rss]]
  | .[] | select(.[1] != null) | "| \(.[0]) | \(.[1].median_mb) | \(.[1].max_mb) |"' "$dir/report.json"
echo
echo "| Pod | median | max |"
echo "|---|---|---|"
jq -r '.pods[] | "| \(.namespace)/\(.name) | \(.median_mb) | \(.max_mb) |"' "$dir/report.json"
