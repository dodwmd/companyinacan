# Feature Specification: Local Dev Cluster (Epic E02)

**Feature Branch**: `epic/E02-dev-cluster`

**Created**: 2026-11-25

**Status**: Implemented offline; cluster behaviour unverified (no k3d in the authoring environment)

**Input**: Epic E02 in `docs/epics/README.md`: "`make dev` gives a working local k3s (k3d) company with the same charts as production." Scope: base manifests in `deploy/`, k3d cluster config, `can dev up|down|status|doctor`, `make dev|down|doctor`, `*.localtest.me` hostnames. SPEC.md §3.1, §3.6.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Base manifests encode the platform's network and security posture (Priority: P1)

A contributor reads and tests `deploy/base/` to see the namespaces, default-deny NetworkPolicies
with explicit allows, Traefik ACME persistence, the CoreDNS rewrite, can-core's RBAC and the
default quotas, without needing a cluster.

**Independent Test**: `go test ./test/contract/...` parses every manifest and asserts the
invariants (see contracts/manifests.md).

**Acceptance Scenarios**:

1. **Given** the base manifests, **When** parsed, **Then** `can-system`, `can-data` and `can-portal` exist with pod-security labels and each has a default-deny (ingress+egress) NetworkPolicy; `kube-system` is not denied.
2. **Given** the policies, **When** inspected, **Then** DNS, ingress from Traefik, the can-core module API (module namespaces only), databases and can-core egress are explicitly allowed, and no policy outside can-core's API-server egress opens `0.0.0.0/0`.
3. **Given** the Traefik `HelmChartConfig`, **Then** ACME storage lives under a persistent volume path and `externalTrafficPolicy` is `Local`.
4. **Given** `coredns-custom`, **Then** `*.localtest.me` rewrites to the Traefik Service, `admin.localtest.me` is pinned (never rewritten), and a module API alias exists.
5. **Given** the RBAC, **Then** can-core's ClusterRole has no wildcard and no `escalate`/`bind`/`impersonate`, nothing binds `cluster-admin`, and kube-system access is limited to two named objects.
6. **Given** every platform namespace and the module-namespace template, **Then** each has a ResourceQuota and a LimitRange with default requests and limits.

### User Story 2 - `make dev` creates and removes a local cluster (Priority: P1)

The operator runs `make doctor`, `make dev` and `make down`. `can dev up` creates the k3d cluster
when absent, applies the base manifests and reloads CoreDNS; re-running only re-applies.

**Independent Test**: unit tests drive the planner with a fake Executor and assert the exact
`k3d`/`kubectl` command lines; CLI tests assert exit codes with fake tool lookup. `--dry-run`
prints the plan without running anything.

**Acceptance Scenarios**:

1. **Given** no tools installed, **When** `can dev up --dry-run`, **Then** the exact commands print and exit is 0.
2. **Given** k3d/kubectl/docker missing, **When** `can dev up`, **Then** exit 1 naming each missing tool with its remedy and nothing is executed.
3. **Given** an existing cluster, **When** `can dev up`, **Then** no `k3d cluster create` runs; manifests are re-applied.
4. **Given** a failing command, **When** `can dev up`, **Then** the plan stops at it and exits 1 with its stderr.
5. **Given** a bad `--name`, **Then** exit 2.
6. **Given** `can dev status`, **Then** exit 0 only when the cluster is running, 1 otherwise (text or `--output json`).

### User Story 3 - Local hostnames (Priority: P2)

`cluster.Hostname("admin")` returns `admin.localtest.me`; invalid labels are rejected. Used by
`can dev up` to print the URLs.

## Requirements *(mandatory)*

- **FR-001**: `deploy/base/` MUST be a valid kustomization applying with `kubectl apply -k`.
- **FR-002**: Every platform namespace MUST have default-deny ingress+egress and explicit allows only.
- **FR-003**: The Traefik ACME store MUST be persistent; Traefik's Service MUST use `externalTrafficPolicy: Local`.
- **FR-004**: CoreDNS customisation MUST go through `coredns-custom` (`*.override`), never by editing `coredns`; `admin.*` MUST NOT be rewritten.
- **FR-005**: can-core RBAC MUST be least privilege (no wildcards, no escalation verbs, no cluster-admin).
- **FR-006**: Every namespace MUST have a ResourceQuota and LimitRange.
- **FR-007**: `dev/k3d.yaml` MUST pin the k3s image tag, map 80/443, keep Traefik, disable metrics-server and enable secrets encryption.
- **FR-008**: `can dev up|down|status|doctor` MUST follow the exit codes 0/1/2/3 (contracts/cli.md); `make dev|down|doctor` wrap them.
- **FR-009**: Commands MUST be produced by a pure planner and executed through an Executor interface.
- **FR-010**: Cluster-dependent tests MUST sit behind the `integration` build tag and never run in `make test`.

## Success Criteria

- **SC-001**: `make test` passes with no cluster, Docker or network (beyond the optional lint download).
- **SC-002**: On a machine with docker, k3d and kubectl, `make dev` yields a cluster on which `go test -tags integration ./test/integration/...` passes. **Not verified in this epic's authoring environment.**

## Assumptions

- Operators run `can` from the repository root (paths are relative).
- Pod and service naming conventions (labels `app.kubernetes.io/name`, ports 8080/8081/8090, Services `can-core-admin` and `can-core-api`) are fixed here for E03/E04 to satisfy.
- Production (E06) reuses `deploy/base` unchanged; only the bootstrap differs.
