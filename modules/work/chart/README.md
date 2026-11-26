# work chart

Helm-compatible templates for the in-house work tracker, rendered by can-core's own sandboxed
renderer (not Helm; the supported subset is in `core/server/installer/chart.go`) and applied by its
installer. The other in-house module charts (forms, people, jobs, finance) use the same templates.

| Object | Notes |
|---|---|
| Deployment `work` | one replica, `Recreate` (SQLite on a ReadWriteOnce volume), non-root uid 65532, read-only root filesystem, all capabilities dropped, seccomp RuntimeDefault; memory request and limit equal the manifest's `ram_mb` and `ram_limit_mb`; readiness `GET /healthz`; liveness `/can-work healthcheck` (the image has no shell or curl) |
| Service `work` | ClusterIP, port 8080 (`work`, `projects`) |
| PersistentVolumeClaim `work-data` | `helm.sh/resource-policy: keep`: disabling the module keeps the data |
| ConfigMap `work-config` | `work.yaml` built from the `config:` block of `values.yaml` only (the binary loads it strictly); no secrets, no webhook URL (events go to can-core's module API) |
| NetworkPolicy `allow-intra-namespace` | off by default (single pod); same-namespace pods only when enabled |
| IngressRoute | only when `ingress.enabled` and can-core supplies `ingress.host`, which it does only for a manifest with `runtime.subdomain`; work has none, so the chart routes nothing (can-core routes the web UI with `expose.ui`, docs/module-routing.md) |

Secrets are not chart values. can-core writes them once into the `can-module` Secret and the installer
injects that Secret as environment into every container, with `CAN_API_URL`, `CAN_MODULE_ID`,
`CAN_WEBHOOK_SECRET`, `CAN_TOKEN_FILE` and `CAN_TOKEN_AUDIENCE`. The chart sets only `CAN_WORK_CONFIG`.
Settings from `company.yaml` are laid over `config:`
([docs/module-wiring.md](../../../docs/module-wiring.md#settings)).

## What is verified

- Unit tests (`core/server/installer/modulecharts_test.go`, run by `make test`): the chart installs
  through the installer against the fake Kubernetes API, the objects above have the shape listed,
  and the rendered config is accepted by the module's own loader.
- `make release-check` fails while `image.digest` holds its `REPLACE_WITH_` placeholder; a
  development install uses the plain tag.
- On a real cluster: the advisory dev-cluster workflow (not a required check) builds the can-work
  image, installs work through can-core on k3d, creates an issue with a personal access token and
  disables the module. Read the workflow run for what actually ran.
