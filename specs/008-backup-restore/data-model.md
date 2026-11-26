# Data Model: Backup, Restore and Recovery Kit (Epic E08)

| Entity | Fields | Notes |
|---|---|---|
| Settings | dumps dir, PVC root, k3s dir, config dir, state dir, script path, repository, retention days, marker max age, schedule, random delay, ledger source, can binary | Validated against shell/unit injection |
| Plan | Dumps[] (module, hook, file), Sources[], Excludes[], Warnings[] | Derived from company.yaml plus manifests |
| Marker | run_id, finished_at | `COMPLETE` in the dumps dir, written by can-core |
| Manifest | relpath -> sha256 | `MANIFEST.sha256` in the dumps dir, written by the script |
| Snapshot | id, short_id, time, paths, tags | From `restic snapshots --json` |
| Kit | version, created, company{name,domain}, restic{repository,password}, bucket{endpoint,name,region,access_key_id,secret_access_key}, provider{type,options} | options are non-secret strings only |
| Entry (ledger) | seq, subject, class, date, prev, hash | `hash = sha256(prev, seq, subject, class, date)`; no PII |
| Restore state | version, fingerprint, snapshot, steps{id -> done, finished, error}, modules[], ledger_done | `.can/restore.json`, 0600 |
| PlanStep | n, id, title, detail, done | `--plan` output |

Files on the host: `/usr/local/sbin/can-backup` (0700), `/etc/systemd/system/can-backup.{service,timer}`,
`/etc/can/backup.d/restic.env` (0600), `/etc/can/backup.d/restic.password` (0600),
`/etc/can/backup.d/webhook.url` (optional, 0600), `/var/lib/can/backup.lock`, `/var/lib/can/backup-status`,
`/var/lib/can/erasure-ledger.jsonl` (written on the host by `can backup ledger-import` from what can-core
collects, by `ledger-add` and by a restore; specs/048-erasure-collection).

Bucket layout: `restic/` (the restic repository, subject to forget/prune), `ledger/erasure-ledger.v1.enc`
(outside the prefix; never pruned).
