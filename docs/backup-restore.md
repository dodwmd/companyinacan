# Backup, restore and the recovery kit

Implements the offline-verifiable part of epic E08 (SPEC.md section 6.5). This is a tool, not legal or
compliance advice; the 35-day horizon below is a default you must review against your own obligations.

> **`can restore` has only been run on throwaway k3d clusters in CI**, with a local restic repository. It has not
> been run on a real server or against a real bucket. Rehearse it on a spare server before you rely on it.

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
     a copy of the applied `company.yaml` (without it the run fails and writes no marker; right after the
     first `can apply` the file takes about a minute to be mounted), and `can-core/secrets.json`: the Secrets of every module namespace plus
     `can-smtp-relay`, `sso-client` and `can-postgres-admin` of `can-system` (a restore re-creates them). The first two are replaced in place, so after a **failed** run they are newer
     than the module dumps beside them; without a marker the set is not backed up either way;
   - runs the `backup` hook of every recorded module that has one, **including disabled and failed modules** (their
     data is still there; see below for the two that never stop a run), as a Job in `mod-<id>`. For the in-house modules the Job is `<binary> backup` in the
     module's own image within the module's memory limit (see [In-house modules](#in-house-modules)). analytics,
     support and newsletter are dumped by can-core itself (see [Wrapped modules](#wrapped-modules)). For
     wiki, status and crm it is a shell script in the `can-hooks` image: restricted pod, projected audience-`can` token, the module's
     volume at `/data`, `/storage` and `/app/data` (read-only unless SQLite; none for MariaDB/PostgreSQL modules).
     A module with neither a volume nor a database is skipped, and so is a database module whose database was never
     provisioned (its `can-db` Secret has no `DB_HOST`). A hook pod that cannot start (image pull, config)
     is given up after 2 minutes, a hook after 15;
   - accepts `PUT /v1/dumps/<module>/<name>` only from the module whose hook is running right now (at most 16
     files and 8 GiB per module and run, 4 GiB per file), into a staging directory. A hook that succeeds without
     uploading anything counts as failed;
   - when every hook succeeded: swaps the staged set in, writes `DUMPED` (which modules, and their database kind),
     flushes everything to disk, then writes `COMPLETE` (run id, finish time). One failing hook (apart from the two
     cases below) therefore stops backups until it is fixed: `can backup status` and the admin status page show the last failure
     (`LAST-RUN`).
   - A run that leaves a database (or vanished data) out is announced: `dump-request --wait` prints it, and the
     host script logs a warning and POSTs it to the failure webhook after each backup (the run still exits 0).
   - **A module whose first install failed never stops backups either** (recorded as failed, no `can-release`
     Secret in its namespace because no rollout ever succeeded, and no earlier dump): its volume may exist while its
     image cannot be pulled, so the hook Job cannot start. It is tried, then skipped and reported like a disabled
     module. It was never routed, so nobody has used it. If its erasure feed cannot be
     read it counts as not asked, not as a failure. Once an install succeeds its dump is mandatory.
   - **A disabled module never stops backups.** If its hook cannot run (the unpublished `can-hooks` image: wiki, status, crm) or fails,
     it is skipped and reported by `can backup status`, the admin status page and `can up`. Only its raw volume
     files are then backed up; for a MariaDB or PostgreSQL module **the database is not backed up**. A module that
     had a dump and suddenly reports no data is reported the same way, and its earlier dump is kept. To get backups
     back after enabling a module whose hook cannot run, disable it. Downloads (`GET /v1/dumps/...`) are refused except while the server is being restored.
2. The host timer `can-backup.timer` (default 03:30 plus up to 15 min) starts `can-backup.service`, which runs
   `/usr/local/sbin/can-backup`. The script, in order:
   - imports into the erasure ledger what can-core collected from the modules (`can backup ledger-import`) and
     syncs it to the bucket, an empty ledger too when nothing was ever erased, so that a restore finds one (a symlinked ledger is refused with exit 13; a ledger that vanished after an earlier run, or is not a
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
     [shared Postgres](#the-shared-postgres) is always excluded) and the host-state
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
| 22 | erasures could not all be collected or imported (a module's feed failed, can-core's last collection is more than 2 h old, or a tombstone was refused), the ledger upload failed, or the ledger vanished or is not a regular file (the backup still ran). If a backup code and 22 both occur, the backup code is returned |

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

`restore` takes the existing database exclusively first and refuses while any process uses it, refuses a dump from a
newer schema than the binary knows, and `backup` refuses an empty or schemaless database file. A referenced file that
is missing fails the backup with the record to **purge** (`POST /v1/purge`; deleting a file only moves it to the trash
and keeps the reference); the same is true after a restore with `-allow-missing-blobs`, which prints it.

To dump or restore one instance by hand, with the service stopped for a restore:

```
can-forms backup -config forms.yaml -out ./set
can-forms restore -config forms.yaml -from ./set            # add -replace to overwrite a database
can-files restore -config files.yaml -from ./set -blobs <restored volume>/blobs
```

**Keys are not in the module dumps.** people, jobs, files, finance, equity and board seal data with keys held in
Kubernetes Secrets. Without the original keys the restored data is unreadable. can-core exports every Secret of
each module's namespace (`can-module` and the operator-made `can-equity-keys` / `can-board-keys` among them) into
`can-core/secrets.json`, and `can restore` re-creates them before the modules. Which keys, and the contract
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
- A restore of a module with a database first checks that the server answers and is Postgres 17, before anything on
  the volume is replaced.
- Disk a restore needs on the server, all at the same time and besides what is there already:
  - the downloaded set, as stored (compressed), in the Job's work volume: up to 8 GiB (the volume is capped at 9 GiB);
  - a complete **uncompressed** second copy of the data on the module's volume: files (or the SQLite database) are
    unpacked beside the old content and swapped in afterwards. This can be far more than the size of the set;
  - for a Postgres module, room in the database server for the old and the new data together (the load is one
    transaction), plus its write-ahead log.

  The 8 GiB limit is on the stored set, so it bounds neither of the last two. When the node evicts the restore pod
  (disk pressure, work volume over its limit) or cannot schedule it, the error says so.
- Databases are dumped from and restored into Postgres **17** only (see [The shared Postgres](#the-shared-postgres)).
- A restore replaces the whole schema `public` of the module's database and the whole content of its volume.
- A server installed before this is covered from the next dump run; no module upgrade is needed.
- wiki, status and crm need the `can-hooks` image, which is not published: enabling one stops every backup
  unless the image is on the server (`make images` builds it and `can up --images-from` ships it; none of these
  hooks has run against its application).

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
| `can backup verify` | `restic check`, restore a sample of dump files into a temp dir, compare SHA-256 with the manifest, then the checks a restore makes of the snapshot (marker, state database, company.yaml, Secrets export, every dump loadable) |
| `can backup kit` | generate the recovery kit (see below) |
| `can backup snapshots --kit <file>` | list the snapshots, and read the erasure ledger, with the kit alone (not the installed credential files or the shell's variables); no lock, nothing written. Run it before destroying a server |
| `can restore --kit <file or -> [--plan]` | restore onto a blank server, run on that server as root |

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

`can restore` runs **on the new server**, as root. It does not create the server.

1. From your laptop, in your company repository (or a directory holding a `company.yaml` with the same name, domain and
   provider as the kit), make the new server with k3s and nothing else: `can up --provision-only`. Do **not** run a full
   `can up` first: the restore refuses a server that is not blank.
2. On the server, put the `can` release binary and restic 0.17 or newer in `/usr/local/bin`, and the recovery kit in a
   root-only file. Use the same `can` release the backup was made with: it carries the module charts.
3. Restore: `sudo can restore --kit /root/kit.enc --release-manifest release-images.json --dir /root/company`
   (from a source checkout use `--images-from <make images output>`). `--plan` shows the steps and what is done.
4. From the laptop, run `can up` with your usual flags: it publishes the site (from your company repository, which is
   **not** in the backup), applies the routes and DNS and installs the backup timer. It keeps the restored admin
   account. Modules with `expose.ui` or `expose.api` stay stopped until this `can up` has written the routing facts
   (docs/module-routing.md); can-core then starts them by itself.
5. Check that the old server is gone, and review users, roles and API tokens: they are those of the backup, so anything
   revoked since then is valid again, and an account deleted since then is back. Pending set-up links are dropped.

The steps `can restore` runs (progress is kept in `/var/lib/can/restore-state.json`; after a failure fix the cause and
run the same command again):

1. **Check and fetch.** The server must be blank. The newest snapshot is chosen (rules below), restored into
   `--staging` (default `/var/lib/can/restore-staging`; it needs room for the whole snapshot, and the volumes need it
   again), every dump is hashed against the manifest, and the state database, company.yaml, Secrets export and ledger
   are checked. Anything that would leave the restore incomplete stops it here, before the server is changed.
2. **Dumps** are put in `/var/lib/can/dumps`, without a completion marker; company.yaml is written to `--dir`.
3. **Platform**, with can-core in **restore mode**: it imports its state (admin password and TOTP seed preserved).
   In restore mode there is no email, webhook delivery, dump run, module reconcile or admin UI (503). The mode is
   latched on can-core's volume: re-applying manifests or restarting does not end it.
4. **Secrets and modules.** The exported Secrets are re-created and every recorded module (disabled ones too) is
   installed with 0 replicas, no seeding Jobs and no public routes.
5. **Volume files** are copied into the new volumes (matched on `<namespace>_<claim>`, ownership preserved).
6. **Restore hooks** load the module dumps (replace-all).
7. **Erasure replay.** The ledger is read again, united with the copy in the snapshot, and each erasure made after the
   module's dump is applied by the module that reported it (one added by hand: by the modules that declare its data
   class): its `hooks.purge` script; `people`, `equity`, `board` and `entity` by their own
   `purge` command; `files` and `jobs` over `/v1/purge` while running without a route. can-core drops its queued
   events, notifications and mail that name the subject.
8. **Go live.** The ledger and the repository credentials are written to the server, queued mail is discarded (it may
   already have been sent), sessions are deleted, can-core restarts normally and puts the modules' workloads and routes
   back. A module that does not come up is reported; can-core keeps retrying it. A module with an exposed UI or
   API starts only after `can up` has written the routing facts (step 4 above): the restore says so and does not
   wait for it.

Overrides, each separate and each printed when used:

| Flag | Accepts |
|---|---|
| `--confirm-overwrite "<company name>"` | restoring over a server that is not blank. Its dumps, can-core state, Secrets and volumes of restored modules are **replaced**; the old state database is kept as `state.db.pre-restore`; modules not in the backup are left alone |
| `--snapshot ID`, `--force-older` | choosing another snapshot; skipping a newer one that fails its consistency check |
| `--without-secrets` | a snapshot made before the Secrets export: modules get new keys, sealed data is unreadable |
| `--accept-unrestored <module or namespace/claim>` | a dump or volume that cannot be brought back (no restore hook, database not backed up, a database server's volume). **It is not in the new server's backups either, and the old snapshots that hold it are deleted after 35 days: recover it before then or it is gone** |
| `--accept-unerased <module:class>` | a tombstone no module can replay, or whose purge keeps failing (`*:<class>` when no installed module holds the class). **You must erase that data by hand** |
| `--allow-no-ledger` | no ledger in the bucket (no `can` on the old server, or backups installed before it uploaded empty ones): only if no erasure was ever requested |
| `--accept-ledger-behind` | the bucket ledger lacks entries the snapshot's copy has: erasures may be lost |
| `--accept-company "<name>"` | a snapshot whose `company.yaml` names another company than the kit (renamed since the kit was made) |
| `--repo /path/restic` | a local copy of the repository instead of the bucket (its ledger is read from `/path/ledger`) |

A resume must use the same choices: a different `--snapshot`, `--repo`, `--dir`, `--force-older` or
`--confirm-overwrite` is refused, and so is running a restore that already finished. An unfinished restore's claim on the
server expires after 72 hours without a run, or as soon as can-core there is no longer in that restore; the next run
starts over with the blank-server check (so it needs `--confirm-overwrite`).

Afterwards the server is **not** backed up until `can up` has run with the `--backup-*` flags (it checks the server
itself, not what the laptop remembers). Erasures that are not in the ledger (see below) are back: repeat them.

Snapshot choice: snapshots are walked newest first and every decision is printed. One tagged `inconsistent` is skipped,
one not tagged `can` is ignored, a newer `can` snapshot that fails its marker check stops the restore, and a transient
restic error while checking also stops it. The chosen snapshot is pinned for resumes.

Which modules lose what, today: every shipped in-house module is restored from its own consistent dump, by
`<binary> restore -replace`; `forms` replays lead erasures over `/v1/purge` ([erasure requests](../modules/forms/README.md#erasure-requests));
`finance` and `work` replay theirs with their own `purge` command
([specs/056](../specs/056-erasure-finance-work/spec.md)); `provisioning` has no purge by subject and class, so
tombstones of its classes need `--accept-unerased`. If `files` or `jobs` refuse to restore because files their
database names are missing, and those files were destroyed after the backup's dump was taken,
`--allow-missing-blobs <module>` goes on without them; otherwise it means the volume was not restored.

## Erasure tombstone ledger

can-core collects the erasures of the modules into the ledger. Once a minute it reads `GET /v1/erasures` of
`people`, `equity`, `board`, `entity`, `files`, `forms` and `jobs` (from the start each time, so a restored module is read
correctly), and appends what is new to a ledger in the dumps directory (`.erasure-ledger/`, never part of the dumps
or a snapshot). On the host, `can backup ledger-import` adds it to `/var/lib/can/erasure-ledger.jsonl`; the backup
script does this nightly and `can-erasure-sync.timer` every quarter of an hour (`can-backup --ledger-only`). An entry
is `(subject id, data class, date)`; subject ids must be opaque, and one that is not (an e-mail address, say) is
refused, counted and reported with exit 22, never written or logged.

**What a module may order.** Its feed is taken only for the data classes its manifest declares (`board` and
`entity` also for `board-private` and `entity-person`, which one binary writes), each entry names the module that
reported it, and a restore applies it to that module alone. One module's feed is read for at most a minute, and given up
after 1,000 refused tombstones. Each module has a bounded share of the ledger (8 MiB,
about 30,000 erasures, 5,000 new a minute; 56 MiB in all). A full share is reported like an unreadable feed. The
ledger is never compacted, so a very large company would reach this after years.

`finance` records a party erasure as `party-<id>-<creation time>` (class `finance-party-contact`) and `work` a
person erasure as a hash of the identifier (class `work-issue`); the erase call returns that subject.

**What a restore replays.** Each entry says when the data was erased. A module dump taken after that already
reflects the erasure and may hold data created since for the same subject, so **nothing is purged from it**; only
erasures made after the dump (its `manifest.json`, else the start of the dump run), or less than a minute before
it, are replayed. A dump carried over from an earlier run without a `manifest.json` (a script hook's) is of unknown
age: the restore stops for it (`--accept-unerased`) rather than guess. can-core's own derived copies (queued events,
notifications, mail) are removed whenever the subject was erased. `files` records the
ids of the erased files, never the data subject. **`can backup ledger-add` records data that has already been erased** (it
erases nothing): dated now, or `--erased-at <RFC 3339 time>`. A snapshot made after that time is left alone, and
the restore lists each such entry as `NOT replayed`: erase the data by hand if it was not really gone. Entries without a time (added by an earlier release) are always replayed. Two limits:
an erasure made in the minute before, or while, a module's database was copied is replayed although the copy may
reflect it, which takes data created for the same subject in between; and if a
restore was told `--accept-unerased`, later backups are taken as reflecting those erasures, so they are not asked
again: erase that data by hand.

**The import and upload need the `can` binary at `/usr/local/bin/can` on the server, which `can up` does not copy
there.** Without it can-core still collects, but nothing reaches the bucket; once there is an erasure to record, every
run fails with exit 22.

**Update the `can` binary on the server with each release**: the script names one that has no `ledger-import`
(exit 22). The frequent sync calls the webhook once per fault, again after a day or after a successful run.

**Servers set up by an earlier release** need `can up` (or `can backup install`) once, for the new script and timer.

**What is not covered:**

- **Modules without an erasure feed record nothing**: `provisioning` and the wrapped apps
  (`crm`, `wiki`, `support`, `newsletter`, `status`, `analytics`). Each run of `ledger-import` names the installed ones
  that hold data classes. Something erased in them is **back** after a restore of an older backup. If a purge hook can
  replay it, erase there and record it: `can backup ledger-add --subject <id> --class <data class>`.
- **The window.** An erasure made less than about a quarter of an hour before the server is lost may not have reached
  the bucket. A failing collection or upload (exit 22, journal, webhook) makes the window as long as it lasts.
- A module that is disabled is not asked; what was collected while it ran stays. An erasure made in the last minute
  before disabling it is collected when it is enabled again.
- An erasure made after the dump by a module that is not installed in the restored company stops the restore
  until `--accept-unerased <module>:<class>` (`*:<class>` for one added by hand).

The ledger is hash-chained, so editing, reordering or removing early entries is detected.
`can backup ledger-sync` merges it into the encrypted object `ledger/erasure-ledger.v1.enc` in the bucket,
outside the `restic/` prefix. The key is derived from the restic password. Sync never shrinks the remote ledger and
refuses to overwrite one it cannot read. The first backup run uploads an empty ledger, so a restore of a company
that never erased anything needs no override; a bucket without a ledger means it was deleted or never uploaded, and
the restore stops. Requirement: the `can` binary at `/usr/local/bin/can` on the host. The S3 client
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

1. Use a throwaway account, a company with realistic data and a bucket you may delete.
2. Take a backup (`can backup run`), then `can backup verify`; note the repository size.
3. Destroy the server (`can down --yes`). With a stopwatch running, follow the five points under "Restore".
4. Stop at the first successful load of `https://<domain>/` and an admin login. Record wall-clock per step (the command
   prints `[n/8]` lines) and the total.
5. Repeat three times, once with an empty ledger and once with at least one erasure, and note the provider, region, server
   type, data size and `can`/k3s/restic versions.
6. Record the results in `specs/042-restore/measurements.md` and update SPEC.md sections 6.5 and 10. Until then the
   claim stays unproven.

## The shared Postgres

can-core runs a Postgres in `can-data` for modules that need one (today `support` and `newsletter`,
[specs/041](../specs/041-wrapped-modules-live/spec.md)).

- Its data directory (`pvc-*_can-data_postgres-data`) is excluded from the file backup, because a copy of a
  running server's files is not a backup.
- Each module's database is in the backup as that module's [dump](#wrapped-modules). A database no module dumps (one
  you made by hand, or the server's roles and settings) is **not** backed up. can-core creates the roles again.
- An operator's own server (`CAN_POSTGRES_ADMIN_URL`) is dumped the same way, and **must be major 17**. On any other
  major the dump run fails (and then nothing is backed up) and a restore into it is refused. If it runs in `can-data` with a claim named `postgres-data`, that volume is
  excluded too.
- The backup script of an existing server excludes the directory only after it is generated again
  (`can backup install` or `can up`).

## What is and is not in a snapshot

- In: `can-core/state.db`, `can-core/secrets.json`, `company.yaml`, each module hook's dump, the local-path volumes (except can-core's
  live database, the SQLite files of the modules dumped in that run and the shared Postgres's data directory), a copy of the k3s datastore, node
  token and encryption key, the erasure ledger copy.
- A module without a backup hook that keeps SQLite on a volume is covered only by the raw copy of live files, which
  may be inconsistent. `can backup install` warns. Every shipped module has a hook.
- Not there: the site's content (it is built from your company repository), database servers' files, the content of an
  S3 bucket used by files, and anything outside the platform's directories.

## Measured (k3d in CI)

Workflow `make-up-e2e` (advisory) runs: dump request, the installed backup script against a local restic
repository, `can backup status` and `verify`, and a restore of the dumps directory. `dev-cluster` (advisory) runs
a module backup hook Job. `wrapped-backup` dumps analytics, support and newsletter, deletes them and the database
server, and restores them with can-core's restore operations (not through restic). `restore-e2e` backs up a company, destroys the cluster and restores it onto a new one
([specs/042-restore](../specs/042-restore/spec.md)). The numbers are in
[specs/037-backup-dumps/measurements.md](../specs/037-backup-dumps/measurements.md).

## Not verified (honest list)

- A real S3-compatible bucket (R2, B2, S3). CI uses a filesystem repository: the script's repository line is the
  one thing changed. The SigV4 ledger client has only met an `httptest` server.
- Hook file permissions against the real module images (the hook runs as uid 1000; files another user wrote with
  mode 0600 would be unreadable).
- A real server: the systemd unit and timer (`ProtectSystem=strict`, `ReadWritePaths`), bootstrap's directory
  ownership, `apt` installing `sqlite3`, and the script reading the live k3s paths. CI runs the script on the
  runner against copies of the k3d node's directories.
- **Restore on a real server**: creating the server, systemd (stopping an installed timer on an overwrite),
  `--release-manifest` and `--images-from` image loading, a real k3s from the bootstrap script, a real bucket (CI uses
  `--repo` with a local copy), a restore through restic of database modules, purge replay of `equity`, `board`,
  `files` and `jobs` on a cluster, `--confirm-overwrite` on a cluster, and disk use and duration with real data sizes.
- The shipped script hooks (wiki, crm, status): CI builds the `can-hooks` image and
  parses them in it, but the image is not published. A source checkout ships it (`make images`, `can up
  --images-from`); without it `can up` and `can apply` refuse to enable these modules, and one enabled anyway fails
  the dump run (no marker is written) until disabled. None of these hooks has run against its application. A release
  publishes the image (docs/releasing.md).
- In-house hooks on a cluster: backup of forms, work and files (`module-backup` in CI), backup and restore through
  `can restore` of forms, work and people (`restore-e2e`). jobs, finance and provisioning are covered by unit tests;
  equity, board and entity are only installed on k3d, not backed up or restored there.
- `can-hooks` Jobs against a database module. Only `crm`'s chart still names a `can-database` Secret that
  can-core does not write.
- The wrapped modules' dumps (analytics, support, newsletter) on a real server, with large data, or against an
  external Postgres.
- Disk use: uploads are capped per file and per module and run, but there is no total quota on the dumps
  directory and a run holds the old and the new set at once. Hooks retry with `--retry` only, which does not cover
  a connection refused while a new pod's network rules are still being programmed.
- The erasure ledger on a real server and bucket: `can-erasure-sync.timer` under systemd has not run anywhere (CI runs
  its command), and the collection has run on k3d only.
- The restore-time target (15 minutes) and the effect of data size on dump and backup time.
- Whether the 35-day retention fits your legal obligations is for you to decide.
