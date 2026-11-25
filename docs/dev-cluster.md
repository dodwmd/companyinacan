# Local dev cluster (`make dev`)

A single-node k3s cluster in Docker (via [k3d](https://k3d.io)) with the same base manifests
production uses. Status: the planner, CLI and manifests are tested offline; behaviour on a real
cluster is **not yet verified** (see "Verification status").

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
- `deploy/templates/module-namespace.yaml`: what can-core (E03) instantiates per module.

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

Written in an environment without Docker, k3d or kubectl, so nothing below has been run:
the `rancher/k3s` tag and the k3d `v1alpha5` schema; the Traefik chart value names; the CoreDNS
rewrite syntax and whether the restart is needed; policy enforcement (including egress to the
API server on 6443); the integration tests and the workflow. The first real run is the
verification (task T013 in `specs/004-local-dev-cluster/tasks.md`).

## Troubleshooting

- `missing k3d`: install from https://k3d.io. `Cannot connect to the Docker daemon`: start Docker.
- Port 80 or 443 busy: stop the other listener or edit `ports` in `dev/k3d.yaml`.
- Start over: `make down && make dev`.
