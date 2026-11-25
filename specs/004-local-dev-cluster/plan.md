# Implementation Plan: Local Dev Cluster (Epic E02)

**Branch**: `epic/E02-dev-cluster` | **Date**: 2026-11-25 | **Spec**: [spec.md](spec.md)

## Summary

Plain Kubernetes manifests in `deploy/base/` (kustomization), a k3d config in `dev/k3d.yaml`, a
`core/cluster` package that plans (pure) and runs (via an Executor) the k3d/kubectl commands, and
`can dev up|down|status|doctor` plus `make dev|down|doctor`. Everything is verified offline by Go
tests; cluster-backed assertions are written under the `integration` build tag and are unrun.

## Technical Context

**Language/Version**: Go 1.27. **Dependencies**: none new (cobra, `go.yaml.in/yaml/v3` already present).
**External tools (runtime only)**: docker or podman, k3d v5.x, kubectl (built-in kustomize via `apply -k`).
**Testing**: `go test` (manifest contract tests in `test/contract`, planner and CLI unit tests); `-tags integration` for the cluster.
**Constraints**: no network or cluster in `make test`; no system software installed by the repo.

## Constitution Check

| Principle | Status | Notes |
|---|---|---|
| I. Spec-first | PASS | Traces to SPEC.md §3.1, §3.6; conventions it introduces (ports, labels, Service names) are recorded in spec Assumptions |
| II. Footprint | PASS | metrics-server disabled; quotas and LimitRange defaults cap every namespace; Traefik requests 48Mi; no new resident components |
| III. Test-first | PASS | Unit and contract tests written before implementation; cluster tests written without a cluster and later confirmed by the `dev-cluster` CI job |
| IV. Module contracts | PASS | Module namespace template fixes the label, egress and quota contract can-core instantiates |
| V. Secure/private | PARTIAL | Default-deny everywhere, secrets encryption on, no secrets in manifests, metadata endpoint/node/cluster CIDRs denied to modules, can-core standing RBAC reduced (no cluster-wide secrets/workloads/exec). Gaps until E03: can-core can RoleBind `can-core-module` into any namespace (escalation to cluster-admin-equivalent), create namespaces with relaxed PSA labels, and rewrite DNS/Traefik values in kube-system; module namespaces can reach every database in can-data (DB auth is the only barrier) |
| VI. OSS hygiene | PASS | No new dependencies; workflow actions pinned by SHA |
| VII. Simplicity/measured | PASS | Plain YAML + kubectl `-k`, no Helm/Terraform; remaining unverified claims are listed in research.md |

## Project Structure

```text
deploy/base/            kustomization, namespaces, quotas, networkpolicies, rbac, traefik-config, coredns-custom
deploy/templates/       module-namespace.yaml (instantiated by can-core, E03)
dev/k3d.yaml            k3d Simple config
core/cluster/           Config, plans, Manager, Executor, ExecExecutor, Prerequisites, Hostname
core/cli/dev.go         can dev up|down|status|doctor
test/contract/manifests_test.go   offline manifest invariants
test/integration/       //go:build integration, needs a cluster
.github/workflows/dev-cluster.yml non-required, unverified
docs/dev-cluster.md
```

## Complexity Tracking

None.
