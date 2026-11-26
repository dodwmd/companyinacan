# Feature Specification: Pod-to-node isolation (Epic E06b)

**Branch**: `epic/E06b-node-isolation` | **Date**: 2026-11-25 | **Closes**: issues #13, #16

## Problem

On k3s, Kubernetes NetworkPolicy does not cover traffic from a pod to the node it runs on (Phase 0,
[measurements](../../docs/measurements/phase0-ci.md)): a module pod under default-deny still reached the
API server (6443, also through `10.43.0.1:443`) and the kubelet (10250). SPEC 3.1 assumed it could not.
The bootstrap's `ufw` rules allow the whole pod CIDR in, because system pods need the API server, so they
do not close it either.

## Threat model

**Assets**: the node (root of everything), the Kubernetes API (Secrets, including module keyrings), the
kubelet (exec into any pod), SSH, cloud instance metadata (cloud-init user-data), any other service
listening on the host.

**Attacker**: code running in a module pod (a vulnerable or malicious wrapped app, a compromised
dependency, a remote code execution through a public module path). It runs under PodSecurity
`restricted`: non-root, no added capabilities, no host namespaces, no hostPath, no mounted API token.
It can open any connection the network allows.

**Out of scope**: an attacker who already has root on the node or cluster-admin; a compromised pod in
`kube-system` or can-core (both legitimately talk to the API); hostNetwork pods (PodSecurity refuses
them in module namespaces); other machines on a private network; pod-to-pod traffic (NetworkPolicy
does cover it); egress filtering to the internet.

| # | Threat | Control |
|---|---|---|
| T1 | Pod talks to the API server (node address or `10.43.0.1`) with a stolen or leaked credential, or exploits an API server bug | host input rules drop it |
| T2 | Pod attacks the kubelet | dropped |
| T3 | Pod brute-forces or exploits SSH from inside, where `operator_ip` and the cloud firewall do not apply | dropped |
| T4 | Pod reaches any other host service (a database on the host, node_exporter, a local mail relay), also over IPv6 link-local or the bridge address | everything to the host is dropped except what system pods need |
| T5 | Pod reads cloud metadata (cloud-init user-data, which holds the bootstrap) | host forward rules drop `169.254.0.0/16`, `fd00:ec2::254` and `fe80::a9fe:a9fe` for every pod |
| T6 | A new module pod starts before its protection exists (the ~20 s NetworkPolicy lag) | the host rules deny by default: a new address is untrusted until proven a system pod |
| T7 | The firewall locks the operator out | structurally impossible for connections made directly to the server, see FR-3; access tunnelled through a pod is cut off (documented) |
| T8 | Something else on the host removes or overrides the rules | own table; a drop is final across tables; the sync service reloads a missing, changed or dormant table; `can doctor --server` reports it (on demand only: nothing alerts) |

### Residual risks (accepted, documented)

- A pod in `kube-system` (Traefik is internet-facing) or can-core can still reach the API server: they
  need it. Authentication and RBAC remain their barrier.
- The trusted set is by pod address. After a system pod goes away its address stays trusted until the
  next refresh (seconds when the CNI state directory is seen, otherwise at most 15 s). The k3s IPAM
  hands out addresses in rotation, so immediate reuse is not expected; it is not prevented. An address
  also reported by a non-trusted pod is withdrawn, but a new pod's address appears in its status only
  after the pod has it: reuse within a terminating system pod's grace period is trusted until then.
- Nothing checks the state on a schedule or alerts: detection is `can doctor --server` (or the warning
  of `can up` and `can status`), run by a person.
- If the sync service stops, the set freezes: restarted system pods lose the API (loud), and stale
  addresses stay trusted until it runs again.
- Between kernel start and `can-firewall.service` no pods exist; if that unit fails, k3s still starts
  (availability over isolation) and the sync service tries again to load the rules.
- Module egress to RFC1918 networks other than the cluster (a private network, a home LAN) is **not**
  blocked: modules may need a relay there. IPv6 cluster networking is not supported.
- Root on the node can remove the rules.

## User Scenarios

1. **P1** An operator runs `make up`: the server ends with the isolation enforced, with no extra step,
   and the site, admin UI, DNS and modules work.
2. **P1** An operator's SSH keeps working whatever happens to the rules.
3. **P1** The server reboots, or a system pod restarts: isolation holds and the cluster recovers by itself.
4. **P2** An operator asks `can doctor --server` whether the isolation is in force and gets a non-zero
   exit when it is not.
5. **P2** Something is broken: `can-firewall disable` on the server turns it off.

## Functional Requirements

- **FR-1** Traffic from a pod to any address of the node is dropped, except replies to connections the
  host opened, IPv6 neighbour discovery, and, from trusted pods, TCP 6443 and TCP/UDP connections made to
  port 53 (wherever the host DNATs them).
- **FR-2** Trusted pods are the non-hostNetwork, unfinished pods of `kube-system`, can-core in
  `can-system`, and of namespaces the operator lists in `/etc/can/firewall/trusted-namespaces`
  (never `mod-*`). A terminating one counts only until its deletion timestamp (the end of its grace
  period). An address also claimed by an unfinished, non-trusted, non-hostNetwork pod is not trusted.
  A failed listing keeps the previous set. The check reports a contested system pod address as a problem.
- **FR-3** Every chain accepts first whatever did not arrive on `cni0` or `flannel.1`. The chains'
  policy is accept. The tool changes no other nftables table and never flushes the ruleset.
- **FR-4** Loading is atomic and idempotent. A ruleset that does not load changes nothing.
- **FR-5** `install` refuses to start without the pod bridge and at least one trusted pod, verifies
  that the rules are loaded, the system pods are trusted and the API server is ready, and otherwise
  restores the previous state (rules, unit files, enabled and running state) and fails the bootstrap.
  The previous script is not kept: a unit restarted by that rollback loads the installed script's rules.
- **FR-6** The rules are loaded at boot before k3s; the trusted set starts empty and fills when the API
  answers.
- **FR-7** DNS, ingress, can-core, kubelet probes, `kubectl logs`/`exec`, pod-to-pod traffic allowed by
  NetworkPolicy and module egress to the internet keep working.
- **FR-8** `can doctor --server` and `can diagnose --server` report the state over SSH, read-only,
  failing closed: both chains must list line for line (counters aside) as the script's rules do, the
  table must not be dormant, both units must be enabled, the sync unit running and
  `can-firewall.service` not failed. `can up` and `can status` warn, without failing, when a server has
  no rules or different ones; `can status` not while it is provisioning. The sync service reloads only a
  missing, dormant or changed table, never one it cannot compare.
- **FR-9** `make down` on the ssh provider removes the rules and units.
- **FR-10** Not supported, and cut off by the rules: access to the node tunnelled through a pod (SSH
  included), and k3s ServiceLB in front of `hostNetwork` backends.

## Success Criteria

- **SC-1** Real nftables in network namespaces (`test/hostfw/netns.sh`): each blocked path is first
  reached with the same probe, then blocked; operator SSH is reachable in every state.
- **SC-2** Real k3s in k3d: module and unlabelled pods lose the API server, kubelet, `10.43.0.1:443`
  and a stand-in metadata server; CoreDNS, Traefik and local-path restart and become Ready; ingress,
  DNS and internet egress work; the existing cluster tests pass with the rules loaded.
- **SC-3** Not claimed: anything on a real server (see [plan.md](plan.md), "Unverified").
