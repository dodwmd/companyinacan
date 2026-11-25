# Phase 0 measurements: the part CI can do (epic E00)

Recorded 2026-11-25 from the non-required `footprint` workflow (`.github/workflows/footprint.yml`)
on GitHub-hosted `ubuntu-latest` runners. Harness: `dev/footprint/`, `core/footprint`,
`can footprint report|check`. Spec: `specs/009-phase0-measurements/`.

## What these numbers are, and are not

**Measured here:** idle memory of real k3s v1.31.5+k3s1 clusters (k3d v5.8.3, one server, cgroup v2,
Docker), taken as the cgroup working set (`memory.current` minus `inactive_file`) of the k3d node
container and of each pod, median over 12 samples 10 s apart, after every pod was Running and a
60 s idle wait. The node container holds k3s, containerd and all pods.

**Runner:** GitHub `ubuntu-latest`, 4 vCPU, about 15.7-16 GB RAM (`/proc/meminfo` MemTotal 15700-15983
MiB), kernel 7.0.0-1020-azure (3 of 25 jobs landed on 7.0.0-1016-aws).

**What that does NOT prove.** This is not a 2 GB VPS. On a 16 GB machine nothing is under memory
pressure: the Go runtime of the k3s server (about 490 MiB RSS here) is not forced to collect, the
kernel keeps page cache, there is no swap, no OOM and no cgroup limit. A small VPS could show a
lower k3s figure (GC under pressure) or a higher system total (the OS, sshd, journald, containerd
and kubelet at boot are not in these numbers). Also not included: the k3d `serverlb` nginx
container (does not exist on a real server), the operating system, and anything transient (image
pulls, Helm renders). Treat the figures as the platform's own steady-state processes, on a roomy
host. The real-VPS figure is still blocked (below).

Run-to-run noise is about +/- 20 MiB on the node total (see the spread), so differences smaller
than that are not findings.

## 1. Idle RAM of k3s + Traefik + CoreDNS + local-path (MiB)

Five runs per variant. Node total = whole node container working set (median of each run's median);
range is min to max over the five runs.

| Variant (`dev/footprint/k3d-*.yaml`) | Node total, median of runs | Range | Pods only | k3s server RSS (range) |
|---|---|---|---|---|
| default: k3s as shipped (adds metrics-server) | 542.6 | 535.5 to 557.4 | 68-73 | 478 to 498 |
| trimmed: `--disable=metrics-server --secrets-encryption` (what `make dev` uses) | 516.4 | 508.3 to 523.0 | 50-57 | 486 to 499 |
| lean: trimmed + `--disable=servicelb --disable-cloud-controller` | 508.5 | 484.9 to 529.9 | 50-53 | 464 to 510 |
| deploy/base applied (`can dev up`) | 538.0 | 525.9 to 563.1 | 50-55 | 501 to 536 |

Per pod, steady state: Traefik 25-34, CoreDNS 13.3-14.3, local-path-provisioner 7.1-8.5,
svclb-traefik 0.9, metrics-server 18 (default variant only).

Reading:
- The whole node is about 510-560 MiB; roughly 460-510 of that is the k3s server, containerd and
  kernel memory, and only about 50-55 is the pods (Traefik is the largest).
- Disabling metrics-server saves its 18 MiB pod and some control-plane load, about 26 MiB on the
  median node total. Disabling ServiceLB and the cloud controller (lean) did not give a difference
  outside the noise, and it leaves Traefik's Service `<pending>`: do not adopt it on this evidence.
- Applying `deploy/base` costs roughly +20 MiB over trimmed (the three extra namespaces' objects,
  quotas and the HelmChartConfig which makes Traefik reconfigure), again near the noise floor.

**SPEC 3.2 row "k3s + Traefik + CoreDNS + local-path, target <= ~600 MB": confirmed as an upper
bound on a 16 GB runner (510-563 MiB, headroom 40-90 MiB).** It was never shown for a 2 GB host.

## 2. can-static and forms (images built in CI from the repo Dockerfiles)

| Component | Idle RAM, median of 5 runs (range) | Declared | Check |
|---|---|---|---|
| forms gateway (`mod-forms`, Deployment, no traffic) | 3.2 MiB (3.14 to 3.34) | `ram_mb: 15` in `modules/forms/module.yaml` | `can footprint check`: within (limit 18.00) in every run |
| can-static serving a one-page site | 2.2 MiB (2.14 to 2.30) | none (no module manifest) | not checked |

These are working sets with no requests served; the forms README quotes 13.9 MiB RSS, a different
measure (RSS includes mapped binary pages that the working set excludes). The declared 15 MB is a
request for a 1.2x check, so the forms module is far inside its budget on this measure. Under load
and with a real database it will be higher; not measured. The images are built with the digests of
the current `golang:1.27` and distroless tags, not pinned release digests.

## 3. NetworkPolicy, module-auth and CoreDNS proofs (stub modules on real k3s)

`test/integration/proofs_test.go` instantiates `deploy/templates/module-namespace.yaml` and
`module-rbac.yaml` the way can-core will. Every denial has a positive control. Result of the
last two runs (the earlier runs differ only by test-harness bugs fixed since):

| Proof | Result |
|---|---|
| Template applies; PodSecurity `restricted` rejects a root pod; LimitRange defaults (32Mi request, 192Mi limit) applied | PASS |
| CoreDNS rewrite from a module namespace: `foo.localtest.me` resolves to the Traefik ClusterIP, `admin.localtest.me` is not rewritten | PASS |
| Module A cannot reach a pod in module B (control: explicitly admitted namespace can) | PASS |
| A module namespace reaches the can-core module API port 8090 on a stand-in pod, not the admin port 8080 | PASS |
| TokenReview as `can-core`: valid module token authenticates as `system:serviceaccount:mod-stuba:stub`, wrong audience rejected, tampered token rejected, a module ServiceAccount cannot create TokenReviews | PASS (from outside the pod: `kubectl create token --audience`, impersonating can-core; the projected-volume path inside a pod is not exercised) |
| can-core RBAC: no secrets in kube-system or in a namespace without the RoleBinding, workloads allowed only after the per-namespace RoleBinding | PASS |
| **Pod-to-node egress denial** | **FAIL, finding** |

**Finding (issue #13): NetworkPolicy does not stop pod-to-node traffic on k3s.** A pod in a
namespace with only a default-deny Ingress+Egress policy still connects to the node's API port
(172.18.0.2:6443) and kubelet (:10250); a module-template pod still reached `NODE_IP:6443` and the
kubernetes Service `10.43.0.1:443` 90 s after its namespace existed, despite the `ipBlock except`.
Pod-to-pod default-deny works. A host rule in the node's INPUT chain (`iptables -I INPUT -s
10.42.0.0/16 -d <node> -p tcp --dport 6443 -j DROP`) did block it. So SPEC 3.1's host-firewall step
(E06) is required, not optional, and the comment in the template ("also blocks the Kubernetes API
server") was wrong. Until then a module pod can talk to the API server and the RBAC of its
ServiceAccount is the only barrier.

**Second finding: policy enforcement lags.** In a new default-deny namespace a pod could still
reach another pod at +0 s and was blocked by +20 s (kube-router programs policies asynchronously).
A freshly started module pod has a short window of seconds during which default-deny is not yet in
effect. Module workloads should start after the policies are effective, or tolerate the window.

## 4. Not measured (blocked or deferred)

| Item | Why |
|---|---|
| Idle RAM on a real 2 GB and 4 GB VPS, with and without trimming | needs a provider account (human-gated) |
| `make up` end to end against the five-minute budget (SPEC 3.4) | needs a provider account and token; `can dev up` bringing a k3d cluster ready took about a minute in CI, which is not the same thing |
| Image-pull time, sslip.io and Let's Encrypt behaviour, ufw rules with k3s on a real host | need a real network and host |
| Idle RAM of Postgres/MariaDB, FreeScout on Postgres, Listmonk, GoatCounter/Umami, Plane, Kimai/Solidtime, Documenso, Snipe-IT, Invoice Ninja/Akaunting; their licences | each needs configured databases and a module manifest; deferred, the harness (`measure.sh --manifest`) is ready for them |
| Swap/zram behaviour, OOM, peak memory during image pulls and Helm renders | a 16 GB runner cannot show it |
| can-core, site renderer pod and the portal | can-core does not exist yet (E03) |

## 5. Runs

| Run | Event | Link |
|---|---|---|
| 1 | pull_request | https://github.com/dodwmd/companyinacan/actions/runs/45545124840 (proofs had harness bugs; baselines valid) |
| 2 | workflow_dispatch | https://github.com/dodwmd/companyinacan/actions/runs/45545824842 |
| 3 | pull_request | https://github.com/dodwmd/companyinacan/actions/runs/45545829517 |
| 4 | workflow_dispatch | https://github.com/dodwmd/companyinacan/actions/runs/45546467413 (proofs as tabled) |
| 5 | pull_request | https://github.com/dodwmd/companyinacan/actions/runs/45546477050 (proofs as tabled) |

Each run uploads its JSON reports, raw samples and `meta.txt` as workflow artifacts (retained 90
days by default); the numbers above are copied from them.
