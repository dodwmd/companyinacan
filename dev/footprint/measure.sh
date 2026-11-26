#!/usr/bin/env bash
# Footprint harness (epic E00): create a k3d cluster, deploy something, wait for it to settle,
# sample idle RAM over a window and write a JSON report. Requires docker, k3d, kubectl and the
# `can` CLI (CAN env var, default `go run ./cmd/can`). Run in CI only so far: see
# .github/workflows/footprint.yml and docs/measurements/.
#
# Usage:
#   measure.sh --label NAME --k3d-config FILE [options]
#   measure.sh --label NAME --use-can-dev [options]      # `can dev up`: dev/k3d.yaml + deploy/base
#
# Options:
#   --name N          cluster name (default fp-<pid>; 32 chars max)
#   --manifest FILE   kubectl apply -f FILE after the cluster is up (repeatable)
#   --import IMAGE    k3d image import IMAGE before applying manifests (repeatable)
#   --settle SECS     idle wait after everything is Ready, before sampling (default 60)
#   --samples N       samples in the window (default 12)
#   --interval SECS   seconds between samples (default 10)
#   --out DIR         output directory (default footprint-out/<label>)
#   --keep            leave the cluster running
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
label="" cfg="" use_can_dev=0 name="fp-$$" settle=60 samples=12 interval=10 out="" keep=0
manifests=() images=()
while [ $# -gt 0 ]; do
  case $1 in
    --label) label=$2; shift 2 ;;
    --k3d-config) cfg=$2; shift 2 ;;
    --use-can-dev) use_can_dev=1; shift ;;
    --name) name=$2; shift 2 ;;
    --manifest) manifests+=("$2"); shift 2 ;;
    --import) images+=("$2"); shift 2 ;;
    --settle) settle=$2; shift 2 ;;
    --samples) samples=$2; shift 2 ;;
    --interval) interval=$2; shift 2 ;;
    --out) out=$2; shift 2 ;;
    --keep) keep=1; shift ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done
[ -n "$label" ] || { echo "--label is required" >&2; exit 2; }
[ -n "$cfg" ] || [ "$use_can_dev" = 1 ] || { echo "need --k3d-config or --use-can-dev" >&2; exit 2; }
out=${out:-footprint-out/$label}
mkdir -p "$out"
ctx="k3d-$name"
read -r -a can <<<"${CAN:-go run ./cmd/can}"

# Never delete a cluster that was there before this run. owned is set only once k3d has said that no
# cluster of this name exists, and only an owned cluster is ever deleted. If k3d cannot say, stop.
owned=0
cleanup() {
  if [ "$owned" = 1 ] && [ "$keep" = 0 ]; then k3d cluster delete "$name" >/dev/null 2>&1 || true; fi
}
trap cleanup EXIT

if ! clusters=$(k3d cluster list --no-headers); then
  echo "cannot list k3d clusters, so cannot tell whether $name already exists; nothing was done" >&2
  exit 1
fi
if awk -v n="$name" '$1 == n { found = 1 } END { exit !found }' <<<"$clusters"; then
  echo "k3d cluster $name already exists; delete it or choose another --name" >&2
  exit 1
fi
owned=1

if [ "$use_can_dev" = 1 ]; then
  "${can[@]}" dev up --name "$name"
else
  # k3d occasionally fails with "could not start a docker container" (runner/docker hiccup): clean up
  # the half-created cluster and retry a bounded number of times instead of failing the whole job.
  attempt=1
  until k3d cluster create "$name" --config "$cfg" --wait --timeout 180s; do
    [ "$attempt" -lt 3 ] || { echo "k3d cluster create failed $attempt times" >&2; exit 1; }
    echo "k3d cluster create failed (attempt $attempt), retrying" >&2
    k3d cluster delete "$name" >/dev/null 2>&1 || true
    sleep $((attempt * 5))
    attempt=$((attempt + 1))
  done
  kubectl --context "$ctx" wait --for=condition=Ready node --all --timeout=180s
fi

for img in ${images[@]+"${images[@]}"}; do k3d image import "$img" -c "$name"; done
for m in ${manifests[@]+"${manifests[@]}"}; do kubectl --context "$ctx" apply -f "$m"; done

# Wait until every pod is Running/Completed and every Deployment is Available.
deadline=$((SECONDS + 300))
while :; do
  pending=$(kubectl --context "$ctx" get pods -A --no-headers 2>/dev/null |
    awk '$4 != "Running" && $4 != "Completed" {print $1"/"$2" "$4}')
  if [ -z "$pending" ] && kubectl --context "$ctx" wait --for=condition=Available deployment --all -A --timeout=60s >/dev/null 2>&1; then
    break
  fi
  if [ "$SECONDS" -ge "$deadline" ]; then
    echo "pods not ready after 300s:" >&2
    echo "$pending" >&2
    kubectl --context "$ctx" get pods -A >&2 || true
    exit 1
  fi
  sleep 5
done
echo "ready; idling ${settle}s before sampling" >&2
sleep "$settle"

{
  echo "date_utc=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "label=$label"
  echo "k3s=$(kubectl --context "$ctx" get node -o jsonpath='{.items[0].status.nodeInfo.kubeletVersion}')"
  echo "runner_vcpus=$(nproc)"
  echo "runner_mem_total_mib=$(awk '/MemTotal/{printf "%d", $2/1024}' /proc/meminfo)"
  echo "kernel=$(uname -r)"
  echo "settle_s=$settle samples=$samples interval_s=$interval"
} >"$out/meta.txt"

"$here/sample.sh" "$name" "$out/samples" "$samples" "$interval"
mv "$out/samples/pods.json" "$out/pods.json"
"${can[@]}" footprint report --pods "$out/pods.json" --samples "$out/samples" --label "$label" >"$out/report.json"
echo "report: $out/report.json" >&2
cat "$out/report.json"
