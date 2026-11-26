# Company in a Can

A self-hosted business control panel, in the spirit of Webmin but for business people: one server,
one admin login, a company site with lead capture, and modules for the rest of the company life
cycle (support, work tracking, people, hiring, invoices, equity, ...) that you switch on and off.

> **Status: pre-release. Nothing has been run on a real server yet, and no release has been cut.**
> No real server, DNS, certificate, email relay, identity provider, payment provider or phone has been used.
> The Docker Compose stack runs end to end on one machine, tested in CI over HTTP and with headless Chrome, but it is
> [insecure on purpose](docs/local-compose.md). Treat this as a working prototype, not as somewhere to put real
> company data. **[What works today, what is experimental, what is unverified](docs/status.md)** is the
> module-by-module table, and [the security model](docs/security-model.md) lists the known weaknesses.

## Try it in two commands (Docker Compose, dev only)

```sh
make e2e-up        # builds and starts the whole stack as compose project "can-e2e" (first build: a few minutes)
make e2e           # runs the end-to-end suite through the Caddy edge (about 2 minutes; needs Go)
```

Then open <http://localhost:8080> (the example site), <http://admin.localhost:8080> (admin UI, user `admin`,
password `dev-only-admin-password`, TOTP secret from `COMPOSE_PROJECT_NAME=can-e2e dev/compose/bootstrap-admin.sh`),
<http://work.localhost:8080/ui/> (work tracker), <http://jobs.localhost:8080> (careers and recruiter UI) and the
other hosts listed in [docs/local-compose.md](docs/local-compose.md). `make e2e-down` removes it.
`make compose-up` starts the same stack as project `companyinacan` without the test overlay (run only one of the
two at a time). Everything there uses public fake secrets and plain HTTP, so never expose it. More paths (local
k3d cluster, a real server) are in [docs/getting-started.md](docs/getting-started.md).

## What is in it

| Module | What it is | Status | Declared RAM | Notes |
|---|---|---|---|---|
| can-core | Control plane: admin UI (password + TOTP, optional OIDC SSO), users and roles, module catalogue, approvals, notifications, events, audit log, personal access tokens, business overview | experimental | 48 MB (soft limit 64) | Installed on k3d in CI; in compose it records modules but does not install them |
| site + forms | Static company site from `company.yaml`, lead-capture gateway (`leads`) | forms: no status marker (treated as ready); the site is rendered by `can` | 15 MB (forms) | What `make up` deploys; run on k3d in CI and in compose. A `contact` section in `company.yaml` links the site to the form ([docs/site-contact.md](docs/site-contact.md)) |
| `work` | Jira-like tracker with API, CLI and web UI | experimental | 28 MB | Installed through can-core on k3d in CI |
| `people` | Directory, org, 1:1s, goals, reviews, time off, web UI | experimental | 30 MB | Sensitive fields sealed; backup and restore run on k3d |
| `jobs` | Requisitions, careers page with JobPosting data, feeds, ATS, recruiter UI | experimental | 30 MB | Installs on k3d in CI. Resume uploads need a virus scanner. Board connectors are stubs |
| `finance` | Parties, invoices (email, PDF, payment reminders), payments, contracts and emailed-link signing, accounting, timesheets, expenses, purchase orders, web UI | experimental | 28 MB | Fake payment provider; the signature is a *simple* electronic signature |
| `provisioning` | Joiner, mover and leaver account provisioning (SCIM) driven by people events | experimental | 20 MB | Installs and reads `people` on k3d, but connector credentials cannot be supplied; connectors tested against fakes only; not in compose ([docs/provisioning.md](docs/provisioning.md)) |
| `files` | Spaces with access lists, versions, share links, scan hook, retention, legal hold, erasure, web UI; local or S3 storage | experimental | 28 MB | No virus scanner ships, so uploads are closed by default (compose switches the requirement off) ([docs/files.md](docs/files.md)) |
| `equity` | Cap table, option grants with vesting, exercises, ledger, holder identity checks | experimental | 16 MB | API and web UI (off by default); installs on k3d in CI. Not legal or tax advice; quantities stored in plaintext |
| `board` | Boards and committees, meetings, quorum, resolutions, minutes | experimental | 20 MB | API only; installs on k3d in CI; not in compose. Not legal advice; secret ballots are unlinkable only after the vote closes |
| `entity` | Legal entities, officers and directors, statutory registers, filings calendar | experimental | 20 MB | Same binary and limits as `board`. Not legal advice; register share counts stored in plaintext |
| `support`, `newsletter`, `analytics` | Wrapped FreeScout, Listmonk, GoatCounter | experimental | 192, 50, 64 MB | Installed, backed up and restored on k3d in CI; no real domain or mail relay |
| `crm` | Wrapped EspoCRM | **not-deployable** | 350 MB | Image needs root, tags unresolved |
| `wiki`, `status` | Wrapped DokuWiki, Uptime Kuma | experimental | 64, 128 MB | Charts checked offline only, never started |

Status values are the ones in each `modules/<id>/module.yaml`. What each module can actually do (install, UI,
backup, restore, erasure) is in [docs/status.md](docs/status.md). `can validate` refuses a `not-deployable` module
and warns for `experimental` ones.

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

The module-by-module table and what each CI workflow proves are in [docs/status.md](docs/status.md). In brief:

| Claim | Evidence | Not verified |
|---|---|---|
| Unit, race and contract tests, licence check, lint | `make test`; CI checks `lint`, `unit`, `contract`, `licences`, `dco` | |
| The stack on Docker Compose, through its edge: site and lead capture, admin login with TOTP, work, people, jobs, finance, equity and files flows, personal access tokens, Apps sign-in, business overview, restart persistence | `make e2e`, [e2e workflow](.github/workflows/e2e.yml) | Kubernetes (TokenReview, NetworkPolicy), TLS, backups |
| The web pages in a real browser engine, at desktop and phone size | `make e2e-browser` (headless Chrome), advisory `browser-e2e` job | https, other browsers, screen readers, real devices |
| On k3d: `make up` phases, module install through can-core, routing, backup and restore | `make-up-e2e`, `dev-cluster`, `modules-k3d`, `module-routing`, `module-backup`, `restore-e2e`, `wrapped-modules`, `wrapped-backup` workflows | using each module there ([status](docs/status.md#modules)) |
| A real server, DNS, ACME certificates, backups to object storage, restore on new hardware | built offline against fakes | **all of it**: needs a provider account; [the runbook](docs/real-server-verification.md) and `can verify --server` are ready for it |

## Footprint: measured against claimed

| Item | Claimed | Measured | Source |
|---|---|---|---|
| k3s + Traefik + CoreDNS + local-path, idle | at most about 600 MB | 510 to 563 MiB node total, on a 16 GB CI runner (not a 2 GB VPS); the pods are 50 to 55 MiB of it | [phase0-ci.md](docs/measurements/phase0-ci.md) |
| can-core, idle | 48 MB declared, 96 MiB limit | 31 MiB RSS (14 MiB anonymous) idle, 33 MiB after the end-to-end suite, 51 to 53 MiB peak with admin logins, in Compose; the cluster figure (49 MiB) is older than the 2026-11-26 reductions and was not repeated | [footprint.md](docs/measurements/footprint.md) |
| In-house services, idle | 15 to 30 MB declared each | 14 to 19 MiB RSS each idle (4 to 6 MiB anonymous), 16 to 24 MiB after the end-to-end suite (5 to 10 MiB anonymous), in Compose (forms 3.2 MiB cgroup working set in CI) | [footprint.md](docs/measurements/footprint.md), phase0-ci.md |
| Whole compose stack (9 services, Caddy and two development stubs) | "under 100 MiB" | **met idle; under load it depends on the machine.** cgroup on a 32-thread development machine: 73 MiB idle (110 before), 120 MiB after the end-to-end suite and 112 a minute later (163 and 153 before). One run on the 4-vCPU CI runner: 68 idle, 92 after the suite (no "before" there). This **leaves out most of the binaries' file pages**, already cached by the image build: resident sets add up to about 208 MiB idle and 242 MiB loaded, nearer what a freshly booted server needs. Rootless Podman locally | [footprint.md](docs/measurements/footprint.md) |
| "Starter stack fits a 2 GB server" | aim | **not established**: the full default bundle has never run together or on a 2 GB machine; the wrapped apps (support, newsletter, analytics) were only started one at a time on k3d. Use 4 GB | [footprint.md](docs/measurements/footprint.md#2-gb-server-are-the-spec-targets-met-by-the-full-default-bundle) |

## Security posture and limitations

Deny by default, hashed or sealed secrets, hash-chained audit log, TOTP on the admin account, separation of
duties on approvals, and one policy layer per sensitive module. Known open issues, stated plainly, are in
[docs/security-model.md](docs/security-model.md): the host rules that keep pods off the server (NetworkPolicy
cannot on k3s, [node-isolation.md](docs/node-isolation.md)) have never run on a real server; someone who has the
admin password can keep guessing authentication codes; equity quantities are stored in plaintext; what is erased in
`finance`, `work`, `provisioning` or a wrapped app comes back with a restore of an older backup; and the compose stack is not safe to expose. Report
vulnerabilities as described in [SECURITY.md](SECURITY.md).

## Observability

Every service has a token-protected metrics listener, `/readyz`, request ids and log redaction, and the compose stack has an
optional Prometheus and Grafana profile: see [docs/observability.md](docs/observability.md).

## Documentation map

Start at [docs/index.md](docs/index.md); [docs/status.md](docs/status.md) says what works. The architecture and roadmap are in [SPEC.md](SPEC.md), the epic
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
make up ARGS="./my-company --plan" IMAGES_FROM=build/images   # real server: docs/getting-started.md (make images first)
```

The project is developed spec-first with [GitHub Spec Kit](https://github.com/github/spec-kit).

## Contributing

Not yet open for outside contributions while the design settles. Ideas and feedback via issues are welcome. The
rules are in [CONTRIBUTING.md](CONTRIBUTING.md) (DCO sign-off, Spec Kit workflow); report vulnerabilities as in
[SECURITY.md](SECURITY.md).

## Licence

[Apache License 2.0](LICENSE). Bundled or wrapped third-party applications keep their own licences; see
[THIRD_PARTY_LICENSES](THIRD_PARTY_LICENSES) and [modules/README.md](modules/README.md).
