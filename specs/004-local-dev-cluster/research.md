# Research: Local Dev Cluster (E02)

Nothing here was checked against a running cluster. Items marked UNVERIFIED must be confirmed by the first real `make dev`.

## R1. Apply mechanism
- **Decision**: `kubectl apply -k deploy/base` (kustomize is built into kubectl).
- **Rationale**: no extra binary; plain YAML stays testable by parsing it.
- **Alternatives**: Helm chart (needless templating for static config), Flux/Argo (footprint).

## R2. Cluster tool
- **Decision**: k3d with a `k3d.io/v1alpha5` `Simple` config file; image `rancher/k3s:v1.31.5-k3s1`.
- **UNVERIFIED**: that this exact tag exists and that the `v1alpha5` field names match the installed k3d. Production k3s (E06) must use the same minor.
- k3d's own loadbalancer maps host 80/443; ServiceLB inside k3s is kept so Traefik's Service works as in production.

## R3. Traefik configuration
- **Decision**: `HelmChartConfig` named `traefik` in `kube-system` with `persistence` (PVC mounted at `/data`), ACME resolver `letsencrypt` storing `/data/acme.json`, `service.spec.externalTrafficPolicy: Local`, `fsGroup` 65532.
- **UNVERIFIED**: value names against the Traefik chart version bundled with k3s v1.31 (persistence/podSecurityContext/certificatesResolvers keys). Dev never requests certificates.

## R4. CoreDNS
- **Decision**: ConfigMap `coredns-custom` with a `*.override` key (k3s imports `*.override` into the default server block). Rules in order: `admin.localtest.me` exact no-op with `stop`; `can-api.localtest.me` to the module API Service; regex `(.*)\.localtest\.me` to `traefik.kube-system.svc.cluster.local` with `answer auto`.
- **UNVERIFIED**: regex semantics (trailing dot), `stop` handling, and that CoreDNS needs the `rollout restart` the planner performs (it may reload on its own).

## R5. NetworkPolicy enforcement
- **Decision**: k3s's embedded kube-router controller; nothing to install.
- **UNVERIFIED** (also an E00 Phase 0 item): enforcement of egress denial to the node IP; that the API server is reachable via 6443 after Service DNAT.

## R6. RBAC
- **Decision**: ClusterRole without wildcards or escalation verbs; kube-system limited to two `resourceNames`. Secrets and pods/exec are cluster-wide because can-core reads module Secrets and runs hooks (SPEC §3.1 accepts this).

## R7. Executor and planning
- **Decision**: `Cmd{Name,Args}`; plans are values; `Manager` runs a plan sequentially and stops on the first error. `Status` treats a failing `k3d cluster list <name>` as absent.
- **Alternatives**: calling the k3d Go library (large dependency, version coupling).

## R8. Prerequisites
- **Decision**: reuse `core/doctor.Check` and filter to docker/podman, k3d, kubectl. A running Docker daemon is not probed (would need to exec); `k3d cluster create` reports it.
