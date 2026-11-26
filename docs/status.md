# What works today, what is experimental, what is unverified

As of 2026-11-27. Derived from the code, the module manifests and charts, and the CI workflows, not from other
documents. **No part of this project has run on a real server, no release has been published, and no real phone or
https site has been tried in a browser.** Do not put real company data in it yet.

## In short

| If you want to | Today |
|---|---|
| Try it on your own computer | Works: the Docker Compose stack ([getting-started.md](getting-started.md)). Insecure on purpose |
| Put a company website and a contact form on a server | Built (`make up`) and exercised on a local test cluster; never run on a real server or provider account. The site's `contact` section links to the form's own page ([site-contact.md](site-contact.md)) |
| Run the business modules (tracker, people, finance, ...) on a server | Unverified. No image is published; from a source checkout `make images` builds them all and `make up` ships what `company.yaml` enables. Shown on a test cluster (`work`), never on a real server |
| Rely on backups | No. Backup and restore have passed on a local test cluster only, never against a real server or bucket |

## Modules

Columns:

- **Installs**: can-core installs it on Kubernetes and it becomes Ready, in a CI workflow, on a throwaway k3d
  cluster with images the workflow built or pulled. Ready does not mean the module was used there.
- **Web UI**: its own pages. "Browser" means the headless Chrome suite opens it on the compose stack.
- **Backup / restore**: what has been exercised on k3d. "Unit" means Go tests only.
- **Erasure survives restore**: whether an erasure made in the module is recorded by itself and re-applied by
  `can restore`. "CI" means a restore on k3d showed it; "unit" means collection and replay are built, and only
  Go tests show the replay (CI reads the feed on k3d).

| Module | Marker | Installs | Web UI | Backup | Restore | Erasure survives restore |
|---|---|---|---|---|---|---|
| site | - | CI (published by `make up`) | the public site; browser | rebuilt from your company repository, not backed up | same | no personal data |
| can-core | - | CI | admin UI; browser | CI | CI | its own copies are cleared at restore (unit) |
| `forms` | ready | CI | a hosted form page (browser). The admin Leads page works in no shipped setup (no credential is wired) | CI | CI | CI |
| `work` | experimental | CI | yes; browser | CI | CI | CI (a person's identifier; texts and the audit log are kept) |
| `people` | experimental | CI | yes; browser | CI (restore test) | CI | CI |
| `files` | experimental | CI; **uploads are refused** (no virus scanner, and no supported way to configure one or switch the requirement off) | yes; browser | CI | CI (into a fresh instance, not through `can restore`) | unit |
| `finance` | experimental | CI | yes; browser, with contract signing | CI (restore test) | CI | CI (a customer or vendor; what legal retention keeps stays) |
| `jobs` | experimental | CI; resume uploads are refused (same scanner requirement) | yes; browser, with a careers application | unit | unit | unit |
| `provisioning` | experimental | CI; reads `people` on k3d. **Cannot yet do its job**: connector credentials cannot be supplied | none | unit | unit | **no** |
| `equity` | experimental | CI | yes; browser (off by default; grant approval needs a role no `can` sign-in carries) | unit | unit | unit |
| `board` | experimental | CI | none | unit | unit | unit |
| `entity` | experimental | CI (its own instance and database) | none | unit | unit | unit |
| `analytics` (GoatCounter) | experimental | CI | upstream | CI | CI (can-core's restore operations, not through restic) | **no** |
| `newsletter` (Listmonk) | experimental | CI | upstream | CI | CI (same) | **no** |
| `support` (FreeScout, third-party image) | experimental | CI; mailbox and mail from company.yaml, against an in-memory mail server | upstream | CI | CI (same) | **no** |
| `wiki` (DokuWiki) | experimental | **never started** (image digests are placeholders) | upstream | **blocks all backups** while enabled without the `can-hooks` image | never run | **no** |
| `status` (Uptime Kuma) | experimental | **never started** (same) | upstream | **blocks all backups** while enabled without the `can-hooks` image | never run | **no** |
| `crm` (EspoCRM) | not-deployable | refused by `can validate` and can-core | upstream | **blocks all backups** if recorded and enabled | never run | **no** |

Limits of the erasure column ([backup-restore.md](backup-restore.md#erasure-tombstone-ledger) has all of them):

- The ledger reaches the backup bucket only through the `can` binary at `/usr/local/bin/can` on the server.
  `can up` installs it; that has been run against fakes and a k3d node, not a real server over SSH.
- An erasure made in about the last quarter hour before a server is lost may be missing.
- `finance` erases customers and vendors only, not what it holds about staff (timesheets, expenses). `work`
  replaces the identifier a person appears under, not mentions in texts
  ([specs/056](../specs/056-erasure-finance-work/spec.md)).
- Where a module is marked **no**, what was erased there comes back with a restore of an older backup. Erase it
  again afterwards, by hand.

**No row is verified on a real server.** The images the in-house charts name in `ghcr.io/dodwmd/` do not exist until
a release. From a source checkout, `make images` builds every in-house image and `can-hooks`, and `make up` ships the
ones `company.yaml` enables over SSH ([make-up.md](make-up.md#getting-the-images)).

The Docker Compose stack runs site, forms, can-core, `work`, `people`, `jobs`, `finance`, `equity` and `files`. It
does not install anything: Compose starts the containers. `board`, `entity`, `provisioning` and the wrapped
applications are not in it.

## Platform features

| Feature | Evidence | Not verified |
|---|---|---|
| Admin login (password and TOTP), audit log, approvals, personal access tokens, Apps sign-in, business overview, forms notification email | compose end-to-end suite (`test/e2e`) and, for most, the browser suite; unit tests | real TokenReview in those suites (k3d tests use real tokens) |
| Web pages in a browser | `test/browser`: headless Chrome, desktop and 360 px phone size, with basic accessibility checks | https cookies, separate host names, Firefox, WebKit, screen readers, keyboard-only use, a real phone ([spec](../specs/054-browser-e2e/spec.md)) |
| Users and roles, onboarding wizard, module catalogue | unit tests; the catalogue in compose records state only | enabling a module from the admin UI on a cluster |
| OIDC single sign-on, Web Push, SCIM, CSV import, accounting | unit tests against fakes | any real identity provider, push service, SCIM client or accounting system |
| Contracts signed by emailed link | unit tests; signed in a browser through the compose SMTP sink | a real relay, legal review |
| Outbound email | fake relays and the compose sink | a real relay, deliverability |
| Support inbox mail (`company.yaml`) | `support-mail` on k3d: a message in an in-memory IMAP server becomes a ticket, a reply reaches its SMTP side ([specs/059](../specs/059-support-mailbox/spec.md)) | a real mail provider or relay (the job shows an untrusted relay certificate refused), STARTTLS fetch |
| `make up` phases, backup script, `can verify --server` | `make-up-e2e` on k3d with a fake host and a local restic repository | creating a server, cloud-init, k3s install on a fresh OS, ACME certificates, the five-minute target, `can verify` against a real server |
| Hetzner, DigitalOcean, Cloudflare DNS | fake HTTP APIs | any real account |
| The whole journey with the `can` binary: `make up`, form and mail, modules, backup, destroy, restore | `acceptance`: a CI runner VM as the server over ssh, with stand-ins for certificate, DNS, relay and bucket ([measurements](measurements/acceptance.md)) | a fresh cloud server, ACME, real DNS and mail, a restore onto a second machine, 2 GB |
| Restore onto a new cluster | `restore-e2e` (forms, work, people, finance, with erasures) | a real server or bucket, systemd, restore time, large data |
| Module routing, TLS redirect, edge rate limits | `module-routing` on k3d with Traefik's self-signed certificate | real DNS and certificates, client addresses through ServiceLB |
| Pod-to-node isolation | `node-isolation`: network namespaces, a runner's systemd, k3d | `ufw` on a real server, a reboot, a cloud metadata service |
| Release pipeline | shell self-tests, local reproducible build | it has never run |
| Memory footprint | [measurements](measurements/footprint.md) | a 2 GB or 4 GB server, the full starter bundle together |
| Android | installable web app (PWA) only | any phone |

[real-server-verification.md](real-server-verification.md) is the ordered runbook for closing the real-server
gaps, with `can verify --server` for the automatic checks.

## What CI proves, and when it runs

| Workflow | Runs on | If it fails |
|---|---|---|
| `ci` (`lint`, `unit`, `contract`, `licences`, `dco`) | every pull request and `main` | red; intended to be required, but `main` has no branch protection yet |
| `security` | pull requests, `main`, weekly | red (not required); `dependency-review` is informational |
| `modules-k3d`, `module-backup`, `restore-e2e`, `wrapped-backup`, `node-isolation`, `make-up-e2e`, `support-mail` | pull requests touching their paths, on demand. **Not scheduled** | red (not required) |
| `dev-cluster`, `module-routing`, `wrapped-modules` | pull requests touching their paths, on demand. **Not scheduled** | job marked `continue-on-error`: the run can look green while the job failed |
| `acceptance` | pull requests touching its paths, on demand, weekly | red (not required) |
| `e2e` (jobs `e2e` and `browser-e2e`) | pull requests touching their paths, on demand, nightly | same `continue-on-error` caveat |
| `footprint`, `footprint-budgets` | pull requests touching their paths, on demand. Not scheduled | same `continue-on-error` caveat |
| `flaky-hunt` | nightly and on demand, never on pull requests | red (not required): the unit suite repeated under load to find unreliable tests |

Apart from `ci`, `security`, `e2e` and `flaky-hunt`, nothing runs again after a merge, on `main` or on a timer. So
"CI" above means a run on a pull request passed (the latest seen on 2026-11-27), not that `main` is re-checked.

## Known defects an operator would hit

- **Enabling a module in the admin UI does not check that its image is there** (`can up` and `can apply` do). The
  install then fails; a first install that failed is reported by the backup and does not stop it.
- The release workflow's `release-images.json` ([make-up.md](make-up.md#getting-the-images)) has not been produced by a
  real release yet.
- The admin UI's Leads page has no credential on an install. Messages arrive by notification email (needs a relay)
  or are read through the forms API.
- Most settings of the wrapped modules, and the forms definitions, are not applied from `company.yaml`
  ([module-wiring.md](module-wiring.md#settings)).
- Security weaknesses are listed in [security-model.md](security-model.md#known-residual-risks).

## Decisions waiting on the maintainer

| Decision | Why it matters | Where |
|---|---|---|
| **Protect the repository.** Apply branch protection, the `release-tags` ruleset and the `release` environment (`scripts/gh-protect-main.sh` prints the calls; `--apply` runs them) | On 2026-11-26 `main` was unprotected, with no ruleset and no environment: the five "required" checks are not enforced | [maintainers.md](maintainers.md) |
| **Enable repository security settings**: private vulnerability reporting, secret scanning with push protection, Dependabot alerts and updates, dependency graph, approval for outside contributors' workflow runs | On 2026-11-26 private reporting, secret scanning and Dependabot security updates were off; [SECURITY.md](../SECURITY.md) sends reporters to a button that is not there | [maintainers.md](maintainers.md) |
| **Publish a first release**, including the `can-hooks` image | Without it module images have to be built from a source checkout (`make images`). Make the new packages public | [releasing.md](releasing.md) |
| **Provide a real server** (and a test bucket, domain and mail relay) for one verification run | Nearly every "not verified" line above depends on it; it costs money (the runbook estimates about US$5 plus a domain) and needs accounts | [real-server-verification.md](real-server-verification.md) |
| **Keep, replace or drop the third-party FreeScout image** (PikaPods) | It is a supply-chain dependency on another party's build, amd64 only, with their patch delay; nothing updates pinned digests automatically | [specs/041](../specs/041-wrapped-modules-live/spec.md) |
| **In-house e-signature or Documenso** | SPEC planned Documenso; finance has its own *simple* signature instead. No legal review; Documenso's licence terms were not checked | [specs/049](../specs/049-contract-esign/spec.md) |
| **Accept or change the sign-in guessing risk** | Someone who has the password can keep guessing authentication codes (per-address limits only). A per-account limit was tried and rejected because it caused other problems | [security-model.md](security-model.md#known-residual-risks), [specs/005](../specs/005-can-core-mvp/deployment.md) |
| **CSV import behaviour** (no web page; work import needs `work.admin`; imported active people start no joiner run) | Flagged for confirmation when it was merged | [PR 65](https://github.com/dodwmd/companyinacan/pull/65) |
| **Outside contributions**: open or not, and DCO only or a CLA | The repository is public; SPEC says any CLA must come before the first external pull request | [SPEC.md](../SPEC.md#12-open-source-plan) |
| **Open product questions**: the name; payment links only or a billing module | Listed as open in SPEC | [SPEC.md](../SPEC.md#11-risks--open-questions) |
