# Local dev cluster (`make dev`)

A single-node k3s cluster in Docker (via [k3d](https://k3d.io)) with the same base manifests
production uses. Status: the planner, CLI and manifests are tested offline, and the GitHub
`dev-cluster` job has created the cluster and passed the integration tests (see "Verification
status").

## Prerequisites

Docker or Podman, `k3d` v5.x and `kubectl`. Check with:

```sh
make doctor        # can dev doctor: exit 1 and a remedy per missing tool
```

## Commands

Run from the repository root.

```sh
make dev           # can dev up: create the cluster if absent, apply deploy/base, reload CoreDNS
make down          # can dev down: delete the cluster
bin/can dev status # exit 0 only if running; --output json available
bin/can dev up --dry-run   # print the exact k3d/kubectl commands without running them
```

Flags: `--name` (default `companyinacan-dev`), `--k3d-config`, `--manifests`. Exit codes: 0 ok,
1 failure, 2 usage error (for example a bad `--name`). The kubectl context is
`k3d-<name>`; the current context is left unchanged.

## What gets created

- `dev/k3d.yaml`: one server, ports 80 and 443 on the k3d load balancer, Traefik kept,
  metrics-server disabled, secrets encryption on, k3s pinned to `rancher/k3s:v1.31.5-k3s1`.
  To bump k3s change the tag there; production (E06) must use the same minor version.
- `deploy/base/` (applied with `kubectl apply -k`): namespaces `can-system`, `can-data`,
  `can-portal`; default-deny NetworkPolicies plus explicit allows; ResourceQuota and LimitRange
  per namespace; can-core RBAC; Traefik `HelmChartConfig` (persistent `acme.json`,
  `externalTrafficPolicy: Local`); `coredns-custom`.
- `deploy/templates/module-namespace.yaml` and `module-rbac.yaml`: what can-core (E03) instantiates per module (namespace, default-deny, ingress/egress allows, quota, and the RoleBinding that gives can-core powers in that namespace only). can-core's cluster-wide role is deliberately tiny; see `deploy/base/rbac.yaml`.

Contracts and the invariants the tests enforce: `specs/004-local-dev-cluster/contracts/`.

## Hostnames

`*.localtest.me` resolves to 127.0.0.1 on your machine, so `http://admin.localtest.me` reaches
Traefik through the mapped port 80. Inside the cluster 127.0.0.1 would be the pod itself, so
CoreDNS rewrites `*.localtest.me` to the Traefik Service. `admin.localtest.me` is deliberately
not rewritten: modules must not reach the admin router. `can-api.localtest.me` aliases the module
API Service. The Go helper is `cluster.Hostname("help")` giving `help.localtest.me`.

Local dev uses plain HTTP; the Traefik ACME resolver exists but is never triggered locally.

## Tests

```sh
make test                                                  # offline: planner, CLI, manifest invariants
make dev && go test -tags integration -count=1 ./test/integration/...   # needs a cluster
```

The integration tests check namespaces and default-deny, the Traefik PVC and traffic policy,
`*.localtest.me` resolution from a pod (and that `admin.` is not rewritten), and that
default-deny blocks pod-to-pod traffic. The non-required GitHub workflow `dev-cluster` runs them
with k3d.

## Verification status

Written without local Docker, k3d or kubectl. Evidence since: the non-required `dev-cluster`
workflow (k3d v5.8.3 on a GitHub runner) ran `make dev` and the integration tests, and they
passed: namespaces and default-deny, the Traefik PVC and `externalTrafficPolicy: Local`,
`*.localtest.me` resolving to Traefik from a pod, and default-deny blocking pod traffic.

Still unverified: certificate issuance (ACME) and `acme.json` persistence; the effect of
`externalTrafficPolicy: Local` on client IPs; can-core's RBAC (there is no can-core yet) and the
module-namespace and RBAC templates; whether the CoreDNS restart is needed; egress denial to the
node IP; the later-added positive-control and cross-namespace default-deny probes and the anchored
CoreDNS regex until CI runs them; runs on a developer machine or with Podman. The workflow is
`continue-on-error`, so read its log rather than the green tick.

Known limits (details in `specs/004-local-dev-cluster/research.md`): can-core's egress allows
`0.0.0.0/0` on ports 443 and 6443; a module pod can still send `Host: admin.<domain>` to Traefik,
so the admin router needs the pod-CIDR-rejecting middleware (E03/E06); `can dev down` on a
missing cluster does nothing and succeeds; `can dev up` on a stopped cluster starts it.

## Troubleshooting

- `missing k3d`: install from https://k3d.io. `Cannot connect to the Docker daemon`: start Docker.
- Port 80 or 443 busy: stop the other listener or edit `ports` in `dev/k3d.yaml`.
- Start over: `make down && make dev`.
