# Company in a Can

A self-hosted business control panel, in the spirit of Webmin but for business people: one server,
one admin login, a company site with lead capture, and modules for the rest of the company life
cycle (support, work tracking, people, hiring, invoices, equity, ...) that you switch on and off.

> **Status: pre-release. Nothing has been run on a real server yet.**
> Everything is built and tested offline, in CI and on throwaway local clusters. No `make up` has ever
> provisioned a real server, no real DNS, ACME certificate, email relay, identity provider, payment
> provider or browser has been used against it, and no release has been cut. The full stack runs
> end to end on one machine with Docker Compose (see below) and that run is tested in CI, but that
> stack is [insecure on purpose](docs/local-compose.md). Treat this as a design and a working
> prototype, not as something to put real company data into. [What is verified and what is not](#what-is-verified)
> is listed below, and [the security model](docs/security-model.md) lists the known weaknesses.

## Try it in two commands (Docker Compose, dev only)

```sh
make e2e-up        # builds and starts the whole stack as compose project "can-e2e" (first build: a few minutes)
make e2e           # runs the end-to-end suite through the Caddy edge (about 30 seconds)
```

Then open <http://localhost:8080> (the example site), <http://admin.localhost:8080> (admin UI, user `admin`,
password and TOTP as in [docs/local-compose.md](docs/local-compose.md)), <http://work.localhost:8080/ui/>
(work tracker), <http://jobs.localhost:8080> (careers and recruiter UI) and the other hosts listed there.
`make compose-up` starts the same stack as project `companyinacan` without the test overlay. Everything
there uses public fake secrets and plain HTTP, so never expose it. More paths (local k3d cluster, a real
server) are in [docs/getting-started.md](docs/getting-started.md).

## What is in it

| Module | What it is | Status | Declared RAM | Notes |
|---|---|---|---|---|
| can-core | Control plane: admin UI (password + TOTP, optional OIDC SSO), module registry, RBAC, approvals, notifications, events, audit log, personal access tokens | experimental | 48 MB (soft limit 64) | Runs in compose with module management off; the Kubernetes installer is exercised only on k3d |
| site + forms | Static company site from `company.yaml`, lead-capture gateway (`leads`) | forms: no status set (treated as stable); the site is rendered by `can` | 15 MB (forms) | Verified end to end in compose; forms has never run on a cluster |
| `work` | Jira-like tracker with API, CLI and web UI | experimental | 28 MB | Installed through can-core on k3d once (advisory CI) |
| `people` | Directory, org, 1:1s, goals, reviews, time off, web UI | experimental | 30 MB | Sensitive fields sealed, one policy layer |
| `jobs` | Requisitions, careers page with JobPosting data, feeds, ATS, recruiter UI | experimental | 30 MB | Board connectors are stubs |
| `finance` | Parties, invoices, payments, contracts, timesheets, expenses, purchase orders, web UI | experimental | 28 MB | Fake payment and e-sign providers only |
| `provisioning` | Joiner, mover and leaver account provisioning (SCIM) driven by people events | experimental | 20 MB | Tested against a fake directory and a fake SCIM server only; not in the compose stack ([docs/provisioning.md](docs/provisioning.md)) |
| `files` | Spaces with access lists, versions, share links, scan hook, retention, legal hold, erasure; local or S3 storage | experimental | 20 MB | API only; no scanner ships, so uploads are closed by default (off in compose); runs in the compose stack and its e2e suite ([docs/files.md](docs/files.md)) |
| `equity` | Cap table, option grants with vesting, exercises, ledger | experimental | 16 MB | A tool, not legal or tax advice; quantities stored in plaintext |
| `board` | Boards and committees, meetings, quorum, resolutions (roll call, written consent, secret ballot), minutes | experimental | 20 MB | A tool, not legal advice; not in the compose stack; secret ballots are unlinkable only after the vote closes |
| `entity` | Legal entities, officers and directors, statutory registers with as-of views, filings calendar | experimental | 20 MB | A tool, not legal advice; same binary as `board`; register share counts stored in plaintext |
| `support`, `crm` | Wrapped FreeScout, EspoCRM | **not-deployable** | 250, 350 MB | Image needs root, tags unresolved |
| `newsletter`, `analytics`, `wiki`, `status` | Wrapped Listmonk, GoatCounter, DokuWiki, Uptime Kuma | experimental | 20 to 128 MB | Charts checked offline only, never started |

Status values are the ones in each `modules/<id>/module.yaml`; the full table with what was verified is
[docs/modules-catalogue.md](docs/modules-catalogue.md). `can validate` refuses a `not-deployable` module and
warns for `experimental` ones.

## How it fits together

```text
                          browser / Android PWA / curl
                                      |
                       (TLS, ingress: Traefik on k3s, Caddy in compose)
                                      |
        +-----------------------------+------------------------------+
        |              public paths   |   private: admin., work., ... |
        v                             v                               v
  site + forms  ---- events ---->  can-core  <---- events ----  modules (work, people,
  (static, /_forms)              (admin UI, RBAC,                jobs, finance, equity)
                                  approvals, audit,                 each: own namespace,
                                  tokens, registry)                 own SQLite, own tokens
                                      |
                              Kubernetes API (k3s, single node)
```

How leads, hires and approvals flow is drawn in [docs/architecture.md](docs/architecture.md).

## Clients

Web (responsive, server-rendered, no JavaScript framework) and Android through the installable PWA with Web
Push ([docs/pwa.md](docs/pwa.md)). A native Android app is deferred until the PWA's limits are proven. macOS
and iOS apps are out of scope.

## What is verified

| Claim | Evidence | Not verified |
|---|---|---|
| Unit, race and contract tests, licence check, lint | `make test`, required CI checks (`lint`, `unit`, `contract`, `licences`, `dco`) | |
| The whole stack, through its edge, on Docker Compose: site and lead capture, admin login with TOTP, work, people, jobs, finance, equity flows, cross-module negative checks, restart persistence | `make e2e`, advisory [e2e workflow](.github/workflows/e2e.yml) | personal access tokens verified by modules (compose has no Kubernetes TokenReview); TLS; backups |
| `make dev` creates a k3d cluster; the `work` module installs through can-core and a personal access token works | advisory `dev-cluster` workflow | forms, people, jobs, finance, equity on a cluster; event delivery between modules; approvals |
| Idle memory of k3s, can-static and forms on a CI runner | [docs/measurements/phase0-ci.md](docs/measurements/phase0-ci.md) | any 2 GB or 4 GB server |
| `make up` on a real server, DNS, ACME, backups to object storage, restore from the recovery kit | built offline against fakes | **everything real**: needs a provider account |

## Footprint: measured against claimed

| Item | Claimed | Measured | Source |
|---|---|---|---|
| k3s + Traefik + CoreDNS + local-path, idle | at most about 600 MB | 510 to 563 MiB node total, on a 16 GB CI runner (not a 2 GB VPS); the pods are 50 to 55 MiB of it | [phase0-ci.md](docs/measurements/phase0-ci.md) |
| can-core, idle | 48 MB declared | 41.9 MiB RSS idle, 60 MiB after 4,000 requests; 49 MiB in a cluster | [module-wiring.md](docs/module-wiring.md) |
| In-house services, idle | 15 to 30 MB declared each | 13.5 to 16 MiB RSS each (forms 3.2 MiB cgroup working set in CI) | module READMEs, phase0-ci.md |
| Whole compose stack (10 services incl. Caddy and the e2e stub, plus the one-shot init job), after the e2e suite | "under 100 MiB" | about 94 MB by `docker stats` on rootless podman | measured for E23; [local-compose.md](docs/local-compose.md) |
| "Starter stack fits a 2 GB server" | aim | **not measured**; the wrapped apps (support, newsletter, analytics) were never started | [SPEC.md](SPEC.md) 3.2, 11 |

## Security posture and limitations

Deny by default, hashed or sealed secrets, hash-chained audit log, TOTP on the admin account, separation of
duties on approvals, and one policy layer per sensitive module. Known open issues, stated plainly, are in
[docs/security-model.md](docs/security-model.md): NetworkPolicy does not stop pod-to-node traffic on k3s
(issues [#13](https://github.com/dodwmd/companyinacan/issues/13) and
[#16](https://github.com/dodwmd/companyinacan/issues/16), design undecided), equity quantities are stored in
plaintext, SSO covers the admin UI only, nothing here encrypts Kubernetes Secrets at rest, and the compose
stack is not safe to expose. Report vulnerabilities as described in [SECURITY.md](SECURITY.md).

## Observability

Every service has a token-protected metrics listener, `/readyz`, request ids and log redaction, and the compose stack has an
optional Prometheus and Grafana profile: see [docs/observability.md](docs/observability.md).

## Documentation map

Start at [docs/index.md](docs/index.md). The architecture and roadmap are in [SPEC.md](SPEC.md), the epic
list with statuses in [docs/epics/README.md](docs/epics/README.md), and each feature's spec, plan and tasks
under [specs/](specs/).

## Development

Requires Go (see `go.mod`), `git` and `make`; Docker with Compose v2 (or podman-compose) for the end-to-end suite.

```sh
make test                      # lint + unit + contract tests + licence check
make e2e-up e2e                # end-to-end suite on the compose stack; make e2e-down to remove it
make docs-check                # offline check of every relative Markdown link
make build                     # builds bin/can, bin/can-static, bin/can-core
bin/can init my-company --name "My Co" --domain myco.example --non-interactive
bin/can validate my-company --modules-dir modules
```

The project is developed spec-first with [GitHub Spec Kit](https://github.com/github/spec-kit).

## Contributing

Not yet open for outside contributions while the design settles; the rules are in
[CONTRIBUTING.md](CONTRIBUTING.md) (DCO sign-off, Spec Kit workflow) and [SECURITY.md](SECURITY.md). Ideas and
feedback via issues are welcome once the repository is public.

## Licence

[Apache License 2.0](LICENSE). Bundled or wrapped third-party applications keep their own licences; see
[THIRD_PARTY_LICENSES](THIRD_PARTY_LICENSES) and [modules/README.md](modules/README.md).
