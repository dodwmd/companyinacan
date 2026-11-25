# Measurements and verification record: E06

## Verified offline (in `make test`, no network)

| What | How |
|---|---|
| Bootstrap script syntax and lint | `bash -n` and `shellcheck --severity=warning` on the default and full variants and on the probe, status and uninstall scripts |
| Script properties from SPEC.md 3.1 | structural assertions: pinned version, `fail-swap-on=false`, `secrets-encryption`, metrics-server and cloud controller disabled, default-deny firewall, pod/service CIDR and CNI allows, 6443 never opened, ufw enabled after the last allow rule, image fetch starts before the k3s install, checksum verification |
| Shell-injection resistance | 12 hostile or malformed option values rejected before any script is produced; SSH host/user/path/mode validated |
| Hetzner client | `httptest` with recorded-style fixtures: create, adopt, destroy, retry, error mapping |
| Idempotency | second `up` makes no create call and uploads no script (Hetzner and ssh) |
| State locking | 25 concurrent mutations lose no update; `TryMutate` returns `ErrLocked` |
| Redaction | token never in stdout, stderr, JSON, errors or state, including when the API echoes it |
| CLI contract | exit codes 0/1/2/3, `--plan` side-effect free, `down` needs `--yes` |

## Not verified (needs a real server, account or cluster)

Listed as V001 to V005 in [tasks.md](tasks.md). In short:

1. The bootstrap has never run on a real host. The k3s pin `v1.33.5+k3s1`, the exact flag set, and
   that the ufw rules leave CoreDNS, the API server and kubelet working under default deny are
   assumptions until E00.
2. The Hetzner request and response shapes are from the public API documentation as remembered when
   writing the fixtures; they have not been replayed against the live API. Default server type
   `cx23`, image and location are unverified.
3. sslip.io resolution and Let's Encrypt issuance for the dashed names: nothing issues
   certificates until E03 deploys Traefik with ACME.
4. The SSH tunnel process (`OpenTunnel`) is only tested for argv and port selection; it was never
   connected to a real sshd.
5. **The five-minute budget is not measured.** That stays human-gated under E00; the procedure is
   in `docs/make-up.md`.
6. Cloud-init behaviour (runcmd ordering, log location) is untested on a real image.
