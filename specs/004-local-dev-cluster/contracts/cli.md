# Contract: `can dev` and make targets (E02)

Supersedes the `can dev` stub line in `specs/001-project-foundation/contracts/cli.md`. Global flags and exit codes (`0` ok, `1` failure, `2` usage, `3` not implemented) are unchanged.

Persistent flags on `can dev`: `--name` (default `companyinacan-dev`), `--k3d-config` (`dev/k3d.yaml`), `--manifests` (`deploy/base`). Paths are relative to the working directory; run from the repository root.

| Command | Behaviour | Exit |
|---|---|---|
| `can dev` | prints `can dev needs a subcommand: ...` to stderr | 2 |
| `can dev up [--dry-run]` | `--dry-run`: print the create plan, run nothing, no prerequisite check. Otherwise: check docker/podman, k3d, kubectl; check the config and manifest paths exist; create the cluster if absent, else only re-apply; print the context and `admin.localtest.me` URL | 0; 1 on missing prerequisite/path or a failing command (stderr included); 2 on invalid `--name` |
| `can dev down [--dry-run]` | `k3d cluster delete <name>`; if the cluster does not exist this is a successful no-op (a status check runs first) | 0; 1 on missing prerequisite or failure; 2 on invalid name |
| `can dev status` | text `<name>: running|stopped|absent`, or JSON `{"name","exists","running","servers","serversRunning"}` | 0 only if running, else 1 |
| `can dev doctor` | lists missing dev prerequisites (JSON: array of doctor results); `ok: ...` when none | 1 if any missing, else 0 |

## Command plan for `up` (exact)
```
k3d cluster create <name> --config dev/k3d.yaml
kubectl --context k3d-<name> wait --for=condition=Ready node --all --timeout=180s
kubectl --context k3d-<name> get crd/helmchartconfigs.helm.cattle.io          # retried up to 36 times, 5 s apart
kubectl --context k3d-<name> wait --for=condition=Established crd/helmchartconfigs.helm.cattle.io --timeout=180s
kubectl --context k3d-<name> apply -k deploy/base
kubectl --context k3d-<name> -n kube-system rollout restart deployment/coredns
kubectl --context k3d-<name> -n kube-system rollout status deployment/coredns --timeout=120s
```
The existence check is `k3d cluster list <name> --output json`: only "No nodes found"/"not found" means absent; any other failure (Docker down, k3d broken) aborts with exit 1. A running cluster only runs the last five lines (re-apply). A stopped one first runs `k3d cluster start <name>` and the node wait, then the same five lines.

## Make targets
| Target | Runs |
|---|---|
| `make dev` | `can dev up` |
| `make down` | `can dev down` |
| `make doctor` | `can dev doctor` |
| `make up` | still a stub (epic E06), exit 3 |
