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
| `CAN_COMPANY_NAME`, `CAN_COMPANY_DOMAIN` | empty | company name and domain shown on the admin UI dashboard (E12a); the chart should set them from the applied `company.yaml` |
| `CAN_LEADS_TOKEN_FILE` | empty | file holding the bearer credential can-core presents to the `leads` provider for the admin UI leads view (E12a); unset = the view says leads are not available |

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
  with valid credentials and TOTP code gets in, only slower, unless the 256-waiter cap is saturated by a sustained flood (503 while it lasts; no persistent lockout; existing sessions are unaffected). can-core logs a warning at start in this mode.

In both modes: the TOTP code is checked before the password, so junk logins never run the 19 MiB argon2 step (a
guess passes the code check with probability about 3 in a million); argon2 runs under a concurrency cap; every login
takes at least 150 ms regardless of which check failed. The IP allowlist (SPEC 3.1) is also the availability
mitigation against a sustained flood saturating the waiter cap.

**Residual risk: guessing the authentication code.** With `CAN_TRUST_PROXY=false` nothing limits how many codes someone
who already knows a password can try. The back-off delays each attempt but attempts sent side by side wait side by side:
about 128 a second (256 waiters, 2 s each), plus a burst each time the counters start again. Three codes in a
million are valid at any moment, so such an attacker can expect to get in within about an hour. The password is then the
only real factor. Turn on `CAN_TRUST_PROXY` (and add the Traefik IP allowlist). With it, one address gets 10 attempts a
minute and is locked for 15 minutes after 5 failures on a name (about 480 guesses a day), but the locks are per
address: an attacker with many addresses gets that allowance for each.

**Detection, both modes.** Twenty failed sign-ins on one account within 24 hours write the audit entry
`admin.login.many_failures` and a log warning, once per 24 hours. Nothing is refused because of it. The usual cause of a
real alert is somebody who has the password or is guessing it: change the password.

Recovery from inside the pod: `can-core admin unlock [--username]` and `can-core admin reset-password
[--rotate-totp]`.

## Part 2 variables (installer and reconcile loop)

| Variable | Default | Deployment value |
|---|---|---|
| `CAN_INSTALLER` | `none` | `kubernetes` (deploys modules; `none` only records state) |
| `CAN_COMPANY_FILE` | `/etc/can/company/company.yaml` | the mounted ConfigMap `can-system/company` written by `can apply` |
| `CAN_MODULES_DIR` | `/modules` | module directories baked into the image |
| `CAN_RECONCILE_INTERVAL_SECONDS` | `30` | `15` in the dev manifest (the kubelet refreshes a mounted ConfigMap within about a minute) |
| `CAN_NODE_IP` | none (required with the Kubernetes installer) | downward API `status.hostIP`; excluded from module egress |
| `CAN_POSTGRES_ADMIN_URL` | none | from optional Secret `can-postgres-admin` key `url`; without it a module that needs Postgres fails with a clear error |
| `CAN_POSTGRES_HOST` | `postgres.can-data.svc.cluster.local` | the Service name modules use |

The manifests are in `deploy/can-core/` (applied after `deploy/base`, not part of it: base is applied before any image
exists). Build with `docker build -f deploy/can-core/Dockerfile -t can-core:dev .`; the base-image digests are
placeholders until a release (`make release-check`).
