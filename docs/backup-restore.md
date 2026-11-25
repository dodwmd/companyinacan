# Backup, restore and the recovery kit

Implements the offline-verifiable part of epic E08 (SPEC.md section 6.5). This is a tool, not legal or
compliance advice; the 35-day horizon below is a default you must review against your own obligations.

> **`can restore` is not functional yet.** The provisioning and can-core adapters (epic E03) do not exist in this
> release. A real run stops at step 1 with exit code 3; only `can restore --plan`, kit parsing and the state
> machine itself (tested against fakes) work. Do not rely on a restore until those adapters ship and the
> measurement below has been done.

## How a backup works

1. can-core (E03) runs each module's `backup` hook nightly, writes dumps into `/var/lib/can/dumps`, deletes
   `COMPLETE` at the start of the run and writes it (run id, finish time) at the end.
2. The host timer `can-backup.timer` (default 03:30 plus up to 15 min) starts `can-backup.service`, which runs
   `/usr/local/sbin/can-backup`. The script, in order:
   - syncs the erasure ledger (first and independently, so a failing backup never holds back erasures) and copies it
     into the root-owned host-state directory so every snapshot carries a copy;
   - refuses a dumps directory that is world-writable or contains a symlink (can-core can write there, the script is root),
     and a marker that is missing or older than 24 h;
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

Install on the host: `can backup install --kit <kit file> --enable` (writes the script, unit, timer and credential
files and initialises the repository if none exists). `can backup install --plan` prints the generated script.

### Script exit codes

| Code | Meaning |
|---|---|
| 0 | ok |
| 1 | unexpected error or missing credentials file |
| 10 | completion marker missing (can-core has not finished a dump run) |
| 11 | marker older than 24 h, future-dated, malformed, or changed while the backup ran (snapshot tagged `inconsistent`) |
| 12 | another run holds the lock |
| 13 | unsafe dumps directory (symlink inside, or world-writable) or host-state directory is a symlink |
| 20 | `restic backup` failed (forget/prune is skipped) |
| 21 | forget/prune failed |
| 22 | erasure ledger upload failed (the backup still ran). If a backup code and 22 both occur, the backup code is returned |

Failures go to the journal (`journalctl -t can-backup`) and, through `OnFailure=can-backup-failure.service`, a critical journal line
when the unit fails. The unit runs with `ProtectSystem=strict` and a short `ReadWritePaths=` list. To also get a webhook, put its URL in
`/etc/can/backup.d/webhook.url` (0600); the script POSTs `{"text": "can-backup failed: (code) message"}` (JSON-escaped); the URL is given to curl on stdin, never on a command line.
`restic.env` values are single-quoted so any character in a secret survives sourcing.

## Commands

| Command | Use |
|---|---|
| `can backup status` | marker age, latest snapshot, problems; exit 1 on any problem |
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
- If `/etc/can/backup.d/restic.password` exists, its password is reused; if no such file exists, the command checks that no
  restic repository is already at the target and refuses otherwise (a fresh password would not open it).
- Only an allow-list of provider options (host, port, user, location, server type, image, region, datacenter) goes into the kit.
- The kit must never be committed. Losing it makes the backups unreadable; there is no recovery.

## Restore (`can restore --kit`)

The six steps of SPEC.md section 6.5; progress is saved in `<dir>/.can/restore.json`, so after a failure you fix the
cause and run the same command again. `--plan` shows the steps and which are done without touching anything.

1. Provision, k3s, can-core in restore mode.
2. The host must be blank: the dumps and host-state directories must be absent or empty (else refuse; `--force` overrides).
   The snapshot is chosen as the newest one tagged `can`, not `inconsistent`, whose restored `COMPLETE` marker is present and
   equals the marker copy the backup ran under; older good snapshots are the fallback. Dumps and host state are restored, every
   restored dump is hashed against the manifest, then can-core imports its state and re-creates the exported Secrets.
3. `company.yaml` is written back into `--dir` (an existing different file is never overwritten) and a restore
   apply runs.
4. Module `restore` hooks (replace-all) load the database dumps; modules stay scaled to 0.
5. PVC data is restored to a staging directory (absolute, at least 3 path components, empty or created by `can restore`; it
   is never deleted otherwise) and copied into the new volumes matched on `<namespace>_<claim>` (ownership and modes
   preserved). A volume that already holds data is not overwritten without `--force`. A backed-up volume with no matching claim
   fails the step.
6. The erasure ledger is fetched and decrypted and united with the copy stored inside the snapshot; `purge(subject, class)`
   hooks re-run (progress is tracked per subject and class), tombstones are re-applied to can-core's state, and only then
   workloads scale up. No ledger in the bucket blocks going live unless you pass `--allow-no-ledger` (only appropriate if no
   erasure was ever requested, and never enough when the snapshot holds a ledger copy). A ledger that cannot be fetched always
   blocks, and a bucket ledger that lacks entries the snapshot's copy has (rollback or truncation) blocks too (`--force` applies
   the union anyway).

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

## Not verified (honest list)

Everything below has not been run against a real system; the offline tests cover the logic through fakes.

- Real restic and a real S3-compatible bucket (R2, B2, S3): command lines and exit-code handling are tested against a fake;
  `restic cat config` exit code 10 for "no repository" is taken from restic's documentation, not observed.
- The SigV4 client against a real endpoint (it is tested against an `httptest` server that checks the header shape, not the
  signature). Path-style addressing only; some providers may require virtual-hosted style.
- The generated script on a real host: `bash -n`, shellcheck and a sandbox run with a fake restic pass; `sqlite3 .backup`,
  `flock`, `logger`, `curl` and the k3s paths were not exercised. `systemd-analyze verify` runs only where available.
- Hardening: `ProtectSystem=strict` with `ReadWritePaths` is set from reading the script, not from running it under systemd on a
  real host; if it blocks a path (for example restic's cache or sqlite's WAL for the k3s database) the first real run will show it.
- can-core side: the dump PUT/GET endpoints, marker writing, state import, hook Jobs, namespace and Secret re-creation,
  scale to 0 and up, and purge hooks (E03). `can restore` uses adapters that return "not implemented" (exit 3) until they exist.
- Provisioning during restore (step 1) through the provider package is not wired.
- The bootstrap script still installs only a placeholder in `/etc/can/backup.d`: it does not install restic or sqlite3 or
  call `can backup install`. `make up` does not yet print the kit; use `can backup kit`.
- The k3d integration test of a full restore, and the footprint check for the backup path.
- The restore-time target (15 minutes) and the effect of data size on it.
- Restoring PVC ownership requires running as root; non-root runs keep modes but not owners.
- The 35-day retention matches the default horizon in SPEC.md section 5; whether it fits your legal obligations is for you to decide.
