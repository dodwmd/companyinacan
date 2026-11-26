# Verifying on a real server

**Nothing in this project has ever run on a real server or cloud account.** This is the runbook for the first
time. It is ordered by what hurts most if it is wrong: exposure of the server, loss of data, then everything
else. "Expected" below means what the code is written to do; none of it has been observed. A step that behaves
differently is a finding, not a mistake of yours.

Use a **throwaway** server, bucket, DNS name and mail account, and no real personal data.

- [Cost](#cost) and [safety](#safety)
- [0. Prepare](#0-prepare) and [1. Make the server](#1-make-the-server)
- [2. Run the automated checks](#2-run-the-automated-checks)
- Highest risk: [3. Server exposure](#3-server-exposure-firewall-reboot-metadata),
  [4. Backup and restore](#4-backup-and-restore), [5. Sign-in, client addresses, lockouts](#5-sign-in-client-addresses-and-limits)
- Next: [6. Certificates](#6-certificates), [7. DNS](#7-dns), [8. Mail](#8-mail), [9. Memory](#9-memory-on-2-gb-and-4-gb),
  [10. Single sign-on](#10-single-sign-on), [11. Modules](#11-modules-on-a-cluster), [12. Browsers](#12-browsers-and-phones),
  [13. Providers and teardown](#13-providers-resume-and-teardown)
- [Reporting results](#reporting-results) and [the inventory of unverified items](#inventory)

## Cost

An estimate, not a quote. Prices are the vendors' list prices as remembered or as copied into
[providers.md](providers.md); check them before you start.

| Item | Needed for | Rough cost |
|---|---|---|
| One 4 GB server for one or two days | everything | under US$2 (a 2 GB DigitalOcean Droplet is listed at US$12 a month; 4 GB about double; Hetzner is usually cheaper) |
| A second server for a few hours | restore (section 4), 2 GB measurement (section 9) | under US$1 |
| Object storage bucket (R2, B2, S3) | backups | free tier or cents |
| A domain you can edit DNS for | sections 6, 7, 8 | an existing one, or about US$10 a year |
| SMTP relay account | section 8 | free tier |
| Identity provider | section 10 | free (a developer tenant, or Keycloak on the second server) |

Total: about US$5 plus a domain if you have none. Servers are billed while they exist, also when powered off:
finish with [section 13](#13-providers-resume-and-teardown).

## Safety

- Run `can down --yes` only against the throwaway. It destroys the server on Hetzner and DigitalOcean.
- Do not put `operator_ip` on the first run unless your address is fixed: it can lock you out
  ([providers.md](providers.md)).
- Let's Encrypt allows few certificates for the same names per week, and `sslip.io` limits are shared with
  everyone. Do not loop `can down` and `can up` on the same names. There is no staging switch.

## 0. Prepare

On your machine: Go (the version in `go.mod`), `make`, `git`, `kubectl`, `ssh`, docker or podman, an SSH key.

```sh
git clone https://github.com/dodwmd/companyinacan && cd companyinacan
make build images
bin/can doctor
bin/can init ../verify-co --name "Verify Co" --domain verify.YOURDOMAIN --non-interactive --provider hetzner
```

Edit `../verify-co/company.yaml`: the provider options ([providers.md](providers.md), `ssh_public_key_file`; the
`ssh` provider on a fresh Ubuntu or Debian server also works). Then `export HCLOUD_TOKEN=...` (or
`DIGITALOCEAN_TOKEN`). Write the bucket credentials to a file outside any repository, mode 0600:
`AWS_ACCESS_KEY_ID=...` and `AWS_SECRET_ACCESS_KEY=...`.

Record the commit (`git rev-parse HEAD`), provider, server type, region and OS image.

## 1. Make the server

```sh
make up ARGS="../verify-co --plan" IMAGES_FROM=build/images
time make up ARGS="../verify-co --backup-endpoint https://ENDPOINT --backup-bucket NAME \
  --backup-credentials-file $HOME/verify-bucket.env" IMAGES_FROM=build/images
make status ARGS=../verify-co
```

Expected: `--plan` changes nothing. The run prints phase lines, the site and admin URLs
(`https://<ip-dashed>.sslip.io`), the admin password and TOTP secret once, and the recovery kit once (type
`show`, then `saved`; keep it in a password manager). `make status` exits 0.

Check: wall-clock time and the per-phase times (the target is five minutes, never measured:
[make-up.md](make-up.md#timing-a-real-server-human-gated-epic-e00)). If a phase fails, note it, and run the same
command again: it should resume. Server defaults (`server_type`, `image`, `location`, `size`, `region`) have never
met the live APIs; note any that are refused.

## 2. Run the automated checks

```sh
bin/can verify --server --company-dir ../verify-co --out verify-1.json
bin/can verify --server --company-dir ../verify-co --probe-public --probe-backup-repo --out verify-2.json
```

Expected: one line per check (`PASS`, `FAIL` or `SKIP`), a count, exit 0 only if nothing failed. On a first run
expect `SKIP server.reboot` (not rebooted yet), `SKIP public.dns` (your domain is not routed yet) and, until the
first backup has run (section 4), `FAIL backup.last-run` and `FAIL backup.repository`. **A skip is not a pass.**

| Flag | What it sends | Checks |
|---|---|---|
| (none) | read-only commands over ssh | bootstrap, k3s, memory, OOM, disk, clock, reboot, ufw, node isolation, secrets encryption, workloads, Traefik client-address policy, `acme.json`, certificate errors, backup timer, dump and last run, credential file modes, presence of the relay and SSO Secrets |
| `--probe-public` | DNS lookups, and connections from your machine to the server | DNS, 6443 and 10250 closed, certificates, site, admin, HTTPS redirect |
| `--probe-rate-limit` | up to 90 requests for the sign-in page | the edge limit refuses |
| `--probe-client-ip` | one failed sign-in for a user that does not exist (an audit entry) | can-core records an address outside the cluster |
| `--probe-backup-repo` | the server lists the bucket (no lock, nothing written) | a recent usable snapshot exists |
| `--probe-smtp` | the server opens TLS to the relay; no sign-in, no mail | port reachable, certificate valid |
| `--probe-sso` | the server fetches the discovery document | issuer, endpoints, algorithms |

No flag sends mail, signs in, or deletes anything. The server checks run from the host, so `--probe-smtp` and
`--probe-sso` do not prove that a **pod** can get out (sections 8 and 10 do).

## 3. Server exposure: firewall, reboot, metadata

Risk: a compromised module reaches the node, the kubelet or the cloud's metadata service.

```sh
S=root@SERVER_IP
ssh $S 'ufw status verbose; can-firewall check; nft list table inet can_isolation'
ssh $S 'k3s kubectl run probe --rm -i --restart=Never --image=busybox:1.36 -- sh -c "
  nc -zw5 SERVER_IP 10250 && echo KUBELET-OPEN; nc -zw5 SERVER_IP 22 && echo SSH-OPEN;
  wget -qO- -T5 http://169.254.169.254/ && echo METADATA-OPEN; nslookup kubernetes.default; echo done"'
nc -zvw5 SERVER_IP 6443; nc -zvw5 SERVER_IP 10250     # from your machine
```

Expected: ufw active with default deny incoming; `status=ok`; the pod prints a DNS answer and `done` and
**none** of the `*-OPEN` lines; both `nc` from outside time out or are refused.

Then reboot and repeat everything above:

```sh
ssh $S reboot
sleep 120; make status ARGS=../verify-co
bin/can verify --server --company-dir ../verify-co --probe-public --out verify-reboot.json
ssh $S 'systemctl is-active k3s ufw can-firewall; swapon --show; journalctl -b -u can-firewall --no-pager | tail'
```

Expected: `PASS server.reboot`, the site answers again without any action, swap is on. Also try
`ssh $S 'ufw reload && can-firewall check'` (expected `status=ok`). The sync loop's cost: `ssh $S 'ps -eo
pcpu,rss,cmd | grep -i can-firewall'`. More: [node-isolation.md](node-isolation.md).

## 4. Backup and restore

Risk: you find out the backups do not restore when you need them. A passing `can verify` proves only that a
backup ran, **not** that it restores.

Put a release-style `can` on the server (the erasure ledger upload needs it) and take a backup now:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o can-linux ./cmd/can
scp can-linux $S:/usr/local/bin/can
ssh $S 'k3s kubectl -n can-system exec deploy/can-core -- /can-core dump-request --wait 5m'
ssh $S 'systemctl start can-backup.service; journalctl -u can-backup --no-pager | tail -20'
ssh $S 'can backup status; set -a; . /etc/can/backup.d/restic.env; set +a; eval "$(grep ^export.RESTIC_REPOSITORY= /usr/local/sbin/can-backup)"; can backup verify'
```

Expected: `backup complete (run ...)`; `status` and `verify` exit 0. A real bucket, the systemd sandbox
(`ProtectSystem=strict`) and the script reading live k3s paths are all untried.

Create data you can recognise first (submit the contact form), and one erasure
(`ssh $S 'can backup ledger-add --subject test-subject-1 --class <a data class>'`). Next morning:
`ssh $S 'systemctl list-timers can-backup.timer; journalctl -u can-backup --since yesterday'` should show the
03:30 run. To test the failure path, write a webhook URL to `/etc/can/backup.d/webhook.url`, rename
`restic.password`, start the service, expect a message and a failed unit, rename it back.

**Restore onto a second, blank server** with a stopwatch (target 15 minutes, unmeasured). Follow
[backup-restore.md](backup-restore.md#restore-can-restore---kit): `can down` the first server (keep the kit and
bucket), `can up --provision-only`, copy `can-linux`, the kit, `build/images` and restic 0.17 or newer to the new
server, `sudo can restore --kit KIT --images-from images --dir /root/company`, then `can up` with the backup
flags again. Expected: eight `[n/8]` steps; the same admin password and TOTP work; the lead you submitted is
there; the erased subject is not; `can verify` passes again. Certificates are not in the backup: new ones are
issued.

Also in the bucket: turn on versioning or object lock on `ledger/`, and check what 35-day retention does with
your provider.

## 5. Sign-in, client addresses and limits

Risk: if ServiceLB hides client addresses, every client shares one bucket, so one attacker locks everyone out.

```sh
bin/can verify --server --company-dir ../verify-co --probe-client-ip --probe-rate-limit
```

Expected: `PASS edge.client-address` and `PASS edge.rate-limit`. Then by hand: from a phone on mobile data, sign
in to the admin URL with a wrong password, and read the address recorded (admin UI, Audit, or
`ssh $S 'k3s kubectl -n can-system exec deploy/can-core -- /can-core audit export | tail -3'`). Expected: the
phone's public address, and the same with `curl -H 'X-Forwarded-For: 192.0.2.1'` (the header must not change it).
Try IPv6 if the server has it. Fail repeatedly from the phone until locked, then sign in from your machine:
expected to work. Recovery: `k3s kubectl -n can-system exec deploy/can-core -- /can-core admin unlock`.

## 6. Certificates

```sh
H=<ip-dashed>.sslip.io
for h in $H admin.$H; do
  openssl s_client -connect $h:443 -servername $h </dev/null 2>/dev/null | openssl x509 -noout -issuer -serial -enddate
done
ssh $S 'k3s kubectl -n kube-system rollout restart deploy/traefik && k3s kubectl -n kube-system rollout status deploy/traefik --timeout=3m'
```

Expected: issuer Let's Encrypt, about 90 days left. After the restart the **same serials** (persisted in
`acme.json`), and the rollout does not hang on the ReadWriteOnce volume. After the reboot of section 3, the same.
`curl -sI http://$H/` answers 307 to `https://`; `curl -si -X POST http://$H/` answers 404. Renewal happens about
60 days in: not testable on a throwaway.

## 7. DNS

Add the records `can status` lists (or set `dns.provider: cloudflare`, token with DNS edit on one zone,
[providers.md](providers.md#cloudflare)), then `can up ... --domain-live`.

Expected: with Cloudflare, records for the domain, `admin`, `help` and each exposed module, commented
`managed-by=can`; a second run changes nothing; a record you created yourself at one of those names is **not**
overwritten (`can up` stops); certificates issue for the domain; `can verify --server --probe-public` shows
`PASS public.dns` and `PASS public.certificates`. `can down` later removes only what `can` created. Also check
sslip.io resolves, and what `make up` does when it does not (plain-HTTP fallback, admin not exposed).

## 8. Mail

Set `email.relay` in `company.yaml` ([email.md](email.md)), put `SMTP_USERNAME` and `SMTP_PASSWORD` in a 0600
file, run `can up ... --smtp-credentials-file smtp.env`, then:

```sh
bin/can verify --server --company-dir ../verify-co --probe-smtp
```

Set `modules.forms.settings.notify_email` to a mailbox you own, apply, and submit the contact form on the site.
Expected: the lead appears in the admin UI, the mail arrives within a minute with can-core's headers, the
delivery page shows it as sent. Check the received headers for SPF, DKIM and DMARC (you publish those records;
`can` does not). Break the password and submit again: expected retries, then a dead letter on the delivery page.
This is the first time pod egress to a relay is used. Note which providers block ports 25, 465 or 587.

## 9. Memory on 2 GB and 4 GB

On each size, one hour after install, with the default bundle:

```sh
ssh $S 'free -m; swapon --show; k3s kubectl top pods -A 2>/dev/null; ps -eo rss,comm --sort=-rss | head -15; journalctl -k | grep -ci "out of memory"'
bin/can verify --server --company-dir ../verify-co --out verify-mem.json
```

Expected (a target, not a measurement): under 1.5 GB used on the 2 GB server, no OOM kills. Repeat after enabling
modules one at a time (analytics, support, newsletter, work, people, jobs, finance), during a backup, and during
an image upgrade. `metrics-server` is disabled, so `kubectl top` may print nothing. Record into
[measurements/footprint.md](measurements/footprint.md). SPEC.md's 600 MB k3s baseline was only measured on a
16 GB CI runner.

## 10. Single sign-on

Follow [sso.md](sso.md) with a real provider (note which), then:

```sh
bin/can verify --server --company-dir ../verify-co --probe-sso
```

In a browser: sign in with SSO (Chrome and Firefox: the redirect back crosses sites and the cookies are
`SameSite`), a user in no mapped group is refused, removing a group removes the role, the password login still
works when the provider is blocked, and a token without `email_verified` is refused. Open a module UI from Apps.
Change the client secret in the Secret and check it is picked up within a minute.

## 11. Modules on a cluster

Only a few modules have ever run on any cluster. For each module you intend to use: enable it in the admin UI,
check `can verify` (`cluster.workloads`, `server.oom`), set `expose.ui`, add its DNS record, issue a personal
access token, do one create-read flow, take a backup and check `can backup status`, then disable it and enable it
again. In particular: forms, people, jobs, finance (event delivery between them, approvals), files (upload,
share link; its S3 backend and ClamAV have no supported wiring), equity, board, entity and provisioning (not
installable yet at the time of writing), analytics, support, newsletter (mail in and out, relay certificate
checking), wiki, crm, status (backup hooks image not published: an enabled one stops all backups).
Details: [module-wiring.md](module-wiring.md), [modules-catalogue.md](modules-catalogue.md).

## 12. Browsers and phones

On the real URLs, in Chrome, Firefox and an Android phone: admin sign-in and every admin page, the setup wizard,
module UIs (sign-in, forms submit without CSRF errors, downloads), the public invoice link, careers application
with an upload, the contact form. Then install the admin UI as an app, enable Web Push, press the test button
with the screen off, and tap the alert ([pwa.md](pwa.md)). Keyboard-only and screen-reader pass; Lighthouse.
Note the browser console errors (Content-Security-Policy violations in particular).

## 13. Providers, resume and teardown

Interrupt `make up` once (Ctrl-C during provisioning, and again during `images`) and run it again: expected to
resume with no second server. Run it a third time on a healthy install: nothing changes. Then:

```sh
bin/can down ../verify-co --plan
bin/can down ../verify-co --yes
```

Expected: server, firewall, uploaded SSH key and `can`'s DNS records are gone. **Confirm in the provider
console**, delete the bucket and the second server, and revoke the tokens. With the `ssh` provider the machine
stays, with ufw and swap.

## Reporting results

Open an issue with the "bug" template, one per failure, titled `real-server: <check id or section>`, and one
summary issue listing every section as pass, fail or not tried. Attach the `can verify` JSON files, and for
failures a `can diagnose --server --company-dir ../verify-co` bundle. Add the commit, provider, server type, OS
image and versions from section 0.

The report is built to carry no secrets, addresses, domain or user names (module names and counts are in it).
**Read it, and anything else you attach, before posting**: the repository is public. Never paste command output
from this runbook without removing addresses, domains, tokens and the recovery kit. Security problems go through
[SECURITY.md](../SECURITY.md), not a public issue.

Report format (`can-verify/v1`): `generated_at`, `can_version`, `provider`, `probes`, `checks` (`id`, `status`
of `pass`, `fail` or `skip`, `detail`) and `summary`.

## Inventory

Every "not verified on a real server" statement collected from the docs, specs and the epics table, and where
this runbook covers it.

| Unverified item | Stated in | Covered by |
|---|---|---|
| Bootstrap on a real host, k3s pin and flags, ufw rules under default deny | specs/006 measurements | 1, 2, 3 |
| Hetzner and DigitalOcean API shapes, default type, image, region, prices, rate limits, cloud-init timing | specs/006, specs/020, [providers.md](providers.md) | 1, 13 (rate-limit responses: not forced) |
| SSH tunnel against a real sshd; image upload and `k3s ctr` import; restic and sqlite3 install | specs/006, specs/033, [make-up.md](make-up.md) | 1 |
| Five-minute `make up` | epics E00, E06, [make-up.md](make-up.md) | 1 |
| Release manifest paths, GHCR push, signing, real release | [releasing.md](releasing.md) | not covered: no server needed, follow releasing.md |
| ACME issuance, `acme.json` persistence, redirect with a real certificate | epics E02, E03f, [dev-cluster.md](dev-cluster.md), specs/040, 045 | 2, 6 |
| sslip.io resolution and fallback | specs/006, specs/017 | 1, 7 |
| Real DNS, Cloudflare zone and permissions, propagation | epics E03f, E12c, specs/020, 045 | 7 |
| Client addresses behind ServiceLB; limits and lockouts | specs/038, 045, [dev-cluster.md](dev-cluster.md) | 2, 5 |
| Node firewall with ufw, reboot, current k3s, real metadata service, CNI directory, sync cost | [node-isolation.md](node-isolation.md), specs/038 | 2, 3 (dual-stack and multi-node unsupported) |
| Egress denial to the node; CoreDNS rewrite and restart; RBAC and namespace templates | [dev-cluster.md](dev-cluster.md) | 3, 11 (CoreDNS: local clusters only) |
| Backup timer and unit sandbox, live k3s paths, real S3, ledger upload, hook file permissions, large data, disk use, duration | [backup-restore.md](backup-restore.md), specs/008, 037, 043, 046 | 2, 4 (large data: not covered) |
| `can restore` on a blank server, overwrite, purge replay, restore time | [backup-restore.md](backup-restore.md), specs/042 | 4 (overwrite and per-module purge replay: try if you use them) |
| `can-hooks` image and the hooks of wiki, crm, status | [backup-restore.md](backup-restore.md) | 11 (blocked until published) |
| 2 GB and 4 GB RAM, full bundle together, SSO and live-connector memory | [footprint.md](measurements/footprint.md), README, [sso.md](sso.md) | 2, 9 (arm64, load: not covered) |
| SMTP relay, pod egress, deliverability, SPF/DKIM/DMARC, inbound mail | [email.md](email.md), epics E03d, E07b | 8, 11 (inbound mail: not built) |
| Real identity provider, mounted Secret and reload, browser redirect flow | [sso.md](sso.md), specs/021, 036 | 2, 10 |
| Modules never installed on a cluster (forms events, people, jobs, finance, equity, board, entity, files, provisioning, wrapped apps) | module READMEs and docs, epics E05, E13-E17 | 11 |
| Real S3 and ClamAV for files; SCIM and SaaS connectors; e-sign, payment, accounting, bank and job board adapters | [files.md](files.md), [provisioning.md](provisioning.md), [finance.md](finance.md), [accounting.md](accounting.md), [recruiting.md](recruiting.md) | not covered: stubs or not wired on a cluster; test against each vendor separately |
| Certificate, disk, DNS and installer probes behind the admin UI | [admin-ui.md](admin-ui.md) | 12 |
| Browsers, screen readers, phones, PWA install, real push services, Lighthouse | [pwa.md](pwa.md), [security-model.md](security-model.md), specs/010, 023, 026, 027, 031, 044 | 12 |
| Prometheus and Grafana loading the shipped rules and dashboard | [observability.md](observability.md), specs/030 | not covered: needs the monitoring images |
| Developer-machine and Podman `make dev`; native Android app | [dev-cluster.md](dev-cluster.md), epics E19 | not covered: not a server item |
