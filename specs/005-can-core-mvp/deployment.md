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
  refused for failures by address, because a shared address would let an anonymous attacker lock the real admin out
  (can-core logs a warning at start). Failures by address only add an increasing delay (250 ms doubling to 2 s) and at most 256 logins wait at once (more get 503). The admin
  with valid credentials and TOTP code gets in, only slower, unless the 256-waiter cap is saturated by a sustained flood (503 while it lasts; no persistent lockout; existing sessions are unaffected).

In both modes: the TOTP code is checked before the password, so junk logins never run the 19 MiB argon2 step (a
guess passes the code check with probability about 3 in a million); argon2 runs under a concurrency cap; every login
takes at least 150 ms regardless of which check failed. The IP allowlist (SPEC 3.1) is also the availability
mitigation against a sustained flood saturating the waiter cap or the hashing slot.

**Per-account limit (both modes).** Checking the code first would let someone who knows the password try codes as fast
as requests arrive. So every attempt on a username is counted before it is checked, and after 10 attempts (in flight or
failed) in 24 hours without a successful sign-in the password is checked first. A wrong password then changes nothing
else, so **nobody without the password can lock the owner out**. With the right password the code attempt is counted
before its outcome is used, and after 5 in 24 hours the account takes no code for 15 minutes. The answer stays the
ordinary 401; the audit log records `admin.login.code_locked` and can-core logs a warning. This lock means someone has
the password (or the owner's authenticator is wrong): change the password. `can-core admin unlock` lifts it, and so
does a new password. Someone who has the password can keep the owner locked out until it is changed. Staying under both
limits they get about 14 codes a day; guessing on through the locks, a few hundred a day, each lock audited.

Password-first checks must not take the single argon2 slot from everyone else. At most one of them holds or waits for
it; the others are refused at once, so a valid sign-in, a step-up, a password change or a set-up link waits for at most
two hashes. A password that has been verified since can-core started is recognised by a keyed MAC kept in memory only
(random key per process), without argon2, so a flood on the owner's own name does not keep the owner out. The remaining
case: right after a restart, before the owner's first sign-in, a flood on the owner's name that is still running can
have the owner's attempts refused until it stops.

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
