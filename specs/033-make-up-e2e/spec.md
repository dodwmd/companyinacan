# Feature Specification: `can up` produces a live company (Epic E24)

**Feature Branch**: `epic/E24-make-up-e2e`
**Created**: 2026-11-25
**Status**: Implemented offline and verified on k3d in CI; the server-provisioning half stays human-gated.
**Depends on**: E02 (base manifests, k3d), E03 (can-core), E04 (site, can-static), E05 (forms),
E06 (providers, bootstrap, tunnel), E08 (backup kit and timer), E20 (cloud providers), E24 closes the gap
that `make up` stopped at bare k3s.

## Problem

SPEC.md section 6.1 promises that `make up` ends with a live site, a working contact form and an admin
login, in about five minutes. Until now `can up` provisioned a server, installed k3s, fetched the
kubeconfig and printed DNS records, then said "can-core install arrives in epic E03". Nothing deployed
the platform, there was no way to obtain the container images from a source checkout, no admin
credentials were created, the site was never published and the recovery kit was never produced.

## User scenarios

### US1 (P1): one command from a company.yaml to a live company

As an operator with a `company.yaml` and an SSH-reachable server (or a Hetzner/DigitalOcean token) I run
`make up IMAGES_FROM=build/images` (after `make images`) and end with: the site served on a real URL,
the admin UI reachable, the admin username and a one-time password and TOTP secret, the contact form
creating leads, and (if I configured a bucket) the backup timer installed and the recovery kit in my hands.

**Acceptance**
1. Given a fresh server, when `can up` finishes, then `can status` exits 0 and reports can-core, the site
   and forms available.
2. The summary shows the site URL, the admin URL, the admin username, the password and the TOTP secret
   once, each only in a terminal after a confirmation, or with `--print-secrets`.
3. `can up --output json` prints one JSON document; it contains no secret unless `--print-secrets` is set.

### US2 (P1): safe to interrupt and repeat

As an operator whose laptop slept in the middle of `can up`, I run the same command again and it resumes.

**Acceptance**
1. Every phase is checkpointed in `.can/state.json`; a re-run skips the work that is not idempotent
   (image upload of an identical set, admin creation, backup installation) and re-applies the rest.
2. A re-run never wipes data: no volume, ConfigMap, Secret or admin is deleted or replaced, with one
   exception stated in US3.
3. Two `can up` runs for the same company at once: the second is refused (operation lock).

### US3 (P1): secrets stay secret

1. The admin password is generated locally, sent to the in-cluster `can-core admin bootstrap` on stdin
   (never argv, never a Kubernetes Secret that outlives the command) and shown once.
2. If a run created the account but stopped before showing the credentials, the next run issues a new
   password and TOTP secret (nobody holds the old ones). Once shown, they are never touched again.
3. The recovery kit is shown once with an explicit confirmation, or written encrypted with `--kit-out`;
   it is never written inside a git working tree and never to `.can/`.
4. No secret reaches argv, logs, `.can/state.json`, error messages (redactor) or a script sent over SSH.

### US4 (P1): images without a registry, or pinned by digest

As a contributor on a source checkout, `make images` builds can-core, can-static and can-forms with podman
or docker, and `can up --images-from` ships the tarballs over SSH after verifying sha256 on both ends. As
an operator on a release, `can up --release-manifest deploy/release-images.json` uses digest-pinned
references produced by the release pipeline. A tag-only or placeholder manifest is refused.

### US5 (P2): `can status` and `can down`

`can status` also reports the stack (phases recorded, deployments available) and degrades when the stack
is down. `can down` keeps its semantics and additionally removes the backup timer, the unit files and the
restic credentials from an `ssh` server it leaves behind.

### US6 (P2): proof on a real cluster

A CI workflow runs the same post-provision phases against k3d and asserts the user-visible outcomes
(can-core healthy, admin login with generated password + computed TOTP, site content through Traefik,
a form POST creating a lead, the kit parsing, no secrets on disk, an idempotent re-run) and reports timings.

## Requirements

- **FR-001** `can up` runs the phases in contracts/cli.md, in that order, each bounded by a timeout.
- **FR-002** Offline checks (kubectl present, images resolvable and verified, backup flags consistent, kit
  destination available) run before any server is created or paid for.
- **FR-003** All Kubernetes changes go through one `Kube` seam (kubectl); secrets only on stdin.
- **FR-004** The platform base, can-core and the site manifests are embedded in the binary (`deploy/stack.go`).
- **FR-005** The site is built in-process (`can site build` code), packed deterministically and published
  through a ConfigMap; an unchanged site never restarts the pod.
- **FR-006** Traffic: https on the sslip.io instant hosts when the pre-check passes, otherwise plain HTTP on
  the IP for the public site only; the company domain is routed with TLS only when its DNS is live.
- **FR-007** `/_forms` is routed in the forms namespace with a higher priority than the site route and the
  prefix stripped.
- **FR-008** The firewall and ports configured by the bootstrap script are unchanged.
- **FR-009** `can up --plan` lists every phase and changes nothing; `--provision-only` keeps the old behaviour.
- **FR-010** Exit codes and JSON as in contracts/cli.md; failures name the phase, how to look, how to resume.
- **FR-011** No destructive default: `can up` never deletes data; `can down` still needs `--yes`.

## Out of scope / known gaps

- Provisioning a real server and the real five-minute budget (human-gated, docs/make-up.md).
- The backup script expects can-core to write dumps to `/var/lib/can/dumps` on the host; the can-core
  Deployment does not mount that hostPath yet, so a backup run exits 10 (marker missing) until it does.
- Image upload through `k3s ctr images import` and `apt install restic` are written but untested on a real host.
- Email relay, custom-domain DNS propagation (explicitly outside the budget in SPEC 3.4).
- `deploy/release-images.json` is generated by `scripts/gen-release-images.sh`; wiring it into
  `release.yml` is left to the release owner (docs/releasing.md).
