# CLI contract: `can up`, `can status`, `can down` (E24)

## `can up [dir]`

```
can up [dir] [--plan] [--provision-only] [--dns manual|cloudflare] [--modules-dir D]
       [--images-from DIR | --release-manifest FILE]
       [--print-secrets] [--no-default-bundle] [--domain-live] [--acme-email ADDR]
       [--backup-endpoint URL --backup-bucket NAME --backup-credentials-file FILE [--backup-region R]]
       [--kit-out FILE [--kit-passphrase-file FILE]]
```

`make up` forwards `ARGS`, and `IMAGES_FROM=<dir>` as `--images-from`.

### Phases (in order; ids are the keys in `.can/state.json` `launch.phases`)

| # | id | What | Idempotent re-run |
|---|---|---|---|
| 0 | preflight | company.yaml valid; DNS plugin check; kubectl found; images resolved and verified; backup flags consistent; kit destination exists. **No server exists yet.** | always |
| 1 | (provider) | preflight probe, bootstrap k3s (existing script, ports and firewall unchanged), wait Ready | skipped when the pinned k3s version is installed |
| 2 | (access) | fetch kubeconfig (to `.can/kubeconfig`), open the SSH tunnel to 127.0.0.1:16443 | always |
| 3 | base | wait for k3s CRDs; apply deploy/base; restart CoreDNS only if `coredns-custom` changed | re-applied |
| 4 | images | upload + sha256-verify + import tarballs (dir source), or nothing (release source) | skipped if the fingerprint matches |
| 5 | company | ConfigMap `can-system/company` (default bundle adds `forms: {}` unless mentioned) | re-applied |
| 6 | core | apply can-core (image, company name/domain, cookie-secure), wait for the rollout | re-applied |
| 7 | admin | create the admin (stdin password); parse the TOTP secret | skipped once the credentials were shown; re-issued if created but never shown |
| 8 | site | build, pack, ConfigMap `site-content`, apply the site pod, wait | re-applied; pod rolls only if the digest changed |
| 9 | routes | IngressRoutes for site/admin; wait for the forms module; `/_forms` route | re-applied |
| 10 | backup | (only with `--backup-*`) install timer + units + credentials on the host, init the repository | skipped once installed; kit rebuilt from the host if not yet shown |
| 11 | ready | `kubectl wait` Available for can-core, site, forms | always |
| 12 | (dns) | Cloudflare upsert, or print the records | idempotent per plugin |
| 13 | (probe) | GET the site URL for up to 60 s; a failure is a warning | always |
| 14 | summary | URLs, credentials, kit, phase table | n/a |

Timeouts (defaults): rollouts 3 min, CRD registration 3 min, module install 5 min, public probe 60 s.

### Exit codes

| Code | Meaning |
|---|---|
| 0 | success |
| 1 | failure. A phase failure prints `phase "<id>" failed: <cause>`, a `look:` line (commands to inspect) and a `resume:` line |
| 2 | usage (unknown flag, bad arguments) |
| 3 | not implemented (provider `local` or `static`, `provider-native` DNS) |

A run interrupted by Ctrl-C or a crash exits non-zero; run the same command again.

### Output

Human output: progress lines `[n/total] phase`, then a summary. Secrets (admin password, TOTP secret and URI,
recovery kit) are printed to stdout only when stdin and stdout are a terminal (after the confirmation prompt
`Type 'saved' ...`, and for the kit `Type 'show' ...` / `'saved'`) or when `--print-secrets` is given. Otherwise the
summary says they were not shown; the next run issues new admin credentials.

`--output json` (global flag): progress goes to stderr, stdout is exactly one document:

```json
{
  "provider": "ssh", "actions": [...], "address": "203.0.113.7",
  "instant": {"mode": "https", "hosts": [...], "urls": [...]},
  "dns_records": [{"type":"A","name":"acme.example","value":"203.0.113.7","ttl":300}],
  "kubeconfig": ".can/kubeconfig", "planned": false,
  "phases": [{"name":"base","seconds":2.1,"finished":true}, {"name":"images","skipped":true,"seconds":0,"finished":true}],
  "site_urls": ["https://203-0-113-7.sslip.io"], "admin_urls": ["https://admin.203-0-113-7.sslip.io"],
  "site_reachable": true, "admin_user": "admin", "warnings": [], "total_seconds": 148.2,
  "admin": {"username":"admin","password":"...","totp_secret":"...","totp_uri":"otpauth://..."},   // only with --print-secrets
  "recovery_kit": "...",                                                                          // only with --print-secrets
  "recovery_kit_file": "/path/kit.enc"                                                            // with --kit-out
}
```

`--plan` prints (or, with `--output json`, emits under `actions`) the provider actions plus one entry per stack
phase (`stack.*`) and `summary`; it changes nothing and needs no kubectl or images.
`--provision-only` stops after the DNS step, exactly the behaviour before E24.

### Refusals before any server exists

- `kubectl was not found in PATH`
- no image source (`--images-from`, `--release-manifest` or `deploy/release-images.json`), both given, a checksum
  mismatch, an unpinned release reference
- partial `--backup-*` flags; backup on without a terminal and without `--kit-out`/`--print-secrets`;
  `--kit-out` without `--kit-passphrase-file` and no terminal

## `can status [dir]`

As before, plus (only when `.can/state.json` has a `launch` object and the provider reports ready) the checks
`phases` (the `ready` checkpoint exists), `can-core`, `site` and, once routed, `forms` (available replicas, read
through the SSH tunnel). A tunnel that cannot be opened is a failed check. Any failed check makes the phase
`degraded` and the exit code 1.

## `can down [dir]`

Unchanged (`--yes` required; DNS records can created are removed first). New: for the `ssh` provider, if
`launch.backup_installed`, the backup timer, unit files, script and `/etc/can/backup.d` (restic credentials) are
removed from the server before k3s is uninstalled; a failure to do so stops the teardown (fail closed). Cloud
providers delete the machine, so nothing is removed separately.
