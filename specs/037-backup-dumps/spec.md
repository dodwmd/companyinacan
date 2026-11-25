# Feature specification: backups that work end to end (epic E08b)

**Branch**: `epic/E08b-backup-dumps` | **Input**: SPEC.md section 6.5, specs/008-backup-restore, the known gap in
specs/033-make-up-e2e (can-core could not write the host dumps directory, so every backup run exited 10).

## User scenarios

1. **Operator turns backups on** (P1). `can up --backup-endpoint --backup-bucket --backup-credentials-file` succeeds
   without an override, installs restic and the timer, and hands out a recovery kit without a caveat.
2. **Nightly backup** (P1). can-core produces a consistent set of dumps and then the completion marker; the host
   timer backs them up with restic.
3. **A module's database is in the set** (P1). The module's `backup` hook runs in the module's namespace and uploads
   through the module API. If it fails, there is no marker and nothing claims to be consistent.
4. **Operator checks** (P2). `can backup status` and `can backup verify` work against the result.

## Requirements

- **FR-001** can-core writes `can-core/state.db` (consistent copy), `company.yaml` and module dumps to the host
  dumps directory, without any pod `hostPath` and without new can-core permissions to create volumes.
- **FR-002** The marker is deleted at the start of a run and written only after every part succeeded: every
  recorded module with a hook and data uploaded something in this run, and the set is on disk (fsync).
- **FR-003** `PUT|GET /v1/dumps/{module}/{name}`: projected-token auth; a module reaches only its own dumps; uploads
  only while its own hook runs, re-checked before the file is committed; capped count and bytes; downloads only in
  restore mode (none exists yet); names restricted; atomic, 0600, size-capped; no links ever created.
- **FR-004** Hooks run as Jobs meeting Pod Security `restricted`, in the module's namespace.
- **FR-005** The backup script's guarantees from specs/008 are unchanged (marker re-check, `inconsistent` tag, ledger
  copy, root-owned host-state, symlink refusal exit 13, `forget --group-by ''`).
- **FR-006** The host gets a restic of 0.17 or newer, pinned by version and sha256.
- **FR-007** `--allow-broken-backup` is accepted and does nothing.
- **FR-009** Raw SQLite exclusions follow what was dumped in the run, never the install-time plan.
- **FR-010** The operator is told what backups do not do (restore, live-file SQLite, unrunnable hooks) and sees the
  last failed run.
- **FR-011** An older install is redone or warned about, and the directory ownership repaired, by `can up`.
- **FR-008** Secrets (restic password, bucket credentials) stay in root-only files; never in arguments or logs.

## Success criteria

- **SC-001** On k3d in CI: dump, marker, snapshot tagged `can`, status and verify pass, the dumps restore and the
  database passes `integrity_check`; a missing marker exits 10.
- **SC-002** On k3d in CI: a hook Job uploads a module dump with its projected token.

## Out of scope

`can restore` adapters (restore mode, state import, module restore, PVC restore); a Secrets export; building the
`can-hooks` image; backup hooks for the in-house modules; real S3 and real server verification.
