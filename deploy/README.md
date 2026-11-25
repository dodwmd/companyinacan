# deploy

Cluster bootstrap assets: cloud-init, k3s configuration, base manifests, NetworkPolicies, Traefik config.
`base/` (kustomize: namespaces, NetworkPolicies, quotas, RBAC, Traefik, CoreDNS) and `templates/` are from epic E02; see `docs/dev-cluster.md`. Bootstrap assets are added by E06.

can-core environment and the `CAN_TRUST_PROXY` setting behind Traefik: `specs/005-can-core-mvp/deployment.md`.
