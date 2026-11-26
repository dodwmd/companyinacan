# Contract: module charts

What the starter charts assume of can-core (E03) and promise to the namespace.

## From can-core

- Release name any; release namespace `mod-<id>` created from `deploy/templates/module-namespace.yaml`
  (pod security `restricted`, default-deny, `module-egress`, LimitRange, quota).
- `--set ingress.host=<subdomain>.<domain>` (required); `ingress.tls=false` on local dev.
- Secrets: `can-module` (`CAN_API_URL`, `CAN_WEBHOOK_SECRET`, generated passwords), `can-db` for a Postgres
  module (`DB_HOST DB_PORT DB_NAME DB_USER DB_PASSWORD`) and `can-smtp` for apps given the relay
  (specs/041; `core/server/installer/secretrefs_test.go` checks every reference). The older names in
  [data-model.md](../data-model.md) are superseded. `can-smtp` references are optional.
- A projected-token audience `can` is minted by the kubelet for the adapter only; the API token of
  the default ServiceAccount is never mounted (`automountServiceAccountToken: false`).

## To the namespace

- Only Deployments, Services, IngressRoutes, PersistentVolumeClaims and NetworkPolicies, all
  namespaced; no RBAC, no CRDs, no cluster-scoped objects.
- Every container: `runAsNonRoot`, `allowPrivilegeEscalation: false`, `capabilities.drop: [ALL]`,
  pod `seccompProfile: RuntimeDefault`, cpu and memory requests, memory limit, readiness and
  liveness probes (support also a startup probe). FreeScout's root filesystem is writable; the
  others are read-only with `/tmp` as an emptyDir.
- Single replica, `strategy: Recreate` (ReadWriteOnce volumes, SQLite single writer).
- PVC `<id>-data` is annotated `helm.sh/resource-policy: keep`: disable and uninstall keep data;
  purge is explicit.
- Service names: app `<id>` (manifest `runtime.ui`), adapter `<id>-adapter` on 8081 (manifest
  `provides`), ClusterIP.
- Public routes: `Host(<host>)` to the app; with the adapter, `Host(<host>) && PathPrefix(/inbound)`
  at priority 100 to the adapter (support only). TLS uses certificate resolver `letsencrypt`.
- NetworkPolicy: none from the chart, except `allow-intra-namespace` (Ingress and Egress, this
  release's pods only, no ipBlock, no namespaceSelector) when the adapter is enabled.

## Footprint rule

Sum of container memory requests (app plus adapter) is at most the manifest's `ram_mb`; sum of
limits at most `ram_limit_mb`. Init containers run before the app and are excluded.
