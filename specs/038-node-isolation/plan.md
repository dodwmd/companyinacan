# Implementation Plan: Pod-to-node isolation (Epic E06b)

**Branch**: `epic/E06b-node-isolation` | **Date**: 2026-11-25 | **Spec**: [spec.md](spec.md)

## Summary

One shell script, `core/provider/hostfw/can-firewall.sh`, embedded in `can`, written to
`/usr/local/sbin/can-firewall` by the server bootstrap and run by two systemd units. Decisions:
[research.md](research.md). Operator view: [docs/node-isolation.md](../../docs/node-isolation.md).

## Design

```text
inet can_isolation
  set trusted4                       addresses of system pods
  chain input   (priority -10, policy accept)
    iifname not cni0/flannel.1       accept        <- the operator's SSH ends here
    replies to host connections      accept        <- kubelet probes
    IPv6 neighbour discovery         accept
    @trusted4 tcp 53, 6443           accept
    @trusted4 udp 53                 accept
    drop
  chain forward (priority -10, policy accept)
    iifname not cni0/flannel.1       accept
    to 169.254.0.0/16, fd00:ec2::254, fe80::a9fe:a9fe   drop
```

| Unit | Does | Order |
|---|---|---|
| `can-firewall.service` (oneshot) | `can-firewall apply` | after `nftables.service` and `ufw.service`, before `k3s.service` |
| `can-firewall-sync.service` | `can-firewall run`: refresh the set, reload a missing table | after k3s; restarts always |

The bootstrap step `node-firewall` runs after the node is Ready: installs `nftables` if absent, writes
the script, runs `can-firewall install`. A failure fails the bootstrap; the script on the server can be
run again.

## Technical Context

**Language**: bash (host), Go 1.27 (embedding, `doctor`/`diagnose`). **Dependencies**: `nftables` on
the server (apt, if absent); no Go dependency added.
**Testing**: Go unit tests (static properties, bootstrap order, check parsing, CLI),
`test/hostfw/netns.sh`, `test/integration/z_hostfw_k3d_test.go`, workflow `node-isolation.yml`.
**Footprint**: one idle bash process plus a `k3s kubectl get pods` twice every 15 s (transient). **Not
measured.** No container, no pod.

## Constitution Check

| Principle | Assessment |
|---|---|
| I. Spec-first | SPEC.md 3.1, 6 and 11 are amended in this change. |
| II. Footprint | No new component in the cluster. Host process cost declared above, unmeasured. |
| III. Test-first | The behaviour is proved against real nftables and a real k3s with controls. The script and its namespace test were written together, not strictly test-first. |
| IV. Contracts | No capability or event changes. A module needing a host service is now refused; none does. |
| V. Secure by default | Deny by default for new pods; no new in-cluster privilege. |
| VI. Hygiene | No new dependency; DCO. |
| VII. Simplicity, measured claims | One table, one script. Unverified items listed below. |

## Cooperation with E08b (backups)

The only shared file is `core/provider/bootstrap.go` (and its golden files). This change adds one block
before `echo "$want_config"`; golden files are regenerated with `go test ./core/provider -update`.

## Unverified

- No real server: Ubuntu or Debian with ufw enabled, a reboot, k3s v1.33.5+k3s1 (CI uses v1.31.5 in k3d).
- No cloud: the real metadata service of Hetzner or DigitalOcean (a stand-in address is used).
- The CNI state directory path on a real k3s (if wrong, the set refreshes every 15 s only).
- `externalTrafficPolicy: Local` with real client addresses (k3d's load balancer hides them).
- Host memory and CPU cost of the sync loop.
- Dual-stack and multi-node clusters (not supported).
