# `make up`, `make down`, `make status`

`make up` turns a `company.yaml` and a server (yours over SSH, or a Hetzner or DigitalOcean account) into
a live company: k3s, the platform, can-core, your site, the contact form, an admin login, optionally the
backup timer and its recovery kit. It is a thin wrapper over `can up|down|status`; extra flags go in `ARGS`.
Run it in the directory that holds `company.yaml`, or pass the directory: `make up ARGS=./my-company`.

```sh
make images                       # once, from a source checkout: build the three images into build/images
make up ARGS=--plan IMAGES_FROM=build/images     # show every phase, change nothing
make up IMAGES_FROM=build/images  # provision, deploy, print the summary
make status                       # exit 0 only when the server and the stack are up
make down ARGS=--plan             # show what would be removed
make down ARGS=--yes              # tear down (destructive, needs --yes)
```

`can up` needs `kubectl` on your machine (it applies the company through an SSH tunnel to the k3s API; port
6443 stays closed on the server). It checks for it, for the images and for the backup settings **before** it
creates or pays for anything.

## Getting the images

The three images (`can-core`, `can-static`, `can-forms`) are built and pinned at release time. A source
checkout has two ways to get them; pass exactly one:

| Way | Command | What happens |
|---|---|---|
| Build locally, ship over SSH | `make images` then `make up IMAGES_FROM=build/images` | `scripts/build-images.sh` builds with podman or docker (`ENGINE=`), writes `build/images/*.tar` and `images.json` with the sha256 of each. `can up` re-checks the checksums, uploads over SSH, checks them again on the server with `sha256sum` and imports them into k3s (`k3s ctr -n k8s.io images import`; the tarballs stay in `/var/lib/rancher/k3s/agent/images`). No registry is involved. Local tags are used (`can-core:dev`), the forms image gets the tag its chart expects. |
| Pull by digest from a release | `can up --release-manifest deploy/release-images.json` (also the default when that file exists) | `deploy/release-images.json` lists every image as `repository@sha256:<digest>`; the server pulls them from GHCR. A tag, a placeholder or a malformed digest is refused. The file is produced by the release pipeline with `scripts/gen-release-images.sh`. |

With neither, `can up` stops before provisioning and says so. `make images` builds your working tree on the
base-image digests the registry serves today (the Dockerfiles keep their release placeholders); it is a
development path, not a supply-chain guarantee. Release images are signed and attested (docs/releasing.md).

## What `make up` does

```
 operator machine                                          server
 ----------------                                          ------
 0 preflight  company.yaml, DNS token, kubectl, images, backup flags      (nothing exists yet)
 1 provision  ------------------ ssh / cloud API ---------> create (hetzner, digitalocean) or probe (ssh)
              ------------------ bootstrap script -------> k3s, firewall 22/80/443, swap   [existing, unchanged]
 2 access     <----------------- kubeconfig over ssh ----- .can/kubeconfig; ssh -L 16443 -> 127.0.0.1:6443
 3 base       -- kubectl apply deploy/base -------------> namespaces, network policies, RBAC, admission policy, Traefik
 4 images     -- tarballs over ssh, sha256 checked ------> k3s containerd   (or: the server pulls by digest)
 5 company    -- ConfigMap can-system/company ----------> company.yaml (+ forms, the default bundle)
 6 core       -- can-core Deployment, wait -------------> can-core, 1 pod, SQLite on a PVC
 7 admin      -- kubectl exec ... admin bootstrap ------> admin account; password on stdin, never argv
 8 site       -- build locally, ConfigMap site-content -> site pod (can-static) unpacks it
 9 routes     -- IngressRoutes -------------------------> site, admin, same-origin /_forms (forms module installed by can-core)
10 backup     -- files over ssh (only with --backup-*) -> pinned restic, dumps dir, timer, repository initialised
11 ready      -- kubectl wait Available ----------------> can-core, site, forms
12 dns / probe / summary   Cloudflare records or printed records; URL probe; credentials and kit shown once
```

The summary prints the site URL, the admin URL, the admin username and the one-time password and TOTP secret
(scan the secret or the `otpauth://` URI into an authenticator app), the phase timings, and next steps. The
admin password and the recovery kit are shown once, each after a confirmation prompt, and only in a terminal.

### Instant URL and DNS

If `<ip-dashed>.sslip.io` resolves to the server and the Let's Encrypt limits allow it, the site is served
with real TLS on `https://<ip-dashed>.sslip.io` and the admin UI on `https://admin.<ip-dashed>.sslip.io`.
Otherwise the public site is served over plain HTTP on the IP and **the admin UI is not exposed**: reach it
with the `kubectl port-forward` command the summary prints. Your own domain is routed (with TLS) only once its
records resolve: automatically with `dns.provider: cloudflare`, otherwise add the printed records and run
`can up --domain-live` (this avoids requesting certificates for names that do not resolve yet). DNS
propagation is not part of the five-minute budget.

The records are the domain, `admin`, `help` and one per module host that company.yaml puts on the
internet (`work.<domain>`, `careers.<domain>`; see [`module-routing.md`](module-routing.md)). `can
status` lists them with where each points now.

On a TLS install `can up` redirects plain-HTTP page loads to HTTPS and rate-limits the admin
sign-in and the public site per client address (`--no-edge-limits` behind your own proxy).

### Secrets

- The admin password is generated on your machine, sent to `can-core admin bootstrap` on stdin, and shown
  once. It is never an argument, a log line, a Secret or in `.can/state.json`.
- Without a terminal nothing secret is printed. For CI use `--print-secrets` (credentials and kit in the
  output, or in `--output json`); without it a non-terminal run says the credentials were **not** shown and the
  next run issues new ones. Once shown (terminal and confirmed, or `--print-secrets`), they are never changed
  by a later run.
- The recovery kit comes with `--backup-endpoint URL --backup-bucket NAME --backup-credentials-file FILE`
  (KEY=VALUE with `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`). It is shown once in a terminal
  (`Type 'show'`, then `'saved'`) or written encrypted with `--kit-out FILE --kit-passphrase-file FILE`.
  It is never written inside a git working tree or to `.can/`, and a run that has no way to deliver it stops
  before provisioning.
- The first backup needs a dump run: can-core makes one about 10 minutes after it starts, then nightly at 02:30
  UTC. To back up at once, on the server:
  `sudo k3s kubectl -n can-system exec deploy/can-core -- /can-core dump-request --wait 5m`, then
  `sudo systemctl start can-backup.service` and `journalctl -u can-backup`.

### The default bundle

If `company.yaml` does not mention the forms module, `can up` enables it in the cluster so the contact form
works. The file on disk is not changed and the summary tells you to add `forms: {}` under `modules:`; if you
do not, a later `can apply` disables it again (its data is kept). `--no-default-bundle` turns this off; an
explicit `forms: {enabled: false}` is always respected.

## Safe to repeat

Run `make up` again after any failure or interruption. Every phase is recorded in `.can/state.json`
(`launch.phases`); `can up` never deletes data (no volume, ConfigMap or admin is replaced), and two runs at
once are refused (`another can up or can down is already running`).

| Interrupted or failed at | What `can up` does next time |
|---|---|
| preflight | nothing exists yet; fix the message and run again |
| provision (server created, k3s not ready) | adopts the server by its tags or recorded id, re-runs the bootstrap only if k3s is not at the pinned version |
| access (tunnel) | opens it again; if local port 16443 is busy, close the other tunnel |
| base | `kubectl apply` again (converges) |
| images | uploads again unless the same set (fingerprint) was recorded as loaded |
| company, routing, core, site, routes | applied again (converges; an unchanged site does not restart the pod) |
| admin, before the account was created | creates it |
| admin, created but the credentials were never shown | issues a new password and TOTP secret (nobody has the old ones) and shows them |
| admin, credentials already shown | leaves the account alone and says so; lost them: `kubectl -n can-system exec -i deploy/can-core -- /can-core admin reset-password --rotate-totp` (new password on stdin) |
| backup, timer installed, kit not shown | rebuilds the kit from the password already on the server and shows it |
| backup, kit already shown | nothing, unless the repository, the bucket credentials or the password on the server differ from that kit: then the install is redone and the new kit is shown (rotated bucket keys are written to the server) |
| backup installed, password file on the server missing or unreadable | stops without changing anything; put the password from your kit back |
| ready / dns / summary | repeated |

A failure prints `phase "<id>" failed: <cause>`, a `look:` line with the commands to inspect, and a `resume:`
line. Every wait has a bound (rollouts 3 min, k3s CRDs 3 min, module install 5 min, public probe 60 s).

## `make down`

`make down` runs `can down`, which depends on `provider.type` in `company.yaml`: `local` deletes the k3d
cluster like `can dev down` (no `--yes` needed, `ARGS=--plan` prints the k3d commands); `ssh`, `hetzner` and
`digitalocean` destroy what `make up` created, plus the Cloudflare records it created, and need `--yes`. For
`ssh`, where the machine stays, it first removes the backup timer, its unit files and the restic credentials
(`/etc/can/backup.d`) from the server, and stops if it cannot. `make server-down` is an alias.

## `make status`

Exit 0 only when the provider reports ready **and**, for a company deployed by `can up`, `phases` (the last run
finished), `can-core`, `site` and `forms` have available replicas. The stack checks are read through the SSH
tunnel; if it cannot be opened that is a failed check, not a silent pass.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | success (`status`: ready) |
| 1 | failure: invalid `company.yaml`, preflight failed, a phase failed, API or SSH error, status not ready |
| 2 | usage: unknown flag, too many arguments, `down` without `--yes` |
| 3 | not implemented: provider `local` or `static`, or the `provider-native` DNS plugin |

The full CLI contract, including `--output json`, is in `specs/033-make-up-e2e/contracts/cli.md`.

## Troubleshooting

- `kubectl was not found in PATH`: install kubectl; nothing was provisioned.
- `no container images to deploy`: run `make images` and pass `IMAGES_FROM=build/images`, or use a release manifest.
- `image X: tarball ... has sha256 ... but images.json records ...`: rebuild with `make images`.
- `cannot reach the cluster API through the SSH tunnel`: the server is provisioned; check `ssh` to it and that
  local port 16443 is free, then run `can up` again.
- `phase "core" failed`: `kubectl --kubeconfig .can/kubeconfig -n can-system logs deploy/can-core` (open the tunnel
  with `can status` or `ssh -N -L 16443:127.0.0.1:6443 user@host` first).
- `phase "routes" failed ... forms module was not installed`: can-core installs it on its next reconcile and pulls
  its image; look at can-core's log and `kubectl -n mod-forms get pods`.
- `restic is not installed on the server`: install restic 0.17 or newer there.
- `preflight failed: ram: ...`: the server is too small; use at least 2 GB.
- `ssh ...: exit status 255`: SSH could not connect; check `host`, `user`, `key_file`, and that you can `ssh user@host`
  yourself. The first connection trusts the host key (cloud servers keep it in `.can/known_hosts`).
- `secret reference "env:HCLOUD_TOKEN" is not usable`: export the token.
- Hetzner and DigitalOcean: look at `/var/log/can-bootstrap.log` on the server if the bootstrap wait times out.
- DigitalOcean: `a droplet named ... was not created by can`: rename or delete it, or set `provider.options.server_name`.
- Cloudflare: `cannot read DNS records in the zone`, `no Cloudflare zone found`, `already exists ... not created by can`:
  see [providers.md](providers.md).
- A module that does not update: `can status` warns about modules whose last install or upgrade failed, with
  can-core's error (a failed upgrade is rolled back, so the module keeps running the previous release).
- `.can/state.json` says something that is no longer true (server deleted by hand): `can status` reports
  `absent`; remove `.can/` to start clean.

## The five-minute budget

SPEC.md section 3.4 sets the target. Honest status: **the end-to-end time on a real server has not been
measured**; that run needs a fresh cloud account and stays human-gated (below). What is measured is the part
that does not depend on a cloud provider: the in-cluster phases, on k3d on a GitHub-hosted runner.

Budget by phase, with what is known:

| Step | SPEC target | Measured | Notes |
|---|---|---|---|
| Create VM (provider API) | 30-60 s | not measured | human run |
| Boot + k3s install (bootstrap script) | about 60 s | not measured | human run |
| images: upload and import (`--images-from`) | in parallel with k3s in the SPEC | not measured on a server; CI imports with `k3d image import` (see below) | the tarballs are tens of MiB; upload time depends on your uplink. A release manifest has no upload, the server pulls |
| base + company + routing + core + admin + site + routes + ready | about 30 s deploy | see the k3d table | |
| Instant URL | immediate | n/a | needs the server's real address |

### Measured on k3d in CI (workflow `make-up-e2e`, advisory)

Two runs of the `make-up-e2e` workflow on a GitHub-hosted `ubuntu-latest` runner (`launch.Run`, the same code
`can up` runs after the server exists; images imported with `k3d image import`; a fake host for the backup
timer; these two runs predate E08b, which runs the backup for real, see specs/037). Times are the per-phase durations the run reports:

| Phase | Run 1 (s) | Run 2 (s) | What it covers |
|---|---|---|---|
| base | 1.8 | 1.6 | wait for k3s CRDs, apply deploy/base |
| images | 7.9 | 7.9 | import three tarballs into k3d (on a real server: upload over SSH + import; not measured) |
| company | 0.1 | 0.1 | ConfigMap |
| core | 4.2 | 5.4 | can-core Deployment available |
| admin | 0.5 | 1.0 | `admin bootstrap` in the pod |
| site | 2.5 | 2.0 | build, ConfigMap, site pod available |
| routes | 4.1 | 11.0 | IngressRoutes, then wait for can-core to install the forms module (its reconcile interval is 15 s) and for its pod, then the `/_forms` route |
| backup, ready | 0.4 | 0.4 | fake host; `kubectl wait` |
| **`launch.Run` total** | **21.5** | **29.5** | |

Outside that table, on the same runner: creating the k3d cluster and applying the base took 31 s, and
`make images` (a cold Go build of three images, not part of a user's `make up` when images are prebuilt or
pulled by digest) took 88 s. The whole test, including the assertions (admin login, site, a form post that
waits out the form's 3 s minimum fill time, kit, idempotent re-run), ran in 41 s.

These numbers include image pulls of Traefik and k3s system images done when the cluster was created (outside
the table), exclude VM creation and the k3s install, and run on a shared runner; treat them as a floor for the
deploy part, not a promise for a fresh server.

### Timing a real server (human-gated, epic E00)

Do not run this from CI. With a fresh Hetzner account:

1. Create a throwaway project and API token; `export HCLOUD_TOKEN=...`. Use a new company directory with
   `provider: {type: hetzner}` and an SSH key not used elsewhere.
2. Build the images (`make images`) and a warm Go build (`make build`) first, so neither is counted.
3. Note the wall clock, then run `time make up IMAGES_FROM=build/images ARGS="--output json"` and keep the JSON:
   `phases[].seconds` and `total_seconds` give the breakdown. Note when the server was created and running from
   the progress lines.
4. Record the server type and location, whether `image_tarballs` or a release manifest was used, and the
   upload time of the images (`images` phase).
5. On the server, record idle memory two minutes after the run: `free -m`, `systemctl status k3s`.
6. Run `make up` again and record the idempotent re-run time.
7. `make down ARGS=--yes`; confirm in the provider console that the server, firewall and key are gone.
8. Repeat three times and report median and worst case in `specs/000-*/measurements.md`.

## Security notes

- **The admin UI is public** on `https://admin.<host>` in https mode. It is protected by a password, mandatory
  TOTP and login throttling. Behind Traefik can-core throttles per client address (`CAN_TRUST_PROXY=true`, the
  last `X-Forwarded-For` entry Traefik appends): repeated failures lock that (username, address) pair, so an
  attacker can lock out the admin only from their own address. Add an IP allowlist (SPEC.md section 3.1) if the
  admin works from fixed addresses. These locks are per address: someone who has the password and many
  addresses can still guess codes. Twenty failed sign-ins on an account within a day are recorded as
  `admin.login.many_failures`. In the plain-HTTP fallback the admin UI is not routed at all.
- **HTTP to HTTPS.** In https mode a route on the `web` entry point (`can-system/https-redirect`)
  redirects `GET` and `HEAD` with a temporary redirect; other methods get 404, so a client that sends a
  password or token over plain HTTP fails instead of carrying on. Certificates use TLS-ALPN on 443 and are
  unaffected. In the plain-HTTP fallback the route is removed. HSTS is sent by module routes, not by the
  site or admin routes.
- **Manifests are embedded, not signature-verified at run time.** The platform base, can-core and site manifests
  are compiled into the `can` binary; the binary's integrity is what the release signatures and checksums
  cover. A modified `can` (or a modified `deploy/release-images.json`, whose digests are only as trustworthy
  as the file you got it from: verify it against the release's signed checksums) can deploy any image as a
  privileged workload. Download `can` and the manifest from the release and verify them (docs/releasing.md).
- **Re-running does not overwrite the company you edited in the cluster.** `can up` remembers the digest of the
  `company.yaml` it last pushed. If the ConfigMap in the cluster differs from that (changed in the admin UI or by
  `can apply`), the `company` phase stops and asks for `--force-company`; an identical document is not
  re-applied at all.
- **The company guard has a window.** `can up` compares the cluster's `company.yaml` with what it last pushed just before applying; an edit made in the admin UI in the instant between that read and the apply can still be overwritten. Do not edit the company in the UI while `can up` runs.
- **`--output json` never carries secrets without `--print-secrets`**, even in a terminal; in that case the
  credentials are *not* marked as shown, and the next run issues new ones.
- `kubectl` is always pinned to this company's kubeconfig **and** its context, never to your current context.
- Image tarballs are streamed over SSH after a checksum pass, so memory use does not grow with their size.

## Known gaps

- **Backups** work as described in [backup-restore.md](backup-restore.md) and are verified on k3d with a local
  restic repository only. Not verified: a real S3 bucket, a real server (systemd timer, k3s paths) and a restore
  onto a blank host (`can restore` has only been run on k3d in CI). wiki, crm and status stop every backup while
  enabled or recorded as failed, because the `can-hooks` image is not published. Disabling the module brings backups
  back, without a dump of it: wiki and status are then covered by raw volume files only, and the database of crm is
  not backed up. analytics, support and newsletter are dumped by can-core and need no such image.
  `--allow-broken-backup` is a deprecated no-op.
- Uploading images through `k3s ctr images import`, and the host tool installation (pinned restic, `sqlite3`
  from apt), are not yet exercised on a real server (CI uses k3d's importer and runs the install script on the
  CI runner).
- **The five-minute budget is not verified on real hardware.** Only the in-cluster part is measured, on k3d.
- The default bundle (forms) is enabled in the cluster only unless you pass `--write-bundle`, which rewrites
  `company.yaml` (comments may be reformatted); otherwise add `forms: {}` yourself.
- Email: set `email.relay` in `company.yaml` and pass `SMTP_CREDENTIALS=file` to `make up` (or add the Secret later), see [email.md](email.md); it is outside the budget (SPEC.md section 3.4). Never run against a real relay or cluster.
