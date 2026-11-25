# Implementation Plan: Backup, Restore and Recovery Kit (Epic E08)

**Branch**: `epic/E08-backup-restore` | **Date**: 2026-11-25 | **Spec**: [spec.md](spec.md)

## Summary

Add `core/backup` (plan, script and unit generators, restic Runner, verify and status, recovery kit,
erasure ledger with an S3 object client, restore state machine) and the `can backup` and `can restore`
commands. Decisions are in [research.md](research.md), entities in [data-model.md](data-model.md),
interfaces and formats in [contracts/](contracts/).

## Technical Context

**Language/Version**: Go 1.27 (existing module)

**Primary Dependencies**: uses `golang.org/x/crypto` (argon2, chacha20poly1305, hkdf; BSD-3-Clause), already a direct
dependency on main, so no go.mod change. Nothing else; S3 is a 100-line SigV4 client over `net/http`.

**Storage**: restore progress in `.can/restore.json` (0600); host files under `/etc/can/backup.d` (0600).

**Testing**: `go test`; restic behind `Runner` (`FakeRunner`) or `ResticAPI` (`FakeRestic`); the S3 client
via `httptest`; generated script by golden files, `bash -n`, shellcheck, `systemd-analyze verify`
(each skipped when absent) and by running it in a sandbox with a fake `restic`.

**Constraints**: no real bucket, restic, host or cluster; no dependency on E03 packages.

## Constitution Check

| Principle | Before research | After design |
|---|---|---|
| I. Spec-first | SPEC.md 6.5 is the source; the six steps are mirrored 1:1 | Two refinements recorded in research.md (R3 ledger upload via `can backup ledger-sync`, R6 unavailable adapters). No SPEC change needed. |
| II. Footprint | Host-side only: no resident component is added (timer + oneshot) | Unchanged. restic and sqlite3 are installed on the host by bootstrap (E06 follow-up). |
| III. Test-first | Tests written per unit before the code (see tasks.md); k3d restore test is a gap | Offline tests complete; the k3d integration test remains human/CI-gated and is listed unverified. |
| IV. Stable module contracts | Uses only `hooks.*` already in module.v1; no schema change | Added `Hooks` to the Go `config.Module` struct only. |
| V. Secure and private | Credentials by file, ledger has no PII and is encrypted, kit never in repo | Argon2id parameters fixed by format; tampered headers cannot request memory; script quoting tested. |
| VI. Open-source hygiene | New dep is BSD-3 | Signed commits; no real credentials in tests. |
| VII. Simplicity, measured claims | No restore-time claim is made | The 15-minute target is a human-gated measurement (docs). |

**Result**: pass. Complexity: no new dependency (stdlib has no argon2/XChaCha20; x/crypto is already required).

## Project Structure

```text
core/backup/
  plan.go        Settings, Plan, BuildPlan
  marker.go      completion marker
  script.go      backup script, service and timer generators
  restic.go      Runner, ExecRunner, FakeRunner, Restic wrapper, env file loader
  verify.go      manifest, Verify, Status
  fake.go        FakeRestic
  kit.go         recovery kit: generate, print once, encrypt, parse
  ledger.go      erasure ledger, sealing, ObjectStore, S3Store
  restore.go     Restore state machine, MatchPVCs, CopyTree
core/cli/backup.go   can backup ..., can restore
```

Shared files touched: `core/config/module.go` (Hooks), `core/cli/root.go` (two lines), `docs/epics/README.md` (status).
