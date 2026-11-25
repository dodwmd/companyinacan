---

description: "Task list for Epic E08: Backup, restore and recovery kit"
---

# Tasks: Backup, Restore and Recovery Kit (Epic E08)

**Input**: [spec.md](spec.md), [plan.md](plan.md), [research.md](research.md), [data-model.md](data-model.md), [contracts/](contracts/)

**Tests**: Constitution III. Test tasks precede their implementation. `[V]` marks work that needs a real
bucket, host or cluster and is **not** done (see docs/backup-restore.md).

## Phase 1: Plan and generators (US1)

- [x] T001 Tests: `BuildPlan` dumps, exclusions, warnings, unknown module, `Settings.Validate` in `plan_test.go`
- [x] T002 `plan.go`
- [x] T003 Tests: marker round trip, freshness, malformed in `marker_test.go`; `marker.go`
- [x] T004 Tests: golden script/service/timer, properties (no credentials, marker before backup), quoting, `bash -n`, shellcheck, `systemd-analyze verify` in `script_test.go`
- [x] T005 Tests: script run in a sandbox with fake restic (exit 10/11/12/20, success) in `script_run_test.go`
- [x] T006 `script.go`

## Phase 2: restic, verify, status (US2)

- [x] T007 Tests: argv/credentials, `EnsureInit`, env file, `ExecRunner`, `Verify` (ok, corruption, failures), manifest traversal, `Status` in `restic_test.go`
- [x] T008 `restic.go`, `verify.go`, `fake.go`

## Phase 3: Recovery kit (US3)

- [x] T009 Tests: generate, validate, text round trip, encryption (wrong passphrase, tamper, weak passphrase), print once, git-tree refusal, password file in `kit_test.go`
- [x] T010 `kit.go`

## Phase 4: Ledger (US5)

- [x] T011 Tests: append/idempotency/PII rejection, tamper detection, seal/open, merge, sync never shrinks, SigV4 client via httptest in `ledger_test.go`
- [x] T012 `ledger.go`

## Phase 5: Restore (US4)

- [x] T013 Tests: order, resume, mid-ledger resume, ledger missing/failing, corrupt dumps, unmatched PVC, foreign state, read-only plan, `MatchPVCs`, `CopyTree` in `restore_test.go`
- [x] T014 `restore.go`, `chown_unix.go`, `chown_other.go`

## Phase 6: CLI and docs

- [x] T015 Tests: kit print/out/git refusal, restore plan/stdin/wrong passphrase/not-implemented, status, verify, install, ledger-add in `core/cli/backup_test.go`
- [x] T016 `core/cli/backup.go`; register in `root.go`; `Hooks` in `core/config/module.go`
- [x] T017 `docs/backup-restore.md` with the restore-time measurement procedure and the unverified list

## QA round 1 (PR #12)

- [x] T023 Marker re-check and `inconsistent` tagging; restore requires a consistent marker and picks a good snapshot
- [x] T024 Ledger sync independent of the backup; restic exit 3 handled; ledger copy in every snapshot, rollback refused
- [x] T025 `forget --group-by ''`; host state in a root-owned directory; symlink and permission refusals; unit hardening and OnFailure
- [x] T026 Blank-host guard, staging validation, volume overwrite guard; kit passphrase confirmation and read-back, existing password and repository checks, option allow-list
- [x] T027 Webhook URL off argv, JSON escaping, quoted env file, S3 https/NoSuchKey/path prefix, torn ledger line, restore state fsync and snapshot pin rules

## Not done

- [ ] T018 [V] Real can-core `Cluster` and `Provisioner` adapters (E03, E06 follow-up)
- [ ] T019 [V] Bootstrap installs restic and sqlite3 and calls `can backup install` (replace the `backup-placeholder` step)
- [ ] T020 [V] k3d integration test of a restore; run against real S3/R2/B2
- [ ] T021 [V] Measured restore time (human-gated procedure in docs/backup-restore.md)
- [ ] T022 [V] `make up` prints the kit (wire `can backup kit` into E06's up flow)
