# Research: Local Dev Cluster (E02)

Written without a cluster. Afterwards the non-required `dev-cluster` GitHub job ran `make dev` on k3d v5.8.3 and all of `test/integration` passed, which confirms the k3s tag, the k3d config, the Traefik PVC and `externalTrafficPolicy` values, the CoreDNS rewrite (before the regex was anchored in a follow-up: re-confirmed by the next CI run) and kube-router default-deny. Items still marked UNVERIFIED have not been exercised.

## R1. Apply mechanism
- **Decision**: `kubectl apply -k deploy/base` (kustomize is built into kubectl).
- **Rationale**: no extra binary; plain YAML stays testable by parsing it.
- **Alternatives**: Helm chart (needless templating for static config), Flux/Argo (footprint).

## R2. Cluster tool
- **Decision**: k3d with a `k3d.io/v1alpha5` `Simple` config file; image `rancher/k3s:v1.31.5-k3s1`.
- Confirmed by CI: the tag exists and the `v1alpha5` config is accepted by k3d v5.8.3. Production k3s (E06) must use the same minor.
- k3d's own loadbalancer maps host 80/443; ServiceLB inside k3s is kept so Traefik's Service works as in production.

## R3. Traefik configuration
- **Decision**: `HelmChartConfig` named `traefik` in `kube-system` with `persistence` (PVC mounted at `/data`), ACME resolver `letsencrypt` storing `/data/acme.json`, `service.spec.externalTrafficPolicy: Local`, `fsGroup` 65532.
- Confirmed by CI: a Traefik PVC exists and the Service has `externalTrafficPolicy: Local`. **UNVERIFIED**: ACME issuance and that `acme.json` is actually written to the volume. Dev never requests certificates. Caveats for E06 (also in the manifest): the RWO PVC may deadlock a RollingUpdate upgrade (switch to Recreate if so); the resolver has no `email`/`caServer`, which the bootstrap must add.

## R4. CoreDNS
- **Decision**: ConfigMap `coredns-custom` with a `*.override` key (k3s imports `*.override` into the default server block). Rules in order: `admin.localtest.me` exact no-op with `stop`; `can-api.localtest.me` to the module API Service; regex `(.*)\.localtest\.me` to `traefik.kube-system.svc.cluster.local` with `answer auto`.
- Rule order and the `admin.` pin are confirmed by CI (`foo.localtest.me` resolves to the Traefik ClusterIP, `admin.localtest.me` to 127.0.0.1). The regex is anchored as `^(.*)\.localtest\.me\.$` (CoreDNS matches the fully-qualified name). **UNVERIFIED**: that the `rollout restart` is needed (CoreDNS may reload on its own).
- **Residual risk, Host header (E06)**: the pin only stops DNS resolution. A module pod may still open a connection to Traefik (it has an egress allow for public-hostname access) and send `Host: admin.<domain>`. The real control is the admin router middleware rejecting pod-CIDR sources (SPEC 3.1), which E03/E06 must implement and test.

## R5. NetworkPolicy enforcement
- **Decision**: k3s's embedded kube-router controller; nothing to install.
- Default-deny of pod-to-pod and cross-namespace traffic, with a positive control, is exercised by the integration tests (CI run proved the first version passes; the positive control and cross-namespace probe are new and unrun). **UNVERIFIED** (also an E00 Phase 0 item): egress denial to the node IP; that the API server is reachable via 6443 after Service DNAT; can-core egress `0.0.0.0/0` on 443/6443 is deliberately broad (node IP unknown at authoring time) and should be narrowed in E06.

## R6. RBAC
- **Decision**: split. ClusterRole `can-core` (cluster-wide) is only namespaces create/get/list/watch, TokenReviews, RoleBindings create and `bind` on `can-core-module`. Secrets, workloads, pods/exec, NetworkPolicies and quotas live in ClusterRole `can-core-module`, which is only ever bound per namespace with a RoleBinding (base for can-system/can-data, `deploy/templates/module-rbac.yaml` per module). kube-system limited to two `resourceNames`. An earlier draft granted all of it cluster-wide, which was effectively cluster-admin.
- **Open**: can-core cannot delete namespaces with this grant (deletion cannot be limited to `mod-*` by RBAC); E03 must decide between a ValidatingAdmissionPolicy-guarded `delete` and another cleanup path. Also E03: a ValidatingAdmissionPolicy denying RoleBindings outside `mod-*`/platform namespaces and relaxed pod-security labels.

## R7. Executor and planning
- **Decision**: `Cmd{Name,Args,Retries}`; plans are values; `Manager` runs a plan sequentially, retrying only commands that declare `Retries` (the CRD existence poll: `kubectl wait` fails at once on a missing object and `--for=create` needs kubectl 1.31), and stops on the first error. `Status` treats only a "No nodes found"/"not found" failure of `k3d cluster list <name>` as absent; other errors surface. `Up` starts a stopped cluster; `Down` of an absent cluster is a no-op.
- **Alternatives**: calling the k3d Go library (large dependency, version coupling).

## R8. Prerequisites
- **Decision**: reuse `core/doctor.Check` and filter to docker/podman, k3d, kubectl. A running Docker daemon is not probed (would need to exec); `k3d cluster create` reports it.
