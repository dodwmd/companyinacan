# Contract: base manifests (E02)

Enforced offline by `test/contract/manifests_test.go`.

1. `deploy/base/kustomization.yaml` lists every `*.yaml` in `deploy/base/`.
2. Namespaces `can-system`, `can-data`, `can-portal`, each with `pod-security.kubernetes.io/enforce`; `can-portal` is `restricted`, `can-system` is not; `kube-system` is never declared.
3. Each platform namespace has a NetworkPolicy `default-deny` (empty `podSelector`, `Ingress`+`Egress`, no rules) and `allow-dns` (to `kube-dns` in `kube-system`, port 53).
4. Named allows: `allow-ingress-from-traefik` (can-system, can-portal); `allow-module-api-from-modules` (can-system, can-core pods, from namespaces of kind module/portal); `allow-can-core-egress`; `allow-db-from-modules-and-core` (can-data, 5432/3306); portal egress to the module API.
5. No NetworkPolicy contains `0.0.0.0/0` except `allow-can-core-egress` (ports 6443/443 only).
6. Each platform namespace has a ResourceQuota and a LimitRange with default requests and limits. Base declares no workloads; any that appear must pin digests and set requests, limits and a securityContext.
7. `HelmChartConfig/traefik` (kube-system, `helm.cattle.io/v1`): `persistence.enabled`, ACME storage under `persistence.path`, `service.spec.externalTrafficPolicy: Local`.
8. `ConfigMap/coredns-custom` (kube-system): keys end in `.override`/`.server`; `admin.localtest.me` pinned before the wildcard `localtest\.me` rewrite to `traefik.kube-system.svc.cluster.local` with `answer auto`; alias to `can-core-api.can-system.svc.cluster.local`.
9. RBAC: `ServiceAccount can-system/can-core`; ClusterRole covering namespaces, networkpolicies, tokenreviews; no `*` anywhere, no `escalate`/`bind`/`impersonate`, nothing bound to `cluster-admin`, every binding subject is can-core; kube-system Role restricted by `resourceNames`.
10. `deploy/templates/module-namespace.yaml`: Namespace labelled `can.dev/namespace-kind: module` with PSA enforce; `default-deny`; `module-egress` (DNS, can-core API, Traefik, can-data, internet except `${NODE_IP}/32`, `10.42.0.0/16`, `10.43.0.0/16`, `169.254.0.0/16`); LimitRange; ResourceQuota.
11. `dev/k3d.yaml`: `k3d.io/v1alpha5` Simple, name `companyinacan-dev`, pinned `rancher/k3s:v...` image, 1 server 0 agents, ports 80/443 on the loadbalancer, `--disable=metrics-server`, `--secrets-encryption`, Traefik not disabled, `wait: true`.
