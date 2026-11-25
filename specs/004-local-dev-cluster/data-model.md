# Data Model: Local Dev Cluster (E02)

## cluster.Config
| Field | Default | Rules |
|---|---|---|
| `Name` | `companyinacan-dev` | `^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`; kubectl context is `k3d-<Name>` |
| `K3dConfig` | `dev/k3d.yaml` | must exist for `up` (unless `--dry-run`) |
| `ManifestsDir` | `deploy/base` | must exist for `up` |

## cluster.Cmd / Plan
`Cmd{Name string; Args []string}`; `String()` is the space-joined line. A `Plan` is `[]Cmd`.
Plans: Create (k3d create, wait nodes, then Apply), Apply (wait for the HelmChartConfig CRD, `apply -k`, restart and await CoreDNS), Delete, Status.

## cluster.Status
`Exists`, `Running` (all servers running), `Servers`, `ServersRunning`.

## Namespaces
| Namespace | Label `can.dev/namespace-kind` | PSA enforce |
|---|---|---|
| `can-system` | system | baseline (hostPath exemption, SPEC §3.1) |
| `can-data` | data | baseline |
| `can-portal` | portal | restricted |
| `mod-<id>` (template) | module | restricted |

## Conventions fixed for later epics
Pod label `app.kubernetes.io/name`: `can-core`, `site`, `traefik` (kube-system). Ports: admin 8080, site 8081, module API 8090, portal 8080, databases 5432/3306. Services: `can-core-admin`, `can-core-api` in `can-system`.

## Template variables (`deploy/templates/module-namespace.yaml`)
`${MODULE_NAMESPACE}`, `${NODE_IP}`, `${QUOTA_REQUESTS_MEMORY}`, `${QUOTA_LIMITS_MEMORY}`.
