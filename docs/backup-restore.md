# Backup, restore and the recovery kit

Implements the offline-verifiable part of epic E08 (SPEC.md section 6.5). This is a tool, not legal or
compliance advice; the 35-day horizon below is a default you must review against your own obligations.

> **`can restore` is not functional yet.** The provisioning and can-core adapters (epic E03) do not exist in this
> release. A real run stops at step 1 with exit code 3; only `can restore --plan`, kit parsing and the state
> machine itself (tested against fakes) work. Do not rely on a restore until those adapters ship and the
> measurement below has been done.

## How a backup works

Backups were verified end to end on a k3d cluster with a local restic repository (see "Measured" below), not
against a real server or bucket. Design and rejected alternatives: [specs/037-backup-dumps](../specs/037-backup-dumps/research.md).

1. can-core writes the dumps to `/var/lib/can/dumps` on the host through the `can-dumps` volume: a
   PersistentVolume declared by the platform manifests, bound to one claim in `can-system`. No pod has a
   `hostPath`, and can-core has no permission to create volumes. The directory belongs to uid 65532 (host
   bootstrap and `can backup install` set that). A run happens nightly at 02:30 UTC (`CAN_DUMP_AT`), about 10
   minutes after start when no marker younger than 20 h exists, and on request:
   `sudo k3s kubectl -n can-system exec deploy/can-core -- /can-core dump-request --wait 5m` (on the server). It:
   - puts the previous set back if an earlier run died while swapping it, deletes `COMPLETE` and removes what an
     earlier run left behind;
   - writes `can-core/state.db` (`VACUUM INTO` over a read-only connection of its own, then `integrity_check`) and
     a copy of the applied `company.yaml`. These two are replaced in place, so after a **failed** run they are newer
     than the module dumps beside them; without a marker the set is not backed up either way;
   - runs the `backup` hook of every recorded module that has one, **including disabled and failed modules** (their
     data is still there), as a Job in `mod-<id>`. For the in-house modules the Job is `<binary> backup` in the
     module's own image within the module's memory limit (see [In-house modules](#in-house-modules)). analytics,
     support and newsletter are dumped by can-core itself (see [Wrapped modules](#wrapped-modules)). For
     wiki, status and crm it is a shell script in the `can-hooks` image: restricted pod, projected audience-`can` token, the module's
     volume at `/data`, `/storage` and `/app/data` (read-only unless SQLite; none for MariaDB/PostgreSQL modules).
     A module with neither a volume nor a database is skipped. A hook pod that cannot start (image pull, config)
     is given up after 2 minutes, a hook after 15;
   - accepts `PUT /v1/dumps/<module>/<name>` only from the module whose hook is running right now (at most 16
     files and 8 GiB per module and run, 4 GiB per file), into a staging directory. A hook that succeeds without
     uploading anything counts as failed;
   - when every hook succeeded: swaps the staged set in, writes `DUMPED` (which modules, and their database kind),
     flushes everything to disk, then writes `COMPLETE` (run id, finish time). One failing hook therefore stops
     backups until it is fixed: `can backup status` and the admin status page show the last failure
     (`LAST-RUN`).
   - A run that leaves a database (or vanished data) out is announced: `dump-request --wait` prints it, and the
     host script logs a warning and POSTs it to the failure webhook after each backup (the run still exits 0).
   - **A disabled module never stops backups.** If its hook cannot run (the unpublished `can-hooks` image: wiki, status, crm) or fails,
     it is skipped and reported by `can backup status`, the admin status page and `can up`. Only its raw volume
     files are then backed up; for a MariaDB or PostgreSQL module **the database is not backed up**. A module that
     had a dump and suddenly reports no data is reported the same way, and its earlier dump is kept. To get backups
     back after enabling a module whose hook cannot run, disable it. Downloads (`GET /v1/dumps/...`) are refused: there is no restore mode yet.
2. The host timer `can-backup.timer` (default 03:30 plus up to 15 min) starts `can-backup.service`, which runs
   `/usr/local/sbin/can-backup`. The script, in order:
   - syncs the erasure ledger (a symlinked ledger is refused with exit 13; a ledger that vanished after an earlier run, or is not a
     regular file, is reported and exits 22 rather than being skipped; first and independently, so a failing backup never holds back erasures) and copies it
     into the root-owned host-state directory so every snapshot carries a copy;
   - refuses a dumps directory that is world-writable or contains a symlink (can-core can write there, the script is root),
     and a marker that is missing or older than 24 h. These checks are not atomic: a path could change between the check and
     its use. The script therefore keeps everything it writes in root-private directories (it checks owner and that they are not
     group- or other-writable) and never follows links that can-core controls, but it cannot make a check-then-use race impossible;
   - copies the marker it is about to back up, the k3s SQLite datastore (`sqlite3 .backup`), the node token and the
     secrets-encryption key, and writes `MANIFEST.sha256`, all into `/var/lib/can/host-state` (root-owned, outside the dumps dir);
   - runs `restic backup` over the dumps (staging and temporary dot files left out), the PVC root (raw SQLite files
     are excluded only for the SQLite modules that `DUMPED` says were dumped in this run; the data directory of the
     [shared Postgres](#the-shared-postgres-is-not-backed-up) is always excluded) and the host-state
     directory, tagged `can`, `run-<id>`, `attempt-<id>`;
   - re-reads the marker: if can-core started a new dump run meanwhile, the snapshot is tagged `inconsistent` (restore never
     picks it), forget is skipped and the run exits 11;
   - `restic forget --group-by '' --keep-within 35d --prune --tag can`. Grouping is disabled so snapshots from a previous
     host name or path set age out instead of living forever (restic groups by host and paths by default).
   restic exit 3 (some files unreadable or vanished) means a snapshot was created: it is logged and treated as success.
3. Credentials live in `/etc/can/backup.d/restic.env` and `restic.password` (root, 0600). The unit carries none.

Install on the host: `can up --backup-endpoint ... --backup-bucket ... --backup-credentials-file ...` does it all,
including `sqlite3` and restic (the upstream release binary pinned by version and sha256 in
`core/backup/provision.go`, installed to `/usr/local/bin/restic` when restic 0.17 or newer is missing). By hand:
`can backup install --kit <kit file> --enable` (writes the script, unit, timer and credential files, creates the
dumps directory and initialises the repository if none exists; install restic and `sqlite3` yourself).
`can backup install --plan` prints the generated script.

### Script exit codes

| Code | Meaning |
|---|---|
| 0 | ok |
| 1 | unexpected error or missing credentials file |
| 10 | completion marker missing (can-core has not finished a dump run) |
| 11 | marker older than 24 h, future-dated, malformed, or changed while the backup ran (snapshot tagged `inconsistent`) |
| 12 | another run holds the lock |
| 13 | unsafe dumps directory (symlink inside, or world-writable), state or host-state directory not a root-owned private directory, or a symlinked ledger |
| 20 | `restic backup` failed (forget/prune is skipped) |
| 21 | forget/prune failed |
| 22 | erasure ledger upload failed, or the ledger vanished or is not a regular file (the backup still ran). If a backup code and 22 both occur, the backup code is returned |

Failures go to the journal (`journalctl -t can-backup`) and, through `OnFailure=can-backup-failure.service`, a critical journal line
when the unit fails. The unit runs with `ProtectSystem=strict` and a short `ReadWritePaths=` list. To also get a webhook, put its URL in
`/etc/can/backup.d/webhook.url` (0600); the script POSTs `{"text": "can-backup failed: (code) message"}` (JSON-escaped); the URL is given to curl on stdin, never on a command line.
`restic.env` values are single-quoted so any character in a secret survives sourcing.

## In-house modules

forms, work, people, jobs, finance, equity, board, entity, provisioning and files each dump their own SQLite
database with `<binary> backup`: `VACUUM INTO` over a read-only connection of its own, `integrity_check`, upload.
Their live database files are then left out of the snapshot. files and jobs also record every file the database
refers to (size and SHA-256); the bytes themselves are in the volume copy, and the backup fails if one is missing
(delete or purge the affected record, or put the file back). With the files `s3` backend the bucket is **not**
backed up by this project.

To dump or restore one instance by hand, with the service stopped for a restore:

```
can-forms backup -config forms.yaml -out ./set
can-forms restore -config forms.yaml -from ./set            # add -replace to overwrite a database
can-files restore -config files.yaml -from ./set -blobs <restored volume>/blobs
```

**Keys are not in the dumps.** people, jobs, files, finance, equity and board seal data with keys held in
Kubernetes Secrets. Without the original keys the restored data is unreadable. They are in the snapshot only
inside the k3s datastore copy; export them and keep them with the recovery kit. Which keys, and the contract
`can restore` follows: [specs/043-module-backup-hooks/contracts/hooks.md](../specs/043-module-backup-hooks/contracts/hooks.md).

## Wrapped modules

analytics, support and newsletter need no hooks image ([specs/046-wrapped-backups](../specs/046-wrapped-backups/spec.md)).
The hook Job uses images already on the server: the pinned Postgres image runs `pg_dump` / `pg_restore` with the
module's own database role, and can-core's image uploads and downloads.

| Module | In the dump set |
|---|---|
| analytics | a checked copy of its SQLite database |
| support | `pg_dump` of its database, then an archive of its volume (attachments) |
| newsletter | `pg_dump` of its database, then an archive of its volume (uploads) |

- The database is dumped before the files are archived. A file the application deletes between the two is missing from
  the set although the dumped database refers to it. The application is not stopped.
- The files are in the dumps directory as well as on their volume, so they take disk twice, and a set is limited to
  4 GiB per file and 8 GiB per module. A bigger module fails the dump run (and then nothing is backed up).
- A restore replaces the whole schema `public` of the module's database and the whole content of its volume.
- A server installed before this is covered from the next dump run; no module upgrade is needed.
- wiki, status and crm still need the unpublished `can-hooks` image: enabling one stops every backup.

## Retention and erasure of dumps

The dumps directory holds one set: each successful run replaces all module dumps with the ones it just made, so a
module that was purged (or no longer has a record) drops out. **One exception keeps data longer:** the dump of a
module that was skipped (disabled with a hook that cannot run or fails, or suddenly without data) is carried forward
unchanged, run after run, with no age limit. It is therefore also in every new snapshot, so it outlives the 35-day
retention for as long as it is carried. It goes away when the module is dumped again or purged, or by hand.

**Clearing a skipped or "no data" module.** If its data is meant to be gone (you deleted its volume or database),
remove the carried dump on the server: `sudo rm -r /var/lib/can/dumps/modules/<id>`. A "no data" module without a
database then drops out of the reports at the next run. A database module is reported as skipped (disabled) or fails the
run (enabled) for as long as can-core records it; there is no production purge yet, so disable it and expect the
report. Older snapshots keep the data until retention expires them.

Leftovers of interrupted runs are removed when the
next run starts, and a purge through can-core deletes that module's dumps at once. A failed run keeps the previous
set on disk (without a marker, so it is not backed up). Older data stays in restic snapshots until the 35-day
retention expires them; erasure requests are re-applied at restore from the erasure ledger, not by rewriting dumps
or snapshots.

## Commands

Run these on the server, as root (`sudo`). `can up` does **not** copy the `can` binary to the server: put the
Linux release binary at `/usr/local/bin/can` there first (the erasure ledger upload needs it too). Without it,
`sudo systemctl start can-backup.service` runs a backup and `journalctl -u can-backup` shows the result.
`status` and `verify` find the repository in `/etc/can/backup.d/restic.env`.

| Command | Use |
|---|---|
| `can backup status` | marker age, latest usable snapshot (tagged `can`, not `inconsistent`), problems; exit 1 on any problem |
| `can backup run` | run the script now |
| `can backup verify` | `restic check`, restore a sample of dump files into a temp dir, compare SHA-256 with the manifest |
| `can backup kit` | generate the recovery kit (see below) |
| `can restore --kit <file or -> [--plan]` | restore onto a blank host |

## Recovery kit

Contents: restic password, bucket credentials and endpoint, company name and domain, provider type and its
non-secret options. Nothing else is needed; everything else is in the snapshot.

- `can backup kit --endpoint ... --bucket ... --credentials-file ...` shows the kit once, after you type `show`,
  and asks you to type `saved`. Store it in a password manager.
- `--out kit.enc` writes a passphrase-encrypted file (argon2id + XChaCha20-Poly1305; at least 12 characters). A typed
  passphrase must be entered twice; the file is read back and decrypted before the command reports success. The tool refuses
  to write inside a git working tree (symlinks are resolved) and never overwrites a file.
- If `/etc/can/backup.d/restic.password` exists, its password is reused, and if a repository exists the command checks that the
  password opens it (refusing otherwise). If no such file exists, the command checks that no repository is already at the target
  and refuses otherwise (a fresh password would not open it). If restic cannot be run or the bucket cannot be reached, the command
  fails closed instead of guessing. Requires restic 0.17 or newer (its "no repository" exit code 10 is what the check relies on).
- Only an allow-list of provider options (host, port, user, location, server type, image, region, datacenter) goes into the kit.
- The kit must never be committed. Losing it makes the backups unreadable; there is no recovery.

## Restore (`can restore --kit`)

The six steps of SPEC.md section 6.5; progress is saved in `<dir>/.can/restore.json`, so after a failure you fix the
cause and run the same command again. `--plan` shows the steps and which are done without touching anything.

1. Provision, k3s, can-core in restore mode.
2. The host must be blank: the dumps and host-state directories must be absent or empty (else refuse; `--force-host` overrides).
   Snapshots are walked newest first and every decision is printed. A snapshot tagged `inconsistent` is skipped (the backup flagged
   it); one not tagged `can` is ignored. For a newer `can` snapshot that fails its check (marker missing or different from the copy the
   backup ran under) the restore stops and asks for `--snapshot ID` or `--force-older`; it never falls back to an older snapshot silently. A
   transient restic error while checking a snapshot also stops the restore (retry); it is never treated as "inconsistent". The chosen
   snapshot id and time are printed. Dumps and host state are restored, every
   restored dump is hashed against the manifest, then can-core imports its state and re-creates the exported Secrets.
3. `company.yaml` is written back into `--dir` (an existing different file is never overwritten) and a restore
   apply runs.
4. Module `restore` hooks (replace-all) load the database dumps; modules stay scaled to 0.
5. PVC data is restored to a staging directory (absolute, at least 3 path components, empty or created by `can restore`; it
   is never deleted otherwise) and copied into the new volumes matched on `<namespace>_<claim>` (ownership and modes
   preserved). A volume that already holds data is not overwritten without `--force-volumes`. A backed-up volume with no matching claim
   fails the step.
6. The erasure ledger is fetched and decrypted and united with the copy stored inside the snapshot; `purge(subject, class)`
   hooks re-run (progress is tracked per subject and class), tombstones are re-applied to can-core's state, and only then
   workloads scale up. No ledger in the bucket blocks going live unless you pass `--allow-no-ledger` (only appropriate if no
   erasure was ever requested, and never enough when the snapshot holds a ledger copy). A ledger that cannot be fetched always
   blocks, and a bucket ledger that lacks entries the snapshot's copy has (rollback or truncation) blocks too
   (`--accept-ledger-behind` applies the union anyway and prints a warning; erasures newer than the snapshot are then not applied).

The snapshot is pinned on the first run (`--snapshot` to choose one) so a resume never mixes snapshots.

## Erasure tombstone ledger

can-core appends `(subject id, data class, date)` to `/var/lib/can/erasure-ledger.jsonl` using `backup.Ledger.Append`
(`can backup ledger-add` does the same by hand). Subject ids must be opaque (no email or name; the tool rejects them).
The file is hash-chained, so editing, reordering or removing early entries is detected. After each backup the script
runs `can backup ledger-sync`, which merges it into the encrypted object `ledger/erasure-ledger.v1.enc` in the bucket,
outside the `restic/` prefix. The key is derived from the restic password. Sync never shrinks the remote ledger and
refuses to overwrite one it cannot read. Requirement: the `can` binary at `/usr/local/bin/can` on the host. The S3 client
requires https, and treats only an S3 `NoSuchKey` 404 as "no ledger" (a missing bucket is an error). A torn last line left by a
crash is ignored and truncated on the next append. Subject ids are checked against a character pattern only: nothing can
enforce that an id is opaque, so callers must never pass an email address or name.

**Residual risk (rollback of the bucket ledger).** The ledger is only as durable as the bucket. Enable bucket versioning or
object lock on `ledger/` so an overwritten or deleted object can be recovered. Restore compares the bucket copy with the copy
inside each snapshot and refuses a shorter one, but an erasure recorded after the last backup and not yet synced exists only on
the server: if the server is lost before the next sync, that erasure is lost and the data would come back from the snapshot.
The ledger is synced at the start of every backup run (and can be run by hand with `can backup ledger-sync`), so the exposure
is one backup interval at most.

**Residual risk (stale-snapshot rollback).** An attacker with write access to the bucket could delete the newest snapshots and
replay an older sealed ledger object together with them. Every check above compares the bucket with a copy that lives in the same
bucket, so a consistent older state is indistinguishable from the real one. Mitigations are outside this tool: bucket versioning
or object lock with a retention period, separate credentials for the backup writer that cannot delete, and noting the latest
snapshot id and ledger length somewhere else (for example when you print the kit) to compare at restore time.

## Measuring restore time (human-gated)

SPEC.md targets a restore onto a blank server in 15 minutes or less. This repository does not claim that number; it is
measured by a person:

1. Use a throwaway account and a company with realistic data (for example the seed from E02) and a bucket you may delete.
2. Take a backup (`can backup run`), then `can backup verify`; note the repository size.
3. Destroy the server (`can down --yes`) and, with a stopwatch running, start from the recovery kit on a laptop with only
   the `can` binary: `can restore --kit kit.enc --passphrase-file p --dir ./restored`.
4. Stop at the first successful load of `https://<domain>/` and an admin login. Record wall-clock per step (the command
   prints `[n/6]` lines to stderr) and the total.
5. Repeat three times, once with an empty ledger and once with at least one erasure, and note the provider, region, server
   type, data size and `can`/k3s/restic versions.
6. Record the results in `specs/008-backup-restore/measurements.md` and update SPEC.md section 6.5 and section 10 with the
   measured number. Until then the claim stays unproven.

## The shared Postgres

can-core runs a Postgres in `can-data` for modules that need one (today `support` and `newsletter`,
[specs/041](../specs/041-wrapped-modules-live/spec.md)).

- Its data directory (`pvc-*_can-data_postgres-data`) is excluded from the file backup, because a copy of a
  running server's files is not a backup.
- Each module's database is in the backup as that module's [dump](#wrapped-modules). A database no module dumps (one
  you made by hand, or the server's roles and settings) is **not** backed up. can-core creates the roles again.
- An operator's own server (`CAN_POSTGRES_ADMIN_URL`) is dumped the same way; its major version must not be newer
  than 17, or the dump run fails. If it runs in `can-data` with a claim named `postgres-data`, that volume is
  excluded too.
- The backup script of an existing server excludes the directory only after it is generated again
  (`can backup install` or `can up`).

## What is and is not in a snapshot

- In: `can-core/state.db`, `company.yaml`, each module hook's dump, the local-path volumes (except can-core's
  live database, the SQLite files of the modules dumped in that run and the shared Postgres's data directory), a copy of the k3s datastore, node
  token and encryption key, the erasure ledger copy.
- A module without a backup hook that keeps SQLite on a volume is covered only by the raw copy of live files, which
  may be inconsistent. `can backup install` warns. Every shipped module has a hook.
- Not there: a separate export of Kubernetes Secrets (SPEC 6.5). Module keys exist only inside the k3s datastore
  copy, so recovering them needs a raw datastore restore. Without them the sealed data of people, jobs, files,
  finance, equity and board cannot be read.
- Not there: the content of an S3 bucket used by files.

## Measured (k3d in CI)

Workflow `make-up-e2e` (advisory) runs: dump request, the installed backup script against a local restic
repository, `can backup status` and `verify`, and a restore of the dumps directory. `dev-cluster` (advisory) runs
a module backup hook Job. The numbers are in
[specs/037-backup-dumps/measurements.md](../specs/037-backup-dumps/measurements.md).

## Not verified (honest list)

- A real S3-compatible bucket (R2, B2, S3). CI uses a filesystem repository: the script's repository line is the
  one thing changed. The SigV4 ledger client has only met an `httptest` server.
- Hook file permissions against the real module images (the hook runs as uid 1000; files another user wrote with
  mode 0600 would be unreadable).
- A real server: the systemd unit and timer (`ProtectSystem=strict`, `ReadWritePaths`), bootstrap's directory
  ownership, `apt` installing `sqlite3`, and the script reading the live k3s paths. CI runs the script on the
  runner against copies of the k3d node's directories.
- **Restore.** `can restore` is not functional (the provisioning and can-core adapters return exit 3). Restoring
  onto a blank host, importing can-core state, module `restore` hooks and PVC data have never run. can-core has no
  restore mode: whoever implements it must keep dump runs off until the import is done.
- The shipped script hooks (wiki, crm, status): CI builds the `can-hooks` image and
  parses them in it, but the image is not published, so while one of these modules is enabled the dump run fails (and
  no marker is written); disabled, it is skipped. None of these hooks has run against its application. A release
  publishes the image (docs/releasing.md); until then there is nothing to pull.
- In-house hooks on a cluster: forms, work and files only (`module-backup` in CI). jobs, people, finance and
  provisioning are covered by unit tests; equity, board and entity cannot be installed by can-core yet. Restore ran
  by hand on fresh instances (docker), not through can-core.
- `can-hooks` Jobs against a database module. Only `crm`'s chart still names a `can-database` Secret that
  can-core does not write.
- The wrapped modules' dumps (analytics, support, newsletter) on a real server, with large data, or against an
  external Postgres.
- Disk use: uploads are capped per file and per module and run, but there is no total quota on the dumps
  directory and a run holds the old and the new set at once. Hooks retry with `--retry` only, which does not cover
  a connection refused while a new pod's network rules are still being programmed.
- The erasure ledger upload (can-core does not write the ledger yet; `can backup ledger-add` does by hand).
- The restore-time target (15 minutes) and the effect of data size on dump and backup time.
- Whether the 35-day retention fits your legal obligations is for you to decide.
