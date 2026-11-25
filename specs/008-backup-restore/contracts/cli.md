# Contract: CLI

Exit codes follow `specs/001-project-foundation/contracts/cli.md`: 0 ok, 1 failure, 2 usage, 3 not implemented.

| Command | Behaviour |
|---|---|
| `can backup run [--script P]` | Runs the installed script; a script failure is reported with its exit code (table below) |
| `can backup status [--repo --password-file --env-file --dumps-dir]` | Marker, latest snapshot, problems; exit 1 if any problem; "backups are OFF" without a repository |
| `can backup verify [--sample N --read-data]` | `restic check`, restore sample to a temp dir, hash compare, temp dir removed |
| `can backup kit --endpoint --bucket --credentials-file [--out F --passphrase-file F --dir D]` | Generate; print once after `show`/`saved`, or write an encrypted file; refuses git working trees |
| `can backup install [--root R --kit F --passphrase-file F --enable --plan]` | Writes script, units, credential files; `restic init` only if no repository exists; `--enable` runs `systemctl daemon-reload` and `enable --now can-backup.timer` |
| `can backup ledger-add --subject S --class C [--ledger F]` | Append a tombstone (opaque id, no PII) |
| `can backup ledger-sync [--ledger F --repo R ...]` | Merge the local ledger into the sealed bucket object (called by the script) |
| `can restore --kit (not functional until the E03 adapters exist) <file\|-> [--plan --dir D --snapshot ID --allow-no-ledger --state F --root R --dumps-dir --pvc-root --staging --passphrase-file]` | Six-step restore, resumable; `--plan` is read-only |

Backup script exit codes: 0 ok; 1 unexpected error or missing credentials; 10 marker missing; 11 marker stale, invalid or changed during the backup;
12 another run holds the lock; 13 unsafe dumps directory; 20 restic backup failed; 21 forget/prune failed; 22 ledger upload failed (snapshot was taken).

Restore interfaces (Go): `Provisioner.Provision`, `Cluster.{ImportState, Apply, RestoreModules, ScaleDown, PVCDirs, Purge,
ReapplyTombstones, ScaleUp}`, `LedgerSource.Fetch`, `ResticAPI.{Snapshots, Restore, Check}`.
