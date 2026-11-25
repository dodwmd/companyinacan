# Data Model: Phase 0 Measurements (E00)

## Raw sample (text, one file per sample, `dev/footprint/sample.sh`)

```
NODE <memory.current bytes> <inactive_file bytes>        node container root cgroup
CG <cgroup path> <memory.current> <inactive_file>        one per pod-level cgroup
PROC k3s-server <VmRSS kB>                               optional
```

A pod-level cgroup is a directory named `pod<uid>` (cgroupfs driver) or `...-pod<uid_>.slice`
(systemd driver); the UID maps to a pod through `kubectl get pods -A -o json`. Any other line, a
negative or non-numeric value, or a missing `NODE` line is a parse error.

## Report (`can footprint report`, JSON)

| Field | Meaning |
|---|---|
| `label`, `source`, `samples` | free text; `cgroup` or `kubectl-top`; window size |
| `node` | `{median_mb, max_mb}` working set of the whole node container |
| `pods_total` | same, summed over all pod cgroups per sample |
| `system` | node minus pods per sample: k3s server, containerd, shims, kernel memory |
| `k3s_server_rss` | RSS of the `k3s server` process |
| `pods[]` | `{namespace, name, median_mb, max_mb, samples}` |
| `namespaces[]` | sum of pod medians per namespace |
| `unmapped_cgroups` | pod cgroups with no matching pod in `pods.json` (count over all samples) |

All figures are MiB rounded to 0.01 ("MB" in `ram_mb` means MiB here).

## Check (`can footprint check`)

Inputs: report, namespace, optional name prefix, declared MB (`--ram-mb` or `--module`
`resources.ram_mb`), tolerance (default 0.20). Output: `{pass, declared_ram_mb, tolerance,
limit_mb, actual_mb, pods, message}`. `actual_mb` is the sum of pod medians in the selection.
Exit 1 when over or when nothing matched.

## Cluster variants (`dev/footprint/k3d-*.yaml`)

| Variant | k3s flags beyond the pinned image `rancher/k3s:v1.31.5-k3s1` |
|---|---|
| default | none (Traefik, CoreDNS, local-path, ServiceLB, metrics-server) |
| trimmed | `--disable=metrics-server --secrets-encryption` (what `make dev` uses) |
| lean | trimmed + `--disable=servicelb --disable-cloud-controller` |
