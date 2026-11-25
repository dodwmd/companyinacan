# Implementation Plan: Phase 0 Measurements, CI-runnable part (Epic E00)

**Branch**: `epic/E00-measurements` | **Date**: 2026-11-25 | **Spec**: [spec.md](spec.md)

## Summary

Shell scripts (`dev/footprint/`) create and sample a k3d node; a Go package `core/footprint`
interprets the samples and checks against `ram_mb`; `can footprint report|check` exposes it; a
non-required workflow `footprint.yml` runs four jobs (baseline x3 variants, platform base,
can-static + forms images, proofs); extra `integration`-tagged tests prove the module-namespace
and TokenReview design. Results go to `docs/measurements/phase0-ci.md`; SPEC.md changes only
where measured.

## Technical Context

**Language/Version**: Go 1.27, bash. **Dependencies**: none new. **External tools (CI only)**: docker, k3d v5.8.3 (checksum-pinned), kubectl, jq (preinstalled on the runner).
**Testing**: unit tests with canned kubectl JSON and canned cgroup samples; `shellcheck`; the workflow for everything cluster-backed.
**Constraints**: nothing in `make test` needs a cluster; the author's machine ran no docker/k3d.

## Constitution Check

| Principle | Status | Notes |
|---|---|---|
| I. Spec-first | PASS | Traces to epic E00, SPEC §3.2, §3.4, §3.1 |
| II. Footprint | PASS | This epic implements the CI footprint check (`can footprint check`, +20%); it adds no resident component |
| III. Test-first | PASS | Parser, aggregator and checker tests use canned input; the cluster-backed scripts and proofs can only be verified by CI and are marked so in the measurements doc |
| IV. Module contracts | PASS | Reads `resources.ram_mb` through the existing `config.DecodeModule`; no schema change |
| V. Secure/private | PASS | Workflow `contents: read`, actions pinned by SHA, k3d checksum-verified, no secrets; fixture tokens are throw-away values in a cluster that lives minutes. Proofs test the security posture and report failures as findings |
| VI. OSS hygiene | PASS | No new dependency; `upload-artifact` pinned by SHA |
| VII. Simplicity/measured | PASS | Plain bash plus one small Go package; every number in docs/measurements comes from a linked CI run; estimates stay labelled |

## Project Structure

```text
dev/footprint/          measure.sh, sample.sh, summary.sh, k3d-{default,trimmed,lean}.yaml, manifests/
core/footprint/         ParsePods, ParseRaw, Build, ParseTop, FromTop, Check
core/cli/footprint.go   can footprint report | check
test/integration/proofs_test.go   //go:build integration
.github/workflows/footprint.yml
docs/measurements/phase0-ci.md    results (written from CI logs)
specs/009-phase0-measurements/
```

## Design decisions

- Memory source is the node container's cgroup v2 tree, read with `docker exec` into the k3d node, because metrics-server is disabled in `dev/k3d.yaml` (an always-on component SPEC 3.2 excludes) and enabling it would distort the very number being measured.
- The reported figure is the working set (`memory.current - inactive_file`), matching kubelet's definition.
- "k3s + Traefik + CoreDNS + local-path" is reported as: whole node container, pod sum, remainder (k3s server, containerd, shims, kernel memory) and the k3s server process RSS, so the reader can see where the memory is.
- The k3d `serverlb` (nginx) container is separate from the node and is not counted; production k3s has no such container.
