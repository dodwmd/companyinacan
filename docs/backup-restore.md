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
   `kubectl -n can-system exec deploy/can-core -- /can-core dump-request --wait 5m`. It:
   - deletes `COMPLETE`;
   - writes `can-core/state.db` (`VACUUM INTO`, then `integrity_check`) and a copy of the applied `company.yaml`;
   - runs each enabled module's `backup` hook as a Job in `mod-<id>` (restricted pod, projected audience-`can`
     token, the module's data volume), which uploads with `PUT /v1/dumps/<module>/<name>`. A module can only
     reach its own dumps, and uploads are refused outside a run;
   - writes `COMPLETE` (run id, finish time) **only if every hook succeeded**. One failing hook therefore stops
     backups until it is fixed; the can-core log names it and `can backup status` reports the missing marker.
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
   - runs `restic backup` over the dumps, the PVC root (SQLite files of modules covered by a dump are excluded) and the
     host-state directory, tagged `can`, `run-<id>`, `attempt-<id>`;
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

## Commands

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

## What is and is not in a snapshot

- In: `can-core/state.db`, `company.yaml`, each module hook's dump, the local-path volumes (except can-core's
  live database and the SQLite files of modules that have a backup hook), a copy of the k3s datastore, node
  token and encryption key, the erasure ledger copy.
- Modules without a backup hook (forms, work, people, jobs, finance and the other in-house modules) are covered
  only by the raw volume copy of live SQLite files, which may be inconsistent. `can backup install` warns.
- Not there: a separate export of Kubernetes Secrets (SPEC 6.5). Module keys exist only inside the k3s datastore
  copy, so recovering them needs a raw datastore restore.

## Measured (k3d in CI)

Workflow `make-up-e2e` (advisory) runs: dump request, the installed backup script against a local restic
repository, `can backup status` and `verify`, and a restore of the dumps directory. `dev-cluster` (advisory) runs
a module backup hook Job. The numbers are in
[specs/037-backup-dumps/measurements.md](../specs/037-backup-dumps/measurements.md).

## Not verified (honest list)

- A real S3-compatible bucket (R2, B2, S3). CI uses a filesystem repository: the script's repository line is the
  one thing changed. The SigV4 ledger client has only met an `httptest` server.
- A real server: the systemd unit and timer (`ProtectSystem=strict`, `ReadWritePaths`), bootstrap's directory
  ownership, `apt` installing `sqlite3`, and the script reading the live k3s paths. CI runs the script on the
  runner against copies of the k3d node's directories.
- **Restore.** `can restore` is not functional (the provisioning and can-core adapters return exit 3). Restoring
  onto a blank host, importing can-core state, module `restore` hooks and PVC data have never run. can-core has no
  restore mode: whoever implements it must keep dump runs off until the import is done.
- The shipped module hooks (wiki, support, newsletter, analytics, crm, status) and the `can-hooks` image: the
  image is not built or published, so these modules' dump runs fail (and no marker is written) until it is. The
  hook Job mechanism was run with a test module and a public curl image.
- `can-hooks` Jobs against a database module (DB credentials come from the `can-database` Secret).
- Disk use: one upload is capped at 4 GiB, but there is no total quota on the dumps directory, so a compromised
  module pod could fill the server's disk during a dump run. Hooks retry with `--retry` only, which does not cover
  a connection refused while a new pod's network rules are still being programmed.
- The erasure ledger upload (can-core does not write the ledger yet; `can backup ledger-add` does by hand).
- The restore-time target (15 minutes) and the effect of data size on dump and backup time.
- Whether the 35-day retention fits your legal obligations is for you to decide.
