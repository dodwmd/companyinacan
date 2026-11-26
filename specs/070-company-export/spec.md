# Feature specification: company data export (epic E18b)

**Branch**: `epic/E18b-export` | **Input**: SPEC.md "Own your data" and section 6.4 (`can export`),
section 6.5, [specs/037](../037-backup-dumps/spec.md), [042](../042-restore/spec.md),
[043](../043-module-backup-hooks/spec.md). User documentation: [docs/export.md](../../docs/export.md).

## User scenarios

1. **Take my data** (P1). The operator runs `can export` and gets one file on their machine with the
   configuration, the website, every module's records and files and the audit logs, readable without
   this project.
2. **The owner without a shell** (P1). The owner downloads the same from the admin UI after
   confirming who they are.
3. **Know what is missing** (P1). Nothing is left out silently, and a partial or broken export is
   never mistaken for a whole one.

## Requirements

- **FR-001** The archive is tar.gz; records are JSON Lines and CSV with a `schema.json`; files are
  their own bytes; `manifest.json` is the last entry.
- **FR-002** `<binary> export` in every in-house module, run as a Job like the backup hook: the
  module's image, restricted pod security, the module's memory limit, no Secret.
- **FR-003** Credentials and key material are never exported. Every column of every in-house table
  is classified; one that is not is left out and warned about, and a test fails for it.
- **FR-004** Sealed fields are decrypted only with `--include-sealed`; only then does a Job get the
  keyring (finance: the seal key).
- **FR-005** CSV cells cannot be run as formulas.
- **FR-006** Wrapped applications give their own data in their own schema, without their accounts and
  secrets, with notes.
- **FR-007** The export is written to the operator's side as it is produced; the server keeps no copy.
  Optional passphrase encryption, from the standard library and `golang.org/x/crypto`.
- **FR-008** Audit entry before data leaves, and one for the outcome. Admin UI: `platform.export`,
  step-up, CSRF.
- **FR-009** Erased data is absent.

## Decisions

| | Decision | Why |
|---|---|---|
| D1 | The module's upload is copied entry by entry into the operator's stream (`PUT /v1/exports/{module}`), not stored as a dump | nothing is left on the server and no volume has to be sized; the cost is that a Job cannot retry and lasts as long as the download |
| D2 | `can export` reaches can-core with `kubectl exec` (over ssh, or `--context`) and a Unix socket next to the state database | no new network listener or credential; the export must run in the serving process, which receives the uploads |
| D3 | Rows are written twice from one snapshot (count, then write) | tar needs sizes up front; no plaintext is staged on disk |
| D4 | Allow-list of columns per service, in `core/modbackup` and `companyexport` | a column added later is not exported by accident |
| D5 | Any module failing fails the export | a silently partial export defeats the purpose; `--skip` is explicit |
| D6 | No import | `can import <file>` of SPEC 6.4 remains the CSV import of three record types; a company import is not designed |
| D7 | `platform.export` is a permission of its own, held by `owner` only by default | an export is more than any other platform permission can read |

## Success criteria

- **SC-001** Unit tests for FR-002 to FR-009.
- **SC-002** k3d in CI: forms, work and files (`module-backup`), analytics, support and newsletter
  (`wrapped-backup`).

## Not verified / out of scope

- A real server; export Jobs of people, jobs, finance, equity, board and provisioning on a cluster;
  memory and time on large data.
- wiki, status and crm: their script hooks have no export; the manifest says so.
- Module data held outside the module (an `s3` files bucket) is listed, not copied.
- The wrapped applications' secret rules go by table, column and file names and have not been
  audited against every version of their schemas.
- A dump run that starts while an export runs is not held back.
- Company import; resumable or incremental export.
