# can-core deployment notes (E03)

Configuration is environment only. The can-core chart (part 2, T036) must set these.

| Variable | Default | Deployment value |
|---|---|---|
| `CAN_ADMIN_ADDR` | `:8080` | `:8080` (matches `deploy/base/networkpolicies.yaml`) |
| `CAN_MODULE_ADDR` | `:8090` | `:8090` (matches the module-API port in the NetworkPolicies) |
| `CAN_DB_PATH` | `/var/lib/can/state.db` | on the can-system PVC |
| `CAN_COOKIE_SECURE` | `true` | `true` (`false` only for plain-HTTP local dev) |
| `CAN_TRUST_PROXY` | `false` | **`true` behind Traefik** (see below) |
| `CAN_TOKEN_REVIEW` | `kubernetes` | `kubernetes` (`fake` is development only and logs a warning) |
| `CAN_PLATFORM_URL` | `http://can-core.can-system.svc.cluster.local:8090` | same |
| `CAN_MEMLIMIT_MB` | 64 | soft Go memory limit; container limit 96 MB |

## Client addresses and login throttling (`CAN_TRUST_PROXY`)

Behind Traefik the TCP peer of every admin request is the Traefik pod, so without help all clients share one
address. can-core therefore has two throttling modes and picks one from `CAN_TRUST_PROXY`:

- **`CAN_TRUST_PROXY=true` (the deployment default for the chart).** The client address is the **last**
  `X-Forwarded-For` entry, the one Traefik itself appends (never the first, which the client controls). Repeated
  failures then lock the (username, address) pair (5) or the address (20) for 15 minutes, and an address is limited to
  10 attempts a minute. Requirements: Traefik must be the only route to the admin port (the NetworkPolicy already
  allows only Traefik) and must not be told to trust forwarded headers from arbitrary clients
  (`forwardedHeaders.trustedIPs` unset or limited to a known load balancer), and the Service keeps
  `externalTrafficPolicy: Local` (already set in `deploy/base/traefik-config.yaml`) so the real source address
  reaches Traefik.
- **`CAN_TRUST_PROXY=false` (the binary's default, correct when addresses cannot be trusted).** Nothing is ever
  refused for failures, because a shared address would let an anonymous attacker lock the real admin out. Failures
  only add an increasing delay (250 ms doubling to 2 s) and at most 256 logins wait at once (more get 503). The admin
  with valid credentials and TOTP code always gets in, only slower while under attack.

In both modes: the TOTP code is checked before the password, so junk logins never run the 19 MiB argon2 step (a
guess passes the code check with probability about 3 in a million); argon2 runs under a concurrency cap; every login
takes at least 150 ms regardless of which check failed. Residual risk in the shared-address mode: an attacker who
already knows the password can guess TOTP codes at a bounded rate (roughly 100 requests a second at the cap, about
3e-6 chance each). Turn on `CAN_TRUST_PROXY` and add the Traefik IP allowlist (SPEC 3.1) to close that.

Recovery from inside the pod: `can-core admin unlock [--username]` and `can-core admin reset-password
[--rotate-totp]`.
