# Feature Specification: Backup, Restore and Recovery Kit (Epic E08)

**Feature Branch**: `epic/E08-backup-restore`

**Created**: 2026-11-25

**Status**: Implemented offline; everything needing a real bucket, restic, host or cluster is listed in
[docs/backup-restore.md](../../docs/backup-restore.md) as unverified.

**Input**: Epic E08 in `docs/epics/README.md`: "Dump endpoints (PUT/GET) and hostPath dumps directory; host
systemd timer running restic with completion-marker check; backup scope and PVC handling; recovery
kit printed at `make up`; `can restore --kit` implementing the six-step sequence in `SPEC.md` section 6.5;
erasure tombstone ledger; `can backup verify`."

This feature covers the part of E08 that can be built and verified without a real S3 bucket, restic
binary, server or cluster, in package `core/backup` plus the `can backup` and `can restore` commands.
The dump endpoints and the can-core side (marker writing, state import, hook Jobs, scaling) belong
to can-core (E03) and are reached through small interfaces defined here, so this package does not
depend on E03's packages.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Nightly encrypted backup I can trust (Priority: P1)

An operator installs the backup on the host. A systemd timer runs a generated script that refuses to
back up unless can-core finished a dump run recently, backs up dumps, PVC data and host state with
restic, applies the 35-day retention, and reports failures loudly.

**Why this priority**: without a trustworthy backup nothing else in this epic matters.

**Independent Test**: run the generated script in a sandbox with a fake `restic`; assert exit codes and
the restic calls. Golden files, `bash -n`, shellcheck and `systemd-analyze verify` check the text.

**Acceptance Scenarios**:

1. **Given** no completion marker, **When** the script runs, **Then** it exits 10 and never calls `restic backup`.
2. **Given** a marker older than 24 h, **Then** exit 11, no backup.
3. **Given** another run holds the lock, **Then** exit 12.
4. **Given** a fresh marker, **Then** it takes a manifest of the dumps, runs `restic unlock`, `backup`, then
   `forget --keep-within 35d --prune`, and records success; a failed backup exits 20 and skips forget.
5. **Given** any failure, **Then** it is logged to journald (tag `can-backup`) and POSTed to the webhook if configured.
6. The unit and script contain no credentials: they are read from root-only files.

### User Story 2 - Know the backup is restorable (Priority: P1)

`can backup verify` runs `restic check`, restores a sample of dump files from the latest snapshot into a
temp directory, and compares SHA-256 hashes with the manifest taken at backup time.

**Acceptance Scenarios**: a clean repository passes; a corrupted file, a failed check, an empty repository
or a missing manifest each fail with exit 1; the temp directory is always removed.

### User Story 3 - Recovery kit (Priority: P1)

`can backup kit` generates the kit (restic password, bucket credentials, provider info), shows it once
after a confirmation prompt, or writes it as a passphrase-encrypted file. It is never written into the
company repository.

**Acceptance Scenarios**: a kit round-trips through print/parse and through encryption; wrong passphrase
and tampering are rejected; the target path inside a git working tree is refused; existing files are
not overwritten; secret-looking provider options are not copied into the kit.

### User Story 4 - Restore onto a blank host (Priority: P1)

`can restore --kit <file|->` runs the six steps of SPEC.md section 6.5 as a resumable state machine:
provision; dumps first and can-core state import; restore apply; module restore hooks; PVC data;
erasure ledger then scale-up. `--plan` prints the steps and changes nothing.

**Acceptance Scenarios**: steps run in order and scale-up is last; a failed step is retried on re-run
without repeating completed steps; the snapshot is pinned on first run; restored dumps are hash-checked
against the manifest before can-core imports them; PVC directories are matched on `<namespace>_<claim>`
and an unmatched backed-up volume is an error; a missing ledger or a failing ledger fetch blocks going
live (`--allow-no-ledger` covers only a missing ledger); a state file from another kit is refused.

### User Story 5 - Erasure tombstone ledger (Priority: P2)

An append-only, hash-chained file of (subject id, data class, date) written by can-core, merged into an
encrypted object in the bucket outside the restic prefix, and re-applied on restore.

**Acceptance Scenarios**: PII-shaped subject ids are rejected; duplicates are idempotent; tampering,
truncation at the head and reordering are detected; the sealed object is unreadable without the restic
password; sync never shrinks or overwrites an unreadable remote ledger.

## Requirements

- **FR-001** Backup plan derived from company.yaml and module manifests: dumps from `hooks.backup`, PVC root,
  SQLite exclusions only for modules whose database is covered by a dump, warnings otherwise.
- **FR-002** Generated script and units as in User Story 1; exit codes 10, 11, 12, 20, 21, 22 documented.
- **FR-003** restic runs only behind a `Runner`; credentials as files or process environment, never argv.
- **FR-004** `can backup run|status|verify|kit|install|ledger-add|ledger-sync`, `can restore`.
- **FR-005** Restore is orchestrated over `Provisioner`, `Restic`, `Cluster` and `LedgerSource` interfaces.
- **FR-006** Ledger entries carry no PII; the sealed object key is derived from the restic password.
- **FR-007** Honest status: anything not run against a real system is listed as unverified.

## Out of scope (not done here)

Dump HTTP endpoints, marker writing, state import, hook Jobs and scaling (can-core, E03); the real
`Provisioner`/`Cluster` adapters; replacing the bootstrap placeholder; the measured restore time
(human-gated, see docs/backup-restore.md); the footprint and k3d integration tests.
