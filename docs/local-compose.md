# Run the whole stack locally with Docker Compose (no Kubernetes)

> **DEV ONLY. Insecure on purpose.** This stack uses public, fake tokens and passwords committed to the
> repository (`dev/compose/secrets/`), plain HTTP, a fake token reviewer in can-core and unpinned base-image
> tags. Never expose it to a network, never reuse its credentials, never put real data in it. The edge port is
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
| (not in the stack) | the `board` and `entity` modules (`cmd/can-board`) are not part of this compose stack yet; see `docs/board.md` |
| <http://equity.localhost:8080> | equity API (`/v1/...`, bearer token; cap table and option grants) |

`*.localhost` resolves to loopback in browsers and in most `curl`/glibc setups. If yours does not (some
resolvers, older macOS tooling), use `curl --resolve admin.localhost:8080:127.0.0.1 ...`, add the names to
`/etc/hosts` (`127.0.0.1 admin.localhost work.localhost people.localhost jobs.localhost finance.localhost
forms.localhost equity.localhost`), or use a resolver such as dnsmasq with `address=/localhost/127.0.0.1`.

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

Use tokens as `Authorization: Bearer <token>`. The people, jobs and equity keyrings (field encryption) are random,
generated on the first `up` into the `dev-secrets` volume and never leave it.

### Logging in to the admin UI

1. `docker compose -p companyinacan logs init` (or run `dev/compose/bootstrap-admin.sh`) and copy the line
   `TOTP secret`.
2. Add it to an authenticator app (or `oathtool --totp -b <secret>`).
3. Open <http://admin.localhost:8080>, user `admin`, password `dev-only-admin-password`, then the 6-digit code.

### Smoke test

```sh
make compose-test            # or: dev/compose/smoke.sh
```

It goes through the edge like a browser: site 200, forms timing token then a form POST that must appear in the
leads API, admin login page 200, work API creates a project and an issue with the dev token, healthz of
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
`jobs-data`, `finance-data`, plus `dev-secrets`), so `down` without `-v` keeps it. Config files and
`secrets/*.env` are baked into / read at container creation: after editing one, recreate the service
(`docker compose -p companyinacan up -d --build --force-recreate <service>`). podman-compose does not
always notice an `env_file` change, or a rebuilt image, on its own: `up -d --build edge` left the old edge
container running until `--force-recreate` was added.

## The end-to-end suite

```sh
make e2e-up     # docker compose -p can-e2e -f docker-compose.yml -f dev/compose/e2e.yml up -d --build
make e2e        # go test -tags e2e ./test/e2e/...   (about 30 s; E2E_RESTART=0 skips the restart test)
make e2e-down   # down -v: removes the can-e2e containers and volumes only
```

Go scenarios in `test/e2e` drive the stack through the Caddy edge exactly like a browser or script (`*.localhost`
host names, resolved to 127.0.0.1 by the test client, so no resolver setup). Each scenario creates uniquely named
data and can be repeated against the same stack. The suite covers: the site and a contact form that becomes a lead
(a); admin login with password and TOTP, dashboard, audit, leads and token pages (b); a work token creating a
project and issue and the web UI board (c); people (1:1, leave request and decision, sensitive-field and leave
masking) (d); a jobs requisition, careers page with JSON-LD, a public application, the recruiter UI, blind
feedback (e); finance party, invoice, second-subject approval, public link, payment (f); equity grant from draft to
board approval and vesting (g); cross-module token refusal, hostile `Host`, no metrics, anonymous refusal (h);
`down` without `-v` and `up`, with all data and the admin TOTP secret intact (i).

`make e2e-up` adds `dev/compose/e2e.yml`, which starts **introstub** (`dev/compose/introstub`, a 90-line fake of
can-core's token introspection with five public fake identities: HR, manager, two employees and a board member) and
starts `people` and `equity` with `*.e2e.yaml` configs that call it. Without it those two services accept one
static token, so a second person could not exist. introstub is test infrastructure for compose only; it is not in
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
  (can-core runs with `CAN_TOKEN_REVIEW=fake`, which accepts module tokens unverified), no Traefik, no TLS,
  no cert-manager. Resource limits and read-only root filesystems are not applied.
- **can-core does not install modules.** It runs with `CAN_INSTALLER=none` (records state only). The services
  are started by Compose as **standalone** processes and are **not registered** with can-core and **not wired
  to each other**; the only shared piece is the edge proxy. Concretely:
  - module management is off (no installer): `/catalogue` answers 503, the API tokens page offers no module, so no
    personal access token can be issued and none is verified by a module (issue
    [#43](https://github.com/dodwmd/companyinacan/issues/43));
  - the admin UI shows no installed modules and its leads view says leads are not available (no `leads`
    provider is registered, no leads token file is configured);
  - forms does not send email (`email.url` unset) and posts no webhook; work, people, jobs and finance post no
    events to can-core (`webhook.url` unset); can-core does not forward module events either;
  - people does not use can-core token introspection in the base stack (only its static service token works; the
    e2e overlay points it at a fake, see above); finance, jobs and
    people do not look each other up (hire hand-off from jobs to people, approvals through can-core, expense
    and timesheet links to work are stubs here: call the APIs directly);
  - finance uses the **fake** payment and e-sign providers; jobs has no virus scanner (`scanner_required:
    false`) and no job-board connectors; no real captcha, email, Postgres or backup hooks.
- No `can up`, DNS, ACME, provider provisioning or backups. `can-hooks` is not built.

### Finance invoice flow needs two subjects

The single dev finance token acts as one subject (`bootstrap-admin`), and finance enforces separation of duties:
whoever created or edited an invoice cannot approve it. So with that token alone you **cannot** complete
create, approve, send and the public invoice link (approve returns 403). To test the flow, mint a second token for
another subject with the approve scope using the tokens API, and approve with it:

```sh
FIN=http://finance.localhost:8080; ADMIN="Authorization: Bearer dev-only-finance-admin-token-0123456789abcdef"
curl -s -X POST $FIN/v1/tokens -H "$ADMIN" -H 'Content-Type: application/json' \
  -d '{"kind":"service","subject":"dev-approver","name":"approver","scopes":["finance.approve","finance.read"]}'   # prints "token": "cft_..."
curl -s -X POST $FIN/v1/invoices/<id>/approve -H "Authorization: Bearer cft_..."   # then send/link with the admin token
```

`FINANCE_FLOW=1 dev/compose/smoke.sh` runs this sequence (create party and invoice, creator approval refused,
approval by the second subject, send). The approver token is stored in the finance database, not in a file, so
there is no second static secret in `dev/compose/secrets/`.

The finance web UI (`http://finance.localhost:8080/`, turned on by `dev/compose/config/finance.yaml`) signs in with
the same tokens: paste the dev admin token to act as `bootstrap-admin`, and sign in with the approver token (above)
in a second browser profile or private window to see the approve button enabled for one subject and disabled for the
other. Cookies are not `Secure` in dev (plain HTTP). `dev/compose/smoke.sh` checks the login page, the anonymous
redirect, the stylesheet, a refused wrong token and that `/v1` and `/public/invoices/` still reach the API.

So this is good for exercising each service's own UI/API, the admin login flow, the form-to-lead path through a
same-origin proxy, and the static site; it cannot tell you whether the modules integrate with can-core.

## Resources

Measured with `docker stats --no-stream` on rootless podman: right after the smoke test can-core is about 29 MiB,
the edge about 15 MiB and the other services 2 to 5 MiB each; after some use (logins, clicking through the admin
UI) can-core reaches about 50 MiB and the whole stack stays **under 100 MiB** (an independent check measured
about 94 MiB; with equity and the e2e stub, after a full e2e run, `docker stats` summed to about 94 MB again: core 35, edge 13, work 9, people 9, jobs 8, finance 6, equity 4, forms 4, site 3, introstub 2). Building needs more: allow
2 GB of free RAM and about 5 GB of disk while images build (Go compiles ten binaries). Idle RAM of the images
is far below the budgets in the module READMEs because `docker stats` reports cgroup memory, not RSS.

## Troubleshooting

- **Only one stack at a time.** Containers have fixed `container_name: can-*` names, so a second compose project
  name (for example `companyinacan` and `can-e2e`) collides with a running one. Tear one down before starting the other.

- **Port 8080 already in use.** Stop whatever uses it, or change the left-hand side of the `edge` port mapping
  in `docker-compose.yml` (for example `127.0.0.1:8081:8080`); then also pass `EDGE_PORT=8081` to the smoke
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
