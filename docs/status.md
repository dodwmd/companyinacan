# What works today, what is experimental, what is unverified

As of 2026-11-26. Derived from the code, the module manifests and charts, and the CI workflows, not from other
documents. **No part of this project has run on a real server, no release has been published, and no web page has
been checked in a real browser.** Do not put real company data in it yet.

## In short

| If you want to | Today |
|---|---|
| Try it on your own computer | Works: the Docker Compose stack ([getting-started.md](getting-started.md)). Insecure on purpose |
| Put a company website with a contact form on a server | Built (`make up`) and exercised on a local test cluster; never run on a real server or provider account |
| Run the business modules (tracker, people, finance, ...) on a server | Not yet. Their container images are not published, `make up` ships only can-core, the site and forms, and some modules do not start when can-core installs them (below) |
| Rely on backups | No. Backup and restore have passed on a local test cluster only, never against a real server or bucket |

## Modules

Columns:

- **Installs**: can-core installs it on Kubernetes and it starts. "CI" means a workflow did this on a throwaway
  k3d cluster with images it built or pulled itself.
- **Web UI**: its own pages. "Upstream" is the wrapped application's own interface.
- **Backup / restore**: what has been exercised on k3d. "Unit" means Go tests only.
- **Erasure survives restore**: whether `can restore` re-applies a recorded erasure by itself. Every "yes" also
  needs the operator to record each erasure with `can backup ledger-add`: nothing records it automatically.

| Module | Marker | Installs | Web UI | Backup | Restore | Erasure survives restore |
|---|---|---|---|---|---|---|
| site | - | CI (published by `make up`) | the public site | rebuilt from your company repository, not backed up | same | no personal data |
| can-core | - | CI | admin UI | CI | CI | its own copies are cleared at restore (unit) |
| `forms` | ready | CI | public form; leads in the admin UI | CI | CI | **no**: no purge by subject |
| `work` | experimental | CI | yes | CI | CI | **no** |
| `people` | experimental | CI | yes | CI (restore test) | CI | CI |
| `files` | experimental | CI; **uploads are refused** (no virus scanner, and no supported way to configure one or switch the requirement off) | yes | CI | CI (into a fresh instance, not through `can restore`) | unit only |
| `finance` | experimental | CI (routing test) | yes | unit | unit | **no** |
| `jobs` | experimental | **No: it exits at start** (`company.name` and `company.base_url` are required and nothing sets them) | yes | unit | unit | unit only |
| `provisioning` | experimental | **No: it exits at start** (`CAN_PROVISIONING_DIRECTORY_TOKEN` is required and nothing sets it) | none | unit | unit | **no** |
| `equity` | experimental | **No: no Helm templates** | none | its own `backup` command, unit | unit | unit only (command exists) |
| `board` | experimental | **No: no Helm templates** | none | unit | unit | unit only |
| `entity` | experimental | **No: no Helm templates** | none | unit | unit | **no** |
| `analytics` (GoatCounter) | experimental | CI | upstream | CI | CI (can-core's restore operations, not through restic) | **no** |
| `newsletter` (Listmonk) | experimental | CI | upstream | CI | CI (same) | **no** |
| `support` (FreeScout, third-party image) | experimental | CI | upstream | CI | CI (same) | **no** |
| `wiki` (DokuWiki) | experimental | **never started** (image digests are placeholders) | upstream | **blocks all backups** while enabled | never run | **no** |
| `status` (Uptime Kuma) | experimental | **never started** (same) | upstream | **blocks all backups** while enabled | never run | **no** |
| `crm` (EspoCRM) | not-deployable | refused by `can validate` and can-core | upstream | **blocks all backups** if recorded and enabled | never run | **no** |

Where a restore meets a recorded erasure it cannot replay, it stops until you pass `--accept-unerased` and erase
the data by hand ([backup-restore.md](backup-restore.md#restore-can-restore---kit)).

**No row is verified on a real server.** On a real cluster today, only the can-core, site and forms images can be
delivered (`make images`, shipped over SSH). The other in-house charts name images in `ghcr.io/dodwmd/` that do not
exist until a release. In CI they work because the workflow builds them and imports them into k3d. The module charts
ask for tag `0.1.0`, so a first release with another version number would not satisfy them.

The Docker Compose stack runs site, forms, can-core, `work`, `people`, `jobs`, `finance`, `equity` and `files`. It
does not install anything: Compose starts the containers. `board`, `entity`, `provisioning` and the wrapped
applications are not in it.

## Platform features

| Feature | Evidence | Not verified |
|---|---|---|
| Admin login (password and TOTP), audit log, approvals, personal access tokens, Apps sign-in, business overview, forms notification email | compose end-to-end suite (`test/e2e`); unit tests | a real browser; real TokenReview in that suite (k3d tests cover it) |
| Users and roles, onboarding wizard, module catalogue | unit tests; the catalogue's enable and disable in compose records state only | enabling a module from the admin UI on a cluster |
| OIDC single sign-on, Web Push, SCIM, CSV import, signing contracts by emailed link, accounting | unit tests against fakes; the invoice flow runs in compose | any real identity provider, push service, SCIM client, accounting system, legal review |
| Outbound email | fake relays and the compose sink | a real relay, deliverability |
| `make up` phases, backup script | `make-up-e2e` on k3d with a fake host and a local restic repository | creating a server, cloud-init, k3s install on a fresh OS, ACME certificates, the five-minute target |
| Hetzner, DigitalOcean, Cloudflare DNS | fake HTTP APIs | any real account |
| Restore onto a new cluster | `restore-e2e` (forms, work, people) | a real server or bucket, systemd, restore time, large data |
| Module routing, TLS redirect, edge rate limits | `module-routing` on k3d with Traefik's self-signed certificate | real DNS and certificates, client addresses through ServiceLB |
| Pod-to-node isolation | `node-isolation`: network namespaces, a runner's systemd, k3d | `ufw` on a real server, a reboot, a cloud metadata service |
| Release pipeline | shell self-tests, local reproducible build | it has never run |
| Memory footprint | [measurements](measurements/footprint.md) | a 2 GB or 4 GB server, the full starter bundle together |
| Android | installable web app (PWA) only | any phone |

## What CI proves, and when it runs

| Workflow | Runs on | If it fails |
|---|---|---|
| `ci` (`lint`, `unit`, `contract`, `licences`, `dco`) | every pull request and `main` | red; intended to be required, but `main` has no branch protection yet |
| `security` | pull requests, `main`, weekly | red (not required); `dependency-review` is informational |
| `module-backup`, `restore-e2e`, `wrapped-backup`, `node-isolation`, `make-up-e2e` | pull requests touching their paths, on demand | red (not required) |
| `e2e` | the same, and nightly | job marked `continue-on-error`: the run can look green while the job failed |
| `dev-cluster`, `module-routing`, `wrapped-modules`, `footprint`, `footprint-budgets` | pull requests touching their paths, on demand (some scheduled) | the same `continue-on-error` caveat |

Path-filtered workflows do not run again on `main` after a merge. "CI" above means the latest runs seen on
2026-11-26 passed, not that every future change is gated on them.

## Known defects an operator would hit

- `jobs` and `provisioning` do not start when installed by can-core (table above).
- A dump run made before the kubelet has mounted a newly created `company` ConfigMap leaves out `company.yaml`
  and still completes; `can restore` cannot use that set.
- Erasures are not recorded in the restore ledger automatically.
- `make up` does not copy the `can` binary to the server; the backup script's erasure-ledger upload needs it at
  `/usr/local/bin/can` ([backup-restore.md](backup-restore.md#commands)).
- `company.yaml` module `settings` are not mapped onto module configuration
  ([module-wiring.md](module-wiring.md)), so for example finance shows the company as its web address.
- Security weaknesses are listed in [security-model.md](security-model.md#known-residual-risks).

## Decisions waiting on the maintainer

| Decision | Why it matters | Where |
|---|---|---|
| **Protect the repository.** Apply branch protection, the `release-tags` ruleset and the `release` environment (`scripts/gh-protect-main.sh` prints the calls; `--apply` runs them) | On 2026-11-26 `main` was unprotected, with no ruleset and no environment: the five "required" checks are not enforced | [maintainers.md](maintainers.md) |
| **Enable repository security settings**: private vulnerability reporting, secret scanning with push protection, Dependabot alerts and updates, dependency graph, approval for outside contributors' workflow runs | Private reporting, secret scanning and Dependabot security updates were off; [SECURITY.md](../SECURITY.md) sends reporters to a button that is not there | [maintainers.md](maintainers.md) |
| **Publish a first release**, including the `can-hooks` image, and choose its version | Without it module images cannot be pulled, and `wiki`, `status` and `crm` block backups. Charts expect `0.1.0`. Check the new packages are public | [releasing.md](releasing.md) |
| **Provide a real server** (and a test bucket, domain and mail relay) for one verification run | Nearly every "not verified" line above depends on it; it costs money and needs accounts | [make-up.md](make-up.md#timing-a-real-server-human-gated-epic-e00), [backup-restore.md](backup-restore.md#measuring-restore-time-human-gated) |
| **Keep, replace or drop the third-party FreeScout image** (PikaPods) | It is a supply-chain dependency on another party's build, amd64 only, with their patch delay; nothing updates pinned digests automatically | [specs/041](../specs/041-wrapped-modules-live/spec.md) |
| **In-house e-signature or Documenso** | SPEC planned Documenso; finance has its own *simple* signature instead. No legal review; Documenso's licence terms were not checked | [specs/049](../specs/049-contract-esign/spec.md) |
| **Accept or change the sign-in guessing risk** | Someone who has the password can keep guessing authentication codes (per-address limits only). A per-account limit was tried and rejected because it caused other problems | [security-model.md](security-model.md#known-residual-risks), [specs/005](../specs/005-can-core-mvp/deployment.md) |
| **CSV import behaviour** (no web page; work import needs `work.admin`; imported active people start no joiner run) | Flagged for confirmation when it was merged | [PR 65](https://github.com/dodwmd/companyinacan/pull/65) |
| **Outside contributions**: open or not, and DCO only or a CLA | The repository is public; SPEC says any CLA must come before the first external pull request | [SPEC.md](../SPEC.md#12-open-source-plan) |
| **Open product questions**: the name; payment links only or a billing module | Listed as open in SPEC | [SPEC.md](../SPEC.md#11-risks--open-questions) |
