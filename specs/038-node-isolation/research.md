# Research and decisions

## R1: Where to enforce

| Option | Verdict |
|---|---|
| NetworkPolicy | does not cover pod-to-node on k3s (Phase 0) |
| Cilium or Calico host policy | would cover it, but replaces the bundled CNI/policy engine and adds an agent to every server, against the footprint budget (SPEC 3.2); not measured |
| Host drop of pod CIDR to node:6443 | breaks CoreDNS, Traefik, can-core: same CIDR, same DNAT'd destination |
| can-core maintains a deny set of module pod addresses | fails open (a new module pod is allowed until listed), needs can-core to reach the host (it is a pod, with no host access, deliberately) |
| Accept, rely on RBAC | leaves the kubelet, SSH, other host services and metadata reachable |
| **Chosen: host nftables, deny by default, allow set of system pod addresses maintained on the host** | fails closed for new pods, no CNI change, nothing in-cluster gains privilege |

## R2: Coexisting with ufw, kube-proxy, flannel, kube-router

An own `inet can_isolation` table with base chains at priority -10. In nftables an accept ends only the
chain it is in, while a drop is final, so no accept elsewhere (ufw's pod CIDR allow, kube-router's
chains) can undo our drop, and ufw, `iptables-restore` and k3s's cleanup do not touch a native table
they do not own. kube-proxy's DNAT happens at prerouting, before the routing decision, so
`10.43.0.1:443` reaches the input hook as node:6443, and ServiceLB/NodePort traffic that is DNAT'd to a
pod goes through forward, not input. Debian's `nftables.service`, if someone enables it, flushes the
ruleset: our unit is ordered after it and the sync service reloads a missing table.

A host resolver may be reached through DNAT to another port: k3d rewrites the node's resolver so that
CoreDNS's upstream queries end on a random port of the node. A rule on `dport 53` broke upstream DNS
there (found by running the existing cluster tests with the rules loaded), so DNS is matched with
`ct original proto-dst 53`.

## R3: Identify pod traffic by interface, not by source address

`iifname` `cni0`/`flannel.1` rather than the pod CIDR. An operator whose own address falls inside
10.42.0.0/16 (a VPN, a LAN) arrives on another interface and is never matched, and a pod cannot escape
by choosing a source address. A different CNI makes the rules match nothing; `check` and `install`
therefore require `cni0`.

## R4: Rollback, and why there is no dead-man timer

`nft -f` is one transaction: a ruleset that does not load changes nothing. `install` saves the previous
table, verifies after loading and restores it on failure. When the units themselves are refused, the
rollback restarts the ones that were running, and those load the rules of the script now installed: the
previous script is not kept. A timed auto-revert protects against rules
that cut the operator off; these rules cannot match the operator's traffic (R3), so a timer would only
add a way for the protection to disappear.

## R5: Keeping the trusted set current

A shell loop on the host: `k3s kubectl get pods`, every 15 s and, at once, when the CNI's address
directory (`/var/lib/cni/networks/cbr0`) changes. Rejected: a long-lived `kubectl --watch` (resident
memory), nftables element timeouts (a stopped agent would cut CoreDNS off the API), persisting the set
across reboots (addresses are reassigned; a stale entry could trust a module pod).

## R6: Testing without a server

Network namespaces give a real kernel, real nftables and controlled peers, including the metadata
address. k3d gives real k3s, kube-proxy, flannel and kube-router, but its node has neither nft nor
systemd, so the shipped script runs from a sidecar sharing the node's network namespace. systemd units
are exercised separately on the CI runner, with a dummy `cni0`. None of this is a rebooted Ubuntu or
Debian server with ufw.
