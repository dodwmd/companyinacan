# Research: Phase 0 Measurements (E00)

Evidence used: the E02 `dev-cluster` workflow runs (a real k3d v5.8.3 cluster comes up and the
integration tests pass in about one minute on `ubuntu-latest`), the repository, and the CI runs
of this branch. Results of the new runs are in `docs/measurements/phase0-ci.md`, not here.

## R1. Where to run k3d
- **Decision**: GitHub Actions `ubuntu-latest`, the same checksum-verified k3d binary as `dev-cluster.yml`.
- **Evidence**: `gh run list --workflow dev-cluster.yml` shows successful cluster runs of ~1 minute.
- **Limit**: the runner is 2-4 vCPU with about 16 GB RAM. It can show what the processes use, not how a 2 GB box copes (page cache pressure, swap, OOM, memory cgroup limits of the VPS).

## R2. How to read memory
- **Decision**: cgroup v2 files inside the node container, via `docker exec`.
- **Why not `kubectl top`**: it needs metrics-server, which `dev/k3d.yaml` disables to save RAM; the default-variant job has it on but still reports through the same path so the variants are comparable. `ParseTop` exists for clusters that have it.
- **Why working set**: `memory.current` includes reclaimable page cache; kubelet and metrics-server subtract `inactive_file`.
- **Risk**: the cgroup layout inside k3d (cgroupfs vs systemd driver, nesting) was not known in advance. The parser accepts both pod-directory shapes; the first CI run shows which one is real, recorded in the measurements doc.

## R3. What "baseline" contains
Node container working set (everything in it), the pod sum (Traefik, CoreDNS, local-path-provisioner, and metrics-server in the default variant, plus the one-shot helm-install job pods which are Completed and use nothing), the remainder (k3s server, containerd, shims, kernel), and the k3s server RSS. SPEC §3.2's "~600 MB" row is the node total.

## R4. Images
- can-static and can-forms are built in CI from the repo Dockerfiles. Their base-image digests are placeholders by design (release pinning); CI resolves the current digests with `docker buildx imagetools inspect` and passes them as build args. This is a measurement build, not a pinned one.
- Other wrapped candidates (Listmonk, GoatCounter, ...) need databases and configuration per module; deferred, see spec "Blocked".

## R5. Proof design
- Every denial has a positive control (an unpolicied or explicitly allowed probe reaches the same target), as the E02 tests do.
- Pod-to-node egress: control reaches the node IP's API port 6443 from an unpolicied namespace; a module pod must not.
- TokenReview is done from outside the pod (`kubectl create token --audience`, impersonating `system:serviceaccount:can-system:can-core`), which proves the RBAC grant and the audience binding but not the projected-volume path inside a pod, and there is no can-core to call it yet.
- Module namespaces are created from `deploy/templates/*.yaml` with the same substitutions can-core will make (E03).

## R6. Unverified after this epic
Listed in the measurements doc ("Not measured").
