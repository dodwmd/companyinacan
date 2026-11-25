---

description: "Task list for Epic E02: Local dev cluster"
---

# Tasks: Local Dev Cluster (Epic E02)

**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/. Tests precede implementation in every story. `[x]` = done and verified offline; `[ ]` = needs a cluster.

## Phase 1: Planner and CLI (US2)

- [x] T001 [US2] Tests for plans, Manager.Up/Down/Status with a fake Executor in `core/cluster/cluster_test.go`; prerequisite and hostname tests in `prereq_test.go`, `hosts_test.go`
- [x] T002 [US2] Implement `core/cluster` (`cluster.go`, `exec.go`, `prereq.go`, `hosts.go`)
- [x] T003 [US2] CLI tests in `core/cli/dev_test.go` (exit codes 0/1/2, dry-run, missing prerequisites, repo-root check, status JSON)
- [x] T004 [US2] Implement `can dev up|down|status|doctor` in `core/cli/dev.go`; replace the stub in `root.go`; adjust the stub tests
- [x] T005 [US2] Makefile targets `dev`, `down`, `doctor`; `test/contract/make_test.go` (`make -n`)

## Phase 2: Manifests (US1)

- [x] T006 [US1] Manifest contract tests in `test/contract/manifests_test.go` (see contracts/manifests.md)
- [x] T007 [US1] `deploy/base/*` (namespaces, quotas, networkpolicies, rbac, traefik-config, coredns-custom, kustomization)
- [x] T008 [US1] `deploy/templates/module-namespace.yaml`
- [x] T009 [US1] `dev/k3d.yaml`

## Phase 3: Cluster verification and docs

- [x] T010 Integration tests under `//go:build integration` in `test/integration/cluster_test.go`
- [x] T011 Non-required workflow `.github/workflows/dev-cluster.yml` (unverified)
- [x] T012 `docs/dev-cluster.md`; update README, `deploy/README.md`, `dev/README.md`, 001 contract note, epics table
- [ ] T013 Run `make dev` and `go test -tags integration ./test/integration/...` on a machine with docker, k3d, kubectl; fix findings (image tag, Traefik values, CoreDNS regex, policy enforcement)
- [ ] T014 Confirm the `dev-cluster` workflow passes on GitHub; then consider making it required
