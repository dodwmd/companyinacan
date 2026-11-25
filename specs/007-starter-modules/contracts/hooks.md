# Contract: backup and restore hooks

The dump endpoint was fixed by epic E08b (specs/037-backup-dumps/contracts). The scripts and this file
change together.

## Execution

can-core mounts `modules/<id>/hooks/*.sh` as a ConfigMap and runs the hook as a Job in `mod-<id>`
with image `hooks.image` (`ghcr.io/dodwmd/can-hooks`, `deploy/hooks`). Jobs run non-root, with the
same restricted security context as the module's pods.

## Job pod requirements

- `securityContext.fsGroup` equal to the module pod's (1000), so the hooks user can write the
  module volume (analytics restore); `runAsNonRoot` and the rest of the restricted profile.
- Hook Jobs may run with a read-only root filesystem: mount an `emptyDir` at `/tmp` (scripts use
  `mktemp -d`) sized for the largest dump. The module PVC is mounted at `/data` for analytics only.
- The bearer token is passed to curl through a 0600 header file (`-H @file`), never on argv;
  every transfer has `--connect-timeout 10 --max-time 600`.
- analytics `restore.sh` refuses to run unless `/data` is a mount point.

## Environment

| Variable | Source | Used by |
|---|---|---|
| `CAN_API_URL` | `can-module` Secret key `api-url` | all |
| `CAN_MODULE` | module id | all |
| `CAN_TOKEN_FILE` | default `/var/run/secrets/can/token` (projected, audience `can`) | all |
| `DB_HOST DB_PORT DB_NAME DB_USER DB_PASSWORD` | `can-database` Secret | support, newsletter |
| `GOATCOUNTER_DATA` | default `/data`; the module PVC must be mounted there | analytics |

## Transport

- Backup: `PUT $CAN_API_URL/v1/dumps/$CAN_MODULE/<name>` with `Authorization: Bearer <token>` and
  the gzip body. Names: `database.sql.gz`, `goatcounter.sqlite3.gz`.
- Restore: `GET` the same URL.
- Exit status is the result: non-zero means failure. can-core, not the hook, owns the completion marker.

## Script rules (tested)

1. `#!/bin/sh`, `set -eu`, parses with `sh -n`.
2. Every required variable checked with `: "${VAR:?}"`.
3. The dump is written to a file in a `mktemp -d` directory removed by a trap; dump commands are
   never the left side of a pipe (POSIX sh has no `pipefail`).
4. Plausibility and integrity before upload: size above a floor and `gzip -t`; SQLite also
   `PRAGMA integrity_check`.
5. Restore verifies the download before touching the target; SQL restore runs with `ON_ERROR_STOP`
   (Postgres) or over a dump with `DROP TABLE IF EXISTS` (MariaDB); SQLite replaces the file by an
   atomic rename after the integrity check and removes stale `-wal` and `-shm`.
6. Secrets only through environment variables (`MYSQL_PWD`, `PGPASSWORD`), never on a command line.

## Per module

| Module | Dump | Consistency |
|---|---|---|
| support | `mariadb-dump --single-transaction --quick --routines --triggers --no-tablespaces` | InnoDB snapshot |
| newsletter | `pg_dump --no-owner --no-privileges --clean --if-exists` | one transaction |
| analytics | `sqlite3 .backup` (online backup API) | consistent under WAL |

Attachments (support) and uploads (newsletter) live on the PVC and are covered by the raw PVC
backup (SPEC 6.5); SQLite files are excluded from it, so the analytics dump is its only copy.
