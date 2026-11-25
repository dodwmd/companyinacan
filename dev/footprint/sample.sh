#!/usr/bin/env bash
# Take idle-memory samples from a k3d node (epic E00). Writes OUTDIR/001.txt, 002.txt, ... in the
# format documented in core/footprint, plus OUTDIR/pods.json (kubectl get pods -A -o json).
#
# Usage: sample.sh CLUSTER OUTDIR [COUNT=12] [INTERVAL_SECONDS=10]
#
# Memory comes from the node container's cgroup v2 files (metrics-server is disabled in this
# project, so `kubectl top` is not available): the container root for the whole node and one
# pod-level cgroup per pod. Everything is read inside the node with the node's own busybox.
set -euo pipefail

cluster=${1:?usage: sample.sh CLUSTER OUTDIR [COUNT] [INTERVAL]}
outdir=${2:?usage: sample.sh CLUSTER OUTDIR [COUNT] [INTERVAL]}
count=${3:-12}
interval=${4:-10}
node="k3d-${cluster}-server-0"

# Runs inside the node (busybox sh), one docker exec per sample.
# shellcheck disable=SC2016  # the variables are meant to expand inside the node, not here
probe='
r=/sys/fs/cgroup
[ -r "$r/memory.current" ] || { echo "no cgroup v2 memory.current at $r" >&2; exit 3; }
inact() { awk "\$1==\"inactive_file\"{print \$2}" "$1/memory.stat"; }
echo "NODE $(cat $r/memory.current) $(inact $r)"
find "$r" -maxdepth 4 -type d \( -name "pod*" -o -name "*-pod*.slice" \) | while read -r d; do
  [ -r "$d/memory.current" ] && echo "CG ${d#$r} $(cat "$d/memory.current") $(inact "$d")"
done
for p in /proc/[0-9]*; do
  c=$(tr "\0" " " < "$p/cmdline" 2>/dev/null) || continue
  case "$c" in
    */k3s\ server*|k3s\ server*) echo "PROC k3s-server $(awk "/^VmRSS:/{print \$2}" "$p/status")"; break ;;
  esac
done
'

mkdir -p "$outdir"
for i in $(seq 1 "$count"); do
  f=$(printf '%s/%03d.txt' "$outdir" "$i")
  docker exec "$node" sh -c "$probe" >"$f"
  if [ "$i" -lt "$count" ]; then sleep "$interval"; fi
done
kubectl --context "k3d-${cluster}" get pods -A -o json >"$outdir/pods.json"
echo "sampled $count x ${interval}s from $node into $outdir" >&2
