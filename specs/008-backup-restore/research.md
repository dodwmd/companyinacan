# Research: Backup, Restore and Recovery Kit (Epic E08)

**R1. Completion marker format.** Two `key=value` lines (`run_id`, `finished_at` as epoch seconds). Chosen so
the bash script reads it with `sed` and no JSON tool. Future-dated markers beyond 5 minutes are rejected.

**R2. Retention = erasure horizon.** SPEC.md section 5 promises that erased data leaves backups after the
retention horizon (35 days). The policy is therefore only `--keep-within 35d`: keep-daily/weekly/monthly
would keep data past the promise. Trade-off: no long-term archive; an operator can add one by changing
the policy, with the documented consequence for erasure.

**R3. Ledger upload.** SPEC says the host timer uploads the ledger. The host has no S3 CLI, so the script
calls `can backup ledger-sync`, which uses a minimal SigV4 client (PUT/GET of one object). Alternative
rejected: a second restic repository (its snapshots would be pruned by policy and it adds a password).
Consequence: the `can` binary must be on the host (`CanBin`, default `/usr/local/bin/can`); until it is,
the script exits 22 after taking the snapshot, which is loud rather than silent.

**R4. Verify without a second source of truth.** The script writes `MANIFEST.sha256` (sha256sum format) into
the root-owned host-state directory (not the can-core-writable dumps directory) just before `restic backup`, so the manifest is part of the snapshot. `verify` restores
the manifest and a sample of files and compares hashes. Restore step 2 hashes all restored dumps against the
same manifest before can-core imports them. This detects repository and restore corruption, not a dump that
was already wrong when taken.

**R5. Kit encryption.** age is not in the standard library; the brief allows argon2id plus XChaCha20-Poly1305
from `x/crypto`. Parameters (t=3, m=64 MiB, p=4) are fixed by the format and checked on read, so a crafted
file cannot demand arbitrary memory. The header is authenticated as AAD.

**R6. Cluster and Provisioner adapters.** E03's packages are not in main. `can restore` ships
`unavailable` adapters that return `provider.ErrNotImplemented` (exit 3) naming E03; `--plan` works fully.
Progress is recorded so a restore started once the adapters exist resumes normally.

**R7. Resumability.** State file records done steps, the pinned snapshot, the module list and how many
ledger entries were already purged. A fingerprint of repository and password prevents mixing kits. Steps
are idempotent by contract of the interfaces; the PVC step re-copies over existing files.

**R8. Kit never in the repo.** `WriteKitFile` walks up looking for `.git` and refuses. Printing needs the typed
word `show` first and a typed `saved` afterwards. This is a guard against accidents, not against a hostile operator.

**R9. SQLite exclusions.** PVC SQLite files (`*.db`, `*.sqlite*`, `-wal`, `-shm`, `-journal`) are excluded only
for modules with a `hooks.backup`; without one they are backed up raw with a warning, because excluding them
would silently lose the data.

**R10. systemd hardening.** Kept minimal (`PrivateTmp`, `ProtectHome`, `NoNewPrivileges`, idle IO class). `ProtectSystem`
was not used: the script writes to `/var/lib/can`, `/run` and the dumps directory and reads `/var/lib/rancher`;
tightening it is a follow-up once measured on a real host.

**R11. Consistency of a snapshot.** The marker is copied into host state before the backup and re-read after it; a change
means can-core began a new dump run, so the snapshot is tagged `inconsistent` and exit 11 is returned. Restore only considers
snapshots tagged `can` and not `inconsistent`, probes each candidate's two marker files into a scratch directory and falls
back to older good snapshots. The probe restores only two small files per candidate (at most ten).

**R12. Ledger rollback.** A copy of the ledger is part of every snapshot. At restore the union with the bucket copy is applied and a
bucket copy lacking snapshot entries is refused. Not detectable: an erasure never synced before the server was lost. Mitigation is
operational (bucket versioning or object lock; sync at the start of each run).

**R13. Root script and the dumps directory.** can-core can write the dumps directory, the script runs as root. It therefore keeps
everything it writes in a root-owned directory outside it, refuses symlinks and a world-writable dumps dir, and never writes into it.
Owner/mode of can-core's files are not otherwise verified; a hostile can-core can still put wrong data in a dump (out of scope here).

**R14. Blank-host guard.** `can restore` refuses non-empty dumps/host-state directories and never deletes a staging directory it did not
create (marker file, depth check) nor overwrites a volume that already holds data; `--force` overrides the first and the last.
