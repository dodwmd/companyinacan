# Run the whole stack locally with Docker Compose (no Kubernetes)

> **DEV ONLY. Insecure on purpose.** This stack uses public, fake tokens and passwords committed to the
> repository (`dev/compose/secrets/`, `docker-compose.yml`), plain HTTP, a fake token reviewer in can-core that
> reads module identities from files, and unpinned base-image tags. Never expose it to a network, never reuse its credentials, never put real data in it. The edge port is
> published on `127.0.0.1` only; keep it that way.

`docker-compose.yml` at the repository root builds and runs every service from source so you can click through
and curl the whole product on one machine, without k3s/k3d, Helm or cluster privileges. It is for trying things
out, not a supported way to run a company (use `make up` for that).

## Prerequisites

- Docker with the Compose v2 plugin (`docker compose`), **or** rootless/rootful Podman with `podman-compose`
  (on machines where `docker` is a podman shim, `docker compose` delegates to `podman-compose`). The file is
  verified end to end with podman-compose 1.6.0 on rootless Podman 6.1 and, in the advisory
  [e2e workflow](../.github/workflows/e2e.yml), with the real Docker Compose v5.1.3 on Docker Engine 29.4.2 on
  GitHub's `ubuntu-latest` (including `depends_on: condition: service_completed_successfully`, healthchecks and
  `down` / `up` with the volumes kept); it uses only features common to both tools.
- Network access to pull `golang:1.27`, `gcr.io/distroless/static-debian12:nonroot` and `caddy:2.8-alpine`
  (no digests are pinned for dev), and to download Go modules on the first build.
- `curl` and `bash` on the host for the smoke test. No Go toolchain is needed.
- About 5 GB of disk for the Go image and build cache; a first build takes a few minutes, later builds reuse the
  cached `go mod download` layer.

## Quick start

```sh
docker compose up --build        # foreground, logs on screen; or: make compose-up   (detached)
```

Everything is under the Compose project name `companyinacan`; containers are named `can-*`. The `init` job
prints the admin credentials in a banner (find it again with `docker compose -p companyinacan logs init`).

| URL | What |
|---|---|
| <http://localhost:8080> | the example site (`examples/acme-co`, built with `can site build`); `/contact/` is a dev-only page with a working form |
| <http://localhost:8080/_forms/contact/token> | forms gateway, same origin as the site (prefix stripped by the edge) |
| <http://admin.localhost:8080> | can-core admin UI (and `/api/v1/...` admin API) |
| <http://work.localhost:8080> | work tracker API (`/v1/...`, bearer token) |
| <http://work.localhost:8080/ui/> | work tracker web UI: sign in by pasting the work admin token below ([work-ui.md](work-ui.md)) |
| <http://people.localhost:8080> | people API (`/v1/...`, bearer token) |
| <http://jobs.localhost:8080> | recruiting web UI (sign in with a jobs access token, [recruiting-ui.md](recruiting-ui.md)); public careers pages at `/careers`; API under `/v1` (bearer token) |
| <http://finance.localhost:8080> | finance web UI (sign in with a finance token, see `docs/finance-ui.md`) and API (`/v1/...`, bearer token) |
| <http://forms.localhost:8080> | leads API of the forms gateway (`/v1/leads`, bearer token) |
| <http://mail.localhost:8080/messages> | the mail the stack "sent": a development-only SMTP sink that delivers nothing ([email.md](email.md)) |
| (not in the stack) | the `board` and `entity` modules (`cmd/can-board`) are not part of this compose stack yet; see `docs/board.md` |
| <http://equity.localhost:8080> | equity API (`/v1/...`, bearer token; cap table and option grants) |
| <http://files.localhost:8080> | files API (`/v1/...`, bearer token) and the public share-link route `/s/{token}`; no virus scanner runs here, so `scanner.required` is off and files are stored labelled `skipped` ([files.md](files.md)) |

The Apps launcher needs a second set of addresses for the same services, on one host name:

| URL | What |
|---|---|
| <http://localhost:8180> | can-core admin UI (open it here to use **Apps**) |
| <http://localhost:8181/ui/> | work web UI |
| <http://localhost:8182> | people web UI |
| <http://localhost:8183> | recruiting web UI |
| <http://localhost:8184> | finance web UI |

`*.localhost` resolves to loopback in browsers and in most `curl`/glibc setups. If yours does not (some
resolvers, older macOS tooling), use `curl --resolve admin.localhost:8080:127.0.0.1 ...`, add the names to
`/etc/hosts` (`127.0.0.1 admin.localhost work.localhost people.localhost jobs.localhost finance.localhost
forms.localhost equity.localhost files.localhost mail.localhost`), or use a resolver such as dnsmasq with `address=/localhost/127.0.0.1`.

### Dev credentials (all fake, all public)

| What | Value |
|---|---|
| Admin UI user / password | `admin` / `dev-only-admin-password` (`dev/compose/secrets/core.env`) |
| Admin TOTP | random; printed once by the `init` job. Run `dev/compose/bootstrap-admin.sh` any time to rotate and print a new secret |
| Work admin token | `dev-only-work-admin-token-0123456789abcdef` |
| People API token | `dev-only-people-api-token-0123456789abcdef` |
| Jobs admin token | `dev-only-jobs-admin-token-0123456789abcdef` |
| Finance admin token | `dev-only-finance-admin-token-0123456789abcdef` |
| Leads API token | `dev-only-forms-api-token-0123456789abcdef` |
| Equity service token | `dev-only-equity-api-token-0123456789abcdef` (roles hr, finance, admin; never board) |
| Files service token | `dev-only-files-api-token-0123456789abcdef` (files.admin, acts as `svc:static`) |

Use tokens as `Authorization: Bearer <token>`. The people, jobs, equity and files keyrings (field encryption) are random,
generated on the first `up` into the `dev-secrets` volume and never leave it.

### Logging in to the admin UI

1. `docker compose -p companyinacan logs init` (or run `dev/compose/bootstrap-admin.sh`) and copy the line
   `TOTP secret`.
2. Add it to an authenticator app (or `oathtool --totp -b <secret>`).
3. Open <http://admin.localhost:8080>, user `admin`, password `dev-only-admin-password`, then the 6-digit code.

### Modules, access tokens and single sign-on

can-core knows `work`, `people`, `jobs`, `finance` and `equity` as installed modules, so the catalogue,
personal access tokens, approvals, events and the Apps launcher work as they do on a cluster. Sign in to the
admin UI first (above). The `admin` account is the owner and holds every permission.

**Catalogue.** <http://admin.localhost:8080/catalogue> lists every module. Ticking or unticking one, previewing
and applying changes **only what can-core records**: Compose starts and stops containers, not can-core. A
disabled module's tokens and launch links stop working (within about 30 seconds); a module that has no
container in this stack (wiki, CRM, ...) can be ticked but nothing runs.

**Personal access tokens.** Under **API tokens** (`/tokens`), pick a module, tick permissions, issue, and copy
the `canpat_...` value (shown once). Use it like any other token:

```sh
curl -H "Authorization: Bearer canpat_..." http://work.localhost:8080/v1/projects
curl -H "Authorization: Bearer canpat_..." http://people.localhost:8080/v1/people
curl -H "Authorization: Bearer canpat_..." http://jobs.localhost:8080/v1/jobs
curl -H "Authorization: Bearer canpat_..." http://finance.localhost:8080/v1/parties
curl -H "Authorization: Bearer canpat_..." http://equity.localhost:8080/v1/entities
```

A token works for its own module only. `work` and `finance` check each permission. `jobs`, `people` and
`equity` turn permissions into a role, so tick what the role in `modules/<id>/module.yaml` lists:

- `jobs`: a role needs all of its permissions (recruiter: everything except `jobs.admin`).
- `people`: `people.read` is an employee; add `people.write` for HR, `people.admin` for admin and
  `people.team.manage` for manager.
- `equity`: `finance` needs `equity.read`, `write`, `approve` and `sensitive-read`; add `equity.admin` for
  admin. A personal access token is never the equity **board**, and because it carries no `person_id` an equity
  token **cannot** draft or administer grants, nor approve, pay or issue exercises, whatever permissions it has.
  It **can** write share ledger entries, with no holder separation-of-duties check ([equity.md](equity.md)).

**Apps (single sign-on into module web UIs).** Open the admin UI at **<http://localhost:8180>** (not
`admin.localhost`), go to **Apps** and press **Open**. In plain HTTP the launch cookie belongs to one host
name, so the admin UI and the module UIs have to share it and differ only by port
([sso.md](sso.md#opening-module-web-pages-from-the-admin-ui-e11c)); from `admin.localhost` the launch is refused.
`people` additionally admits only people in its directory, so create yourself one first, once, with the dev
people token:

```sh
curl -X POST http://people.localhost:8080/v1/people -H "Authorization: Bearer dev-only-people-api-token-0123456789abcdef" \
  -H 'Content-Type: application/json' \
  -d '{"kind":"employee","status":"active","given_name":"Ada","family_name":"Admin","work_email":"ada@acme.example","sso_subject":"user:admin"}'
```

**Cookies are not separated by port: this is weaker than a real installation.** The browser keeps one cookie
jar for `localhost`, so every server on any `localhost` port receives the cookies of all the others, including the
admin UI's `can_session` once you sign in at `localhost:8180`: the module UIs, the site on `localhost:8080`,
and **any unrelated program you run on a `localhost` port** and visit. Whoever receives that cookie can act as you
in the admin UI until the session ends, and any of them can overwrite or plant cookies for the others. The cookie
names differ, so signing in to one does not sign you into another, but that is no protection against the above.
Use a separate browser profile for this stack, do not run untrusted software on localhost while signed in, and
log out when done. A cluster puts each service on its own host name over https.

**How it works, and why it cannot be switched on in production.** `CAN_DEV_STACK=true` makes can-core build
module management without a cluster: the "installer" only records, `company.yaml` is a file in the
`core-data` volume (seeded from `dev/compose/config/company.yaml`), and the fake token review accepts the
random per-module tokens the `init` job writes, each to a volume of its own (`token-<module>`) that only that
module and can-core mount. Events are signed with the public
`x-webhook-secret` of `docker-compose.yml`. The mode exists only in a binary built with `-tags candevstack`
(`dev/compose/Dockerfile`); a release build refuses to start when asked for it, as does any build inside
Kubernetes, with an installer, with real token review or with secure cookies (`core/server/runtime/devstack.go`,
checked by `test/contract/devstack_test.go`).

### Smoke test

```sh
make compose-test            # or: dev/compose/smoke.sh
```

It goes through the edge like a browser: site 200, forms timing token then a form POST that must appear in the
leads API, admin login page 200 (on both of its addresses), work API creates a project and an issue with the dev token, healthz of
people/jobs/finance/work, and the careers page. It prints `ok` per check and exits non-zero with a message on
the first failure. It is safe to re-run (the project key `SMOKE` is reused; issues accumulate).

### Day to day

```sh
make compose-logs                         # follow logs
make compose-down                         # stop, KEEP data (named volumes)
make compose-down ARGS=-v                 # stop and DELETE data and generated keyrings
docker compose -p companyinacan ps        # health of each container
docker compose -p companyinacan up -d --build site    # rebuild one service (e.g. after editing company.yaml)
```

SQLite data lives in named volumes (`companyinacan_core-data`, `forms-data`, `work-data`, `people-data`,
`jobs-data`, `finance-data`, plus `dev-secrets` and the `token-*` volumes), so `down` without `-v` keeps it. Config files and
`secrets/*.env` are baked into / read at container creation: after editing one, recreate the service
(`docker compose -p companyinacan up -d --build --force-recreate <service>`). podman-compose does not
always notice an `env_file` change, or a rebuilt image, on its own: `up -d --build edge` left the old edge
container running until `--force-recreate` was added.

## The end-to-end suite

```sh
make e2e-up     # docker compose -p can-e2e -f docker-compose.yml -f dev/compose/e2e.yml up -d --build
make e2e        # go test -tags e2e ./test/e2e/...   (about 2.5 min; E2E_RESTART=0 skips the restart test)
make e2e-down   # down -v: removes the can-e2e containers and volumes only
```

Go scenarios in `test/e2e` drive the stack through the Caddy edge exactly like a browser or script (`*.localhost`
host names, resolved to 127.0.0.1 by the test client, so no resolver setup). Each scenario creates uniquely named
data and can be repeated against the same stack. The suite covers: the site and a contact form that becomes a lead
(a); admin login with password and TOTP, dashboard, audit, leads, token and catalogue pages (b); a work service token creating a
project and issue and the web UI board (c); people (1:1, leave request and decision, sensitive-field and leave
masking) (d); a jobs requisition, careers page with JSON-LD, a public application, the recruiter UI, blind
feedback (e); finance party, invoice, approval through can-core by a second administrator, public link, payment (f); equity grant from draft to
board approval and vesting (g); a file uploaded, downloaded as an attachment, versioned, shared once by link, held, erased (j); cross-module token refusal, hostile `Host`, no metrics, anonymous refusal (h);
`down` without `-v` and `up`, with all data and the admin TOTP secret intact (i); personal access tokens issued
in the admin UI against all five modules (k); an Apps launch signing in to the four module web UIs, refused in
another browser and from `admin.localhost` (l); a module disabled and enabled in the catalogue (m).

`make e2e-up` adds `dev/compose/e2e.yml`, which starts **introstub** (`dev/compose/introstub`, a 90-line fake of
a token introspection endpoint with five public fake identities: HR, manager, two employees and a board member) and
starts `people` and `equity` with `*.e2e.yaml` configs that call it. It gives the suite identities that personal
access tokens do not express here (several linked directory people, and a board member). introstub is test infrastructure for compose only; it is not in
any release.

TOTP: the suite reads the secret from `can-core admin reset-password --rotate-totp` (the same thing
`dev/compose/bootstrap-admin.sh` does), once per run, so **a run rotates the admin's TOTP secret**; sign in again
with `bootstrap-admin.sh` if you were using an authenticator. The CI job is advisory
(`.github/workflows/e2e.yml`: pull requests touching the stack, nightly, manual); it uploads the service logs when
it fails. The harness (`test/e2e/harness`) has its own unit tests (RFC 6238 vectors).

## Metrics and the optional monitoring profile

Every service starts a token-protected metrics listener on port 9190 of the private compose network
(`CAN_METRICS_ADDR`, `CAN_METRICS_TOKEN` in `docker-compose.yml`; the token is a public dev value like the
others). No metrics port is published to the host. `CAN_RAM_DECLARED_MB` carries each module's declared RAM so
`can_ram_rss_bytes` can be compared with `can_ram_declared_bytes`.

Prometheus and Grafana are not part of the default `up`:

```sh
docker compose -p companyinacan --profile monitoring up -d --build prometheus grafana
# Prometheus http://127.0.0.1:9090 (targets and alerts)   Grafana http://127.0.0.1:3000 (admin / dev-only-grafana-password)
```

Both bind to `127.0.0.1`, use pinned tags (`prom/prometheus:v2.55.1`, `grafana/grafana-oss:11.3.1`) and bake
their configuration into the image from `deploy/monitoring/`. See [observability.md](observability.md) for the
metric catalogue, alerts and the support bundle (`can diagnose --compose-project companyinacan`).

## What is NOT covered

- **No Kubernetes.** No k3s/k3d, no NetworkPolicies, no admission policy, no Helm, no `TokenReview`
  (can-core runs with `CAN_TOKEN_REVIEW=fake`; module identities are files), no Traefik, no TLS,
  no cert-manager. Resource limits and read-only root filesystems are not applied. The modules share one event
  signing secret, so one could forge events to another, which a cluster does not allow.
- **can-core does not install modules.** It records them (`CAN_INSTALLER=none` with `CAN_DEV_STACK`); Compose
  starts the containers. So:
  - purge, Secret rotation and the wizard's email relay credentials are not available;
  - `files`, `board` and `entity` are not wired to can-core; the admin UI's leads view says leads are
    not available (no `leads` provider is registered, no leads token file is configured);
  - forms sends its lead notification through can-core's `email.send` with a fixed development identity, to an SMTP
    sink that keeps the mail (http://mail.localhost:8080/messages, [email.md](email.md)); it is not otherwise wired
    to can-core and posts no webhook;
  - `equity` is introspected only: it publishes no events through can-core and takes no board decisions from it;
  - no reporting-line (`directory`) credential is configured, so approval chains that need a manager use their
    fallback;
  - finance uses the **fake** payment and e-sign providers; jobs has no virus scanner (`scanner_required:
    false`) and no job-board connectors; no real captcha, email relay, Postgres or backup hooks.
- No `can up`, DNS, ACME, provider provisioning or backups. `can-hooks` is not built.

### Finance invoice approval needs two people

finance enforces separation of duties and, wired to can-core, approves an invoice only after can-core's
approval engine did:

1. create the draft invoice (any token);
2. ask for approval with a **personal access token** with `finance.read` and `finance.write` (a static or
   service token is not a person): `POST /v1/invoices/<id>/request-approval`;
3. a **second administrator** holding the `finance:finance` role approves it under **Approvals** in the admin UI.
   Create one with `docker compose -p companyinacan exec -e CAN_ADMIN_PASSWORD=<password> core /can-core admin
   bootstrap --username approver` (prints its TOTP secret), then `... core /can-core admin roles grant approver
   finance:finance`;
4. once the signed event reaches finance, a token with `finance.approve` of a subject that neither created nor
   requested the invoice approves it (`POST /v1/invoices/<id>/approve`).

`FINANCE_FLOW=1 dev/compose/smoke.sh` checks the refusals only; the e2e suite runs the whole flow.

The finance web UI (`http://finance.localhost:8080/`, turned on by `dev/compose/config/finance.yaml`) signs in with
the same tokens (or through **Apps**): paste the dev admin token to act as `bootstrap-admin`. Cookies are not `Secure` in dev (plain HTTP). `dev/compose/smoke.sh` checks the login page, the anonymous
redirect, the stylesheet, a refused wrong token and that `/v1` and `/public/invoices/` still reach the API.

So this is good for exercising each service's own UI/API, can-core's tokens, approvals, events and single sign-on,
the form-to-lead path through a same-origin proxy, and the static site; it cannot tell you anything about how a
cluster installs or isolates the modules.

## Resources

Measured on rootless podman ([measurements/footprint.md](measurements/footprint.md) has the method and the
per-service figures). The whole stack, Caddy and the two stubs included, uses 73 MiB of cgroup memory idle and
about 120 MiB after an e2e run without its restart test (about 122 MB in `docker stats`; 166 MB before the
2026-11 reductions, measured the same way). Those figures leave out most binary file pages, which the image
build had already cached: the processes' resident sets add up to about 208 MiB idle and 242 MiB loaded. A 4-vCPU
CI runner measured less (68 MiB idle, 92 loaded). `docker stats` reports cgroup memory, not RSS. Building needs
more: allow 2 GB of free RAM and about 5 GB of disk while images build (Go compiles eleven binaries).

## Troubleshooting

- **Only one stack at a time.** Containers have fixed `container_name: can-*` names, so a second compose project
  name (for example `companyinacan` and `can-e2e`) collides with a running one. Tear one down before starting the other.

- **Port 8080 (or 8180 to 8184) already in use.** Stop whatever uses it, or change the left-hand side of the `edge` port mapping
  in `docker-compose.yml` (for 8180 to 8184 also `CAN_MODULE_UI_URLS`) (for example `127.0.0.1:8081:8080`); then also pass `EDGE_PORT=8081` to the smoke
  test and use that port in the URLs. The jobs and finance URLs configured in `dev/compose/config/*.yaml`
  (`http://jobs.localhost:8080`) assume 8080 and must be edited to match.
- **Rootless podman and ports below 1024.** The stack deliberately uses 8080. If you move the edge to port 80
  you need `sysctl net.ipv4.ip_unprivileged_port_start=80` (root) or another port.
- **`host.containers.internal`.** Not used: services reach each other by Compose service name on the project
  network (`forms`, `core`, ...). Use `host.containers.internal` (podman) or `host.docker.internal` (Docker) only
  if you add a service that must call something running on your host.
- **SELinux and `:z`.** The stack has no bind mounts (config is baked into images), so no `:z`/`:Z` labels are
  needed. If you add a bind mount on an SELinux host, append `:z` (shared) or `:Z` (private) to the volume.
- **`down` and `up` ordering errors with podman-compose.** `depends_on` uses `condition:
  service_completed_successfully` for the one-shot `init` job (verified with podman-compose 1.6.0 and with Docker
  Compose v5.1.3 in CI), and, in the e2e overlay only, `service_healthy` for the introspection stub; the `init` job is idempotent and may be re-run by podman-compose once per dependent
  service, so "already bootstrapped" lines in its log are normal. Outside the overlay nothing relies on `service_healthy`.
- **A service restarts in a loop.** `docker compose -p companyinacan logs <service>`: secrets and config are
  validated at start and the message names the problem (for example a token shorter than 32 characters).
- **Keyring "must not be accessible by group or others" / "owned by uid".** The `init` job creates keyrings
  with mode 0400 owned by uid 65532 in the `dev-secrets` volume. If you replaced that volume's contents by
  hand, run `docker compose -p companyinacan down -v` and start again.
- **cgroups / `cannot set memory limit` / no healthcheck status.** Rootless Docker/podman without cgroup v2
  delegation cannot enforce limits or run healthcheck timers; the stack sets no limits, and a container showing
  no `(healthy)` still works, check it with the smoke test. systemd-based healthchecks need
  `systemctl --user` available.
- **Pull fails for `gcr.io` or `docker.io`.** Check the network or registry mirrors; images can be overridden with
  `GO_IMAGE=... RUNTIME_IMAGE=... EDGE_IMAGE=... docker compose up --build`.
- **Other containers on the machine.** Everything here is namespaced by the project name `companyinacan` and
  `can-` container names. Tear down only this stack with `docker compose -p companyinacan down -v`; do not use
  `system prune`.
- **Locked out of the admin UI even with the right password.** can-core runs with `CAN_TRUST_PROXY=true`
  behind the edge, and every local client reaches it from the same address, so 5 bad logins lock the account
  (and 20 the address) for 15 minutes, correct logins included. Clear it with
  `docker compose -p companyinacan exec core /can-core admin unlock`.
- **`make e2e` says the edge did not answer.** The suite waits 90 s for `/_edge/healthz` and then exits with a pointer
  to `make e2e-up`. Check `docker compose -p can-e2e ps -a` and the logs of the service that is not healthy.
- **Admin login fails in the suite with 401 right after a login.** An authentication code can be used once; the
  suite waits for the next 30 s step and retries once, so a slow run is normal.
- **First build takes a few minutes and needs the network.** It pulls `golang:1.27`, distroless and caddy
  (not measured on a cold cache here) and downloads Go modules. Later builds are cached.
- **`no pod ... pod_companyinacan` on `down`.** A harmless message from podman-compose; the containers,
  network and (with `-v`) volumes are still removed.
- **Unknown Host headers.** The edge answers any request whose Host matches no vhost with `421 unknown host`
  (a catch-all block at the end of `dev/compose/config/Caddyfile`): not the site, no API, no admin page. Earlier
  versions of this document said such requests were served from the public site; in fact Caddy answered them with
  an empty `200`, which is why the catch-all was added (found by the end-to-end suite). Still keep the port on
  127.0.0.1.
- **Admin login keeps failing.** TOTP codes depend on the clock; check the system time. Run
  `dev/compose/bootstrap-admin.sh` for a fresh secret and clear lockouts with
  `docker compose -p companyinacan exec core /can-core admin unlock`.

## How it is built

- `dev/compose/Dockerfile`: one multi-stage build (repo root as context) that compiles all binaries once and has
  one target per service. Base images are build args (`GO_IMAGE`, `RUNTIME_IMAGE`, `EDGE_IMAGE`) with unpinned
  tags. `go mod download` is its own layer, so it is cached until `go.mod`/`go.sum` change. The release
  `deploy/*/Dockerfile` files (digest-pinned, fail on placeholders) are not used.
- `dev/compose/config/`: per-service dev configs and the Caddy edge config (Caddy: the smallest proxy that does
  host and path routing in a few lines; about 15 MiB).
- `dev/compose/secrets/`: the public fake `*.env` files.
- `dev/compose/init.sh`: the one-shot init job (keyrings, admin bootstrap).
- Health checks use the binaries' own `healthcheck` / `-healthcheck` commands (distroless has no shell or curl).
  can-core has no such command; it is checked through the edge by the smoke test.
