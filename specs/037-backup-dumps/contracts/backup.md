# Contracts

## HTTP (module API, :8090)

`PUT|GET /v1/dumps/{module}/{name}`, in specs/005-can-core-mvp/contracts/module-api.yaml.

## CLI

| Command | Behaviour |
|---|---|
| `can-core dump-request [--wait D]` | ask the running can-core for a dump run; with `--wait`, exit 1 unless a new marker appears within D |
| `can up --backup-*` | installs dumps directory ownership, pinned restic, timer; no override needed |
| `can up --allow-broken-backup` | deprecated, prints a note, does nothing |

## Environment (can-core)

`CAN_DUMPS_DIR` (unset: no dumps), `CAN_DUMP_AT` (`HH:MM` UTC, default `02:30`), `CAN_DUMP_START_DELAY_SECONDS`
(default 600), `CAN_DUMP_REQUEST_FILE` (default `dump.request` next to the state database).

## Cluster objects

| Object | Notes |
|---|---|
| PersistentVolume `can-dumps` | hostPath `/var/lib/can/dumps`, `Retain`, `claimRef can-system/can-dumps` |
| PersistentVolumeClaim `can-system/can-dumps` | `storageClassName: ""`, `volumeName: can-dumps` |
| ConfigMap `mod-<id>/can-hook-backup` | the module's hook script |
| Job `mod-<id>/can-hook-backup-<n>` | restricted, uid 1000, `backoffLimit 0`, deadline 15 min, deleted when finished; mounts `<id>-data` (or the only claim) at `/data` and `/storage` |
