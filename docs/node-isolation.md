# Node isolation: keeping pods away from the server

Kubernetes NetworkPolicy does not stop a pod reaching the node it runs on under k3s
([measurements](measurements/phase0-ci.md)). The server bootstrap therefore installs its own host
firewall rules. Design and threat model: [specs/038-node-isolation](../specs/038-node-isolation/spec.md).

**Tested in CI on throw-away environments only** (network namespaces, systemd on a CI runner, k3s in
k3d). It has never run on a real server; see [what is unverified](#what-is-unverified).

## What it does

| From a pod to | Module and other pods | `kube-system` pods and can-core |
|---|---|---|
| API server (node:6443, `10.43.0.1:443`) | blocked | allowed |
| DNS served by the host itself (53) | blocked | allowed |
| kubelet, SSH, any other host port | blocked | blocked |
| Cloud metadata (`169.254.169.254` and link-local) | blocked | blocked |
| Cluster DNS, other pods, Services, the internet | unchanged (NetworkPolicy still applies) | unchanged |

Traffic from outside the server is never touched: each chain first accepts whatever did not arrive on a
pod interface (`cni0`, `flannel.1`), so these rules cannot lock you out of SSH made **directly** to the
server. (`ufw` with `operator_ip` still can: [providers.md](providers.md).)

## What gets cut off

Anything that reaches the server **through a pod** arrives on a pod interface and is treated as pod
traffic:

- **SSH tunnelled through a pod in the cluster** (a `cloudflared`, Tailscale, WireGuard, bastion or
  similar pod that forwards to the server's port 22). It stops working when the rules load. Make sure you
  can reach SSH directly, or through the provider's console, before you install.
- **A `LoadBalancer` Service served by k3s ServiceLB whose backends use `hostNetwork`**. The `svclb`
  pod forwards to a port on the node, which is dropped, so the published port stops answering. Run the
  backend as an ordinary pod. (Traefik as shipped by k3s is an ordinary pod and is not affected.)

Neither has a setting to allow it: trusted namespaces get the API server and host DNS only.

## Checking

```sh
can doctor --server --company-dir .      # over ssh, read-only; exit 1 unless enforcing
can diagnose --server --company-dir .    # the same result in the support bundle
ssh root@SERVER can-firewall check       # on the server, with counts
ssh root@SERVER nft list table inet can_isolation   # rules and drop counters
```

**Detection is on demand only.** Nothing runs the check on a schedule, and nothing alerts you when the
rules or services stop. Run `can doctor --server` after changes to the server, or schedule it yourself.
(The sync service does reload missing, changed or dormant rules within seconds, but says so only in its
journal.) `can up` and `can status` print a warning, without failing, when the server has no rules or
they are not as expected (`can status` not while the server is still provisioning).

The check fails unless: both chains list exactly the lines the installed script loads (compared with the
same rules loaded into a throw-away network namespace by the same `nft`; counters ignored), the table is
not dormant, `cni0` exists, both units are enabled, the sync service is running,
`can-firewall.service` has not failed, and the trusted set matches the running system pods.

## When something is blocked that should not be

- **Your own workload needs the Kubernetes API** (an operator-installed controller): add its namespace,
  one per line, to `/etc/can/firewall/trusted-namespaces`. Namespaces starting `mod-` are ignored.
  Trust grants the API server and host DNS only. An address that a pod outside these namespaces also
  reports (not counting `hostNetwork` pods) is not trusted until the records agree again (the check reports
  it as a problem); a terminating
  trusted pod stays trusted only until its grace period ends.
- **A pod needs another host service** (a Prometheus pod scraping `node_exporter` on the node, a mail
  relay on the server (so [`email.send`](email.md) needs a relay elsewhere), the kubelet): not supported. Run it as a pod, or elsewhere.
- **Turn it off**: `can-firewall disable` on the server (stays off across reboots).
  Turn it on again with `can-firewall install`.

## Servers bootstrapped before this existed

`can up` does not re-run the bootstrap on a server that already has the pinned k3s. `can up` and `can
status` warn that the server has no node firewall and `can doctor --server` says `not installed`. Read
[what gets cut off](#what-gets-cut-off), then install it from a checkout of this repository:

```sh
scp core/provider/hostfw/can-firewall.sh root@SERVER:/usr/local/sbin/can-firewall
ssh root@SERVER 'apt-get install -y nftables && chmod 0755 /usr/local/sbin/can-firewall && can-firewall install'
```

The same command pair upgrades the script. If a re-install fails, the previous unit files and the units'
enabled and running state are put back, and so are the previous rules, with one limit: the newly copied
script stays, so **a unit that is restarted loads the new script's rules**. That happens at once when
systemd refused the new units (units that were running are restarted), and otherwise at the next restart
or reboot. To go back to the previous rules, copy the previous script and run `can-firewall install`
again. If the bootstrap itself stopped at the `node-firewall` step,
fix the cause and run `bash /usr/local/sbin/can-bootstrap.sh` on the server (running `can up` again would
fail its preflight, because k3s already holds the ports).

## How it fails

| Event | Effect |
|---|---|
| The rules do not load or verification fails during install | previous rules and units kept, bootstrap fails |
| systemd refuses the units during install | previous unit files and enabled/running state restored, bootstrap fails; restarted units load the new script's rules |
| Reboot | rules loaded before k3s with an empty trusted set; system pods wait a few seconds for the API |
| `can-firewall.service` fails at boot | k3s still starts, **without isolation** until the sync service loads the rules; `doctor --server` reports it |
| Sync service stopped | restarted system pods lose the API; `doctor --server` reports it |
| Another tool flushes the nftables ruleset, edits these chains or makes the table dormant | reloaded within seconds by the sync service |
| `unshare --net` does not work | the chains cannot be compared: a missing or dormant table is still reloaded, edited chains are not; the check fails |
| `ufw reload`, kube-proxy, flannel restarts | no effect: they do not own this table |

## What is unverified

- A real Ubuntu or Debian server: with `ufw` enabled, across a reboot, with the production k3s version.
- A real cloud metadata service.
- The CNI state directory the sync loop watches (otherwise it polls every 15 s).
- The host cost of the sync loop.
- Dual-stack and multi-node clusters are not supported.

Not covered at all: module egress to private networks outside the cluster, a compromised system pod or
can-core (they reach the API by design), and the NetworkPolicy programming delay for pod-to-pod traffic.
