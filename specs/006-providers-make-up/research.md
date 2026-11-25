# Research: E06 Providers and `make up`

## R1. SSH transport: system `ssh` binary vs `golang.org/x/crypto/ssh`

**Decision**: exec the system `ssh` behind the `Runner` interface.
**Why**: no new dependency (and licence entry), honours the operator's agent, `~/.ssh/config`,
known_hosts and ProxyJump, and `-L` tunnels come for free. Scripts travel on stdin to `bash -s`, so
nothing sensitive is on a command line. `StrictHostKeyChecking=accept-new` pins the host key on first
use. Host, user, paths and modes are validated before reaching argv (an `-oProxyCommand=` host is
rejected).
**Rejected**: x/crypto/ssh (must re-implement agent, known_hosts, config parsing).

## R2. k3s flags as a config file

**Decision**: write `/etc/rancher/k3s/config.yaml` (`disable`, `disable-cloud-controller`,
`secrets-encryption`, `write-kubeconfig-mode`, `kubelet-arg`, CIDRs) before the pinned install,
instead of `INSTALL_K3S_EXEC` flags.
**Why**: the config file is read on every start, so a later re-run of the install script for an
upgrade (SPEC.md section 5) keeps the same flags. Same effect as the flags SPEC.md lists.
**Unverified**: that this flag set yields the <= 600 MB baseline and that the pinned version
`v1.33.5+k3s1` is the right one: E00.

## R3. Host firewall

**Decision**: `ufw` (installed via apt when missing): default deny incoming, allow routed, allow
22 (or only the operator IP), 80, 443, the pod and service CIDRs, `cni0` and `flannel.1`; enable
last so there is no lockout window. Hosts without apt get a warning, not a silent skip.
**Unverified** on a real host: that these rules let CoreDNS, the API server and kubelet work under
default deny (SPEC.md says Phase 0 verifies this).

## R4. Preflight

A read-only probe prints `key=value` lines; a pure parser turns them into checks, which keeps the
logic testable without a host. Thresholds: RAM >= 1700 MB (a "2 GB" VM reports less than 2048),
disk >= 10 GB, Ubuntu or Debian, amd64 or arm64, systemd, root (sudo handled by the runner), ports
80/443/6443 free unless the bootstrap marker shows k3s itself holds them.

## R5. Idempotency and resume

Every step records completion in state; remote steps are additionally guarded on the host (marker
file with the pinned version, swap and firewall commands are re-runnable). For Hetzner every created
resource is persisted immediately, and lookups by fingerprint/name adopt resources from an
interrupted run. POST is never retried on 5xx. The SSH key is deleted on `down` only if `can`
uploaded it.

## R6. State locking

`flock(2)` on `state.json.lock` for the whole read-modify-write, atomic rename for the write. The
lock is held only for the short mutation, never across network calls. `TryMutate` exists for callers
that must fail fast.

## R7. Redaction

Two layers: literal secrets registered when a reference is resolved (so the Hetzner token is
removed even when the API echoes it), plus patterns for bearer tokens, `token|secret|password`
assignments and private-key blocks (so secrets printed by remote scripts are caught). The writer is
line-buffered so a secret split across writes is still removed.

## R8. sslip.io and ACME limits

Rules used (letsencrypt.org/docs/rate-limits): 5 duplicate certificates per week, 5 failed
validations per hour per hostname. The planner stays below both (4 and 3). The "50 certificates per
registered domain per week" limit is shared with every sslip.io user and cannot be known locally;
the HTTP fallback is the answer when issuance fails, not prediction. DNS is checked by resolving the
dashed name and comparing with the server IP. Fallback is deliberately weaker: plain HTTP,
public site only, admin only via tunnel.

## R9. Hetzner defaults

`ubuntu-24.04`, `fsn1`, server type `cx23`. The type name and location are the most likely things to
rot; they are overridable and flagged unverified. Cloud-init is `#cloud-config` with the script as a
base64 `write_files` entry, run by `runcmd`; completion is detected over SSH by the marker file.

## R10. Tunnel

`ssh -N -L 127.0.0.1:<port>:127.0.0.1:6443` with `ExitOnForwardFailure`. The kubeconfig is
rewritten to a fixed default port (16443) so it stays valid between runs; `OpenTunnel` accepts a
port (0 = any free) for E03's use.
