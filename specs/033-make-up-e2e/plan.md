# Implementation Plan: `can up` end to end (Epic E24)

**Branch**: `epic/E24-make-up-e2e` | **Date**: 2026-11-25 | **Spec**: [spec.md](spec.md)

## Summary

A new package `core/launch` holds everything `can up` does once a cluster exists. `core/cli/up.go` keeps
provisioning (provider, bootstrap, kubeconfig, instant URL) and calls `launch.Run` through a seam, then
prints the summary. `launch.Run` is the same code for a real server (kubectl through the SSH tunnel) and for
the k3d cluster in CI. Design choices in [research.md](research.md), state in [data-model.md](data-model.md),
CLI contract in [contracts/cli.md](contracts/cli.md).

## Technical Context

**Language/Version**: Go 1.27 (existing module). **New dependencies**: none.
**External binaries**: `kubectl` on the operator machine (new hard requirement of `can up`, checked
before provisioning), `ssh` (existing), `podman` or `docker` only for `make images`.
**Storage**: `.can/state.json` gains a `launch` object (phase checkpoints, never a secret).
**Testing**: unit tests with fakes for `Kube`, `ImageLoader`, the host `Runner`; CLI tests through the
existing `lifecycle` seams; `test/integration/makeup_test.go` on k3d (advisory workflow `make-up-e2e.yml`).
**Constraints**: no real server, cloud account or registry anywhere in the test suite.

## Constitution Check

| Principle | Assessment |
|---|---|
| I Spec-first | SPEC.md section 6 gets a note; this spec precedes the code. PASS |
| II Footprint | No new always-on component on the server. The site pod is can-static (requests 16 MiB, limit 32 MiB) plus a short-lived init container. The operator needs `kubectl`. PASS, measured numbers below the budget table in docs/make-up.md |
| III Test-first | `core/launch` tests (images, manifests, stack, backup, loader, status) were written and run red before the implementation; the can-static `-extract` test was run red; CLI tests were added with the CLI. PASS (the CLI commit is not strictly red-first: the legacy tests were changed to `--provision-only`) |
| IV Stable contracts | Module manifests, schemas and the `Provider` interface are untouched. `.can/state.json` is extended additively (version stays 1). PASS |
| V Secure by default | Secrets on stdin only, never argv, state or logs; redactor learns every generated secret; kit once, never in a git tree; fail closed (unpinned image refs, missing kubectl, no kit destination, tunnel failure); no destructive default; host keys keep the existing TOFU file (`.can/known_hosts` for cloud servers); firewall untouched. PASS |
| VI OSS hygiene | No new dependency; scripts are shellchecked; workflow actions are SHA-pinned with `contents: read`. PASS |
| VII Simplicity and measured claims | The five-minute claim is not made: CI timings are reported as k3d-on-a-runner numbers; the real-server run stays human-gated. PASS |

## Structure

```
core/launch/        images.go manifests.go kube.go loader.go stack.go backup.go  (+ tests)
core/cli/           up.go (seams, flags, flow)  up_stack.go (preflight, routes, summary, status/down helpers)
core/staticsrv/     extract.go           safe tar.gz unpack for the site pod
cmd/can-static/     -extract flag
core/provider/      state.go             LaunchState (additive)
deploy/             stack.go (embed)  site/ (site pod)  [base/ can-core/ unchanged]
scripts/            build-images.sh  gen-release-images.sh (+ self-test)
test/integration/   makeup_test.go
.github/workflows/  make-up-e2e.yml
```

## Phases of `can up`

preflight -> provision (+ k3s bootstrap) -> kubeconfig + tunnel -> base -> images -> company -> core -> admin
-> site -> routes -> backup (optional) -> ready -> DNS -> probe -> summary. Details and the resume table are in
docs/make-up.md. `company` runs before `core` on purpose: a ConfigMap volume carries its content from pod
start, whereas an update to a running pod waits for the kubelet sync (a minute or more).
