# Contract: in-house module backup and restore hooks

For epic E08c (`can restore`) and anyone adding an in-house module. The shell-script contract of the wrapped
modules (specs/007-starter-modules/contracts/hooks.md) is unchanged.

## Manifest

```yaml
hooks:
  command: /can-forms   # binary in the module's own image
  backup: backup        # subcommand names, not paths; no hooks.image
  restore: restore
```

## Command line

```
<command> backup  [-config FILE] [-out DIR]
<command> restore [-config FILE] [-from DIR] [-blobs DIR] [-replace] [-allow-missing-blobs]
```

| | |
|---|---|
| Config | `-config`, else the service's `CAN_*_CONFIG`, else `CAN_BACKUP_CONFIG`, else the default path. Only `db_path`, `resume_dir` and `backend` are read; the last two paths may be absent (built-in defaults) |
| Secrets | none are needed or read |
| Without `-out` / `-from` | `CAN_API_URL`, `CAN_MODULE`, token in `CAN_TOKEN_FILE` (default `/var/run/secrets/can/token`): `PUT` / `GET /v1/dumps/$CAN_MODULE/<name>` |
| Exit | 0 ok, 1 failed, 2 usage or configuration, 3 (backup) there is no database |

## Dump set

| Name | Content |
|---|---|
| `database.sqlite3.gz` | gzip of the `VACUUM INTO` copy |
| `blobs.tsv.gz` | files and jobs only: `key<TAB>size<TAB>sha256` per referenced file |
| `manifest.json` | uploaded last: `format` (1), `binary`, `module`, `taken_at`, `database{name,size,sha256}`, `blobs{backend,count,bytes,name,size,sha256}`; sizes and hashes are of the uncompressed content |

## Backup Job (can-core, `core/server/dumps/native.go`)

Namespace `mod-<id>`, image of the container `app` of Deployment `<id>` (chart image if there is none),
command `[<command>, backup]`, the module's uid (65532), claim `<id>-data` read-write at `/data` (a WAL reader
writes the shared index), optional ConfigMap `<id>-config` at `/etc/can-hook-config`, `emptyDir` at `/tmp`
(needs room for the copy of the database), projected token audience `can`, memory limit `ram_limit_mb`.

## What `can restore` does with this

Implemented in specs/042-restore.

1. **Secrets first.** Put each module's keys back **before** the apply that would create them: can-core keeps an
   existing `can-module` Secret and generates new values otherwise, and new keys cannot open the old data.
2. Keep the module at 0 replicas.
3. **Volume data before the hook.** The backed-up volume is copied into the new claim first (it holds blobs and
   resumes; the dumped SQLite files are not in it), then the hook runs. `-blobs` is therefore not used.
4. Run `[<command>, restore, -replace]` in the same Job shape as backup (module scaled to 0, claim `<id>-data`),
   with the same environment. It downloads the set from `GET /v1/dumps`, which only a can-core in restore mode
   serves, or reads `-from DIR`.
5. Run the erasure replay (`purge`) after the hooks and before scaling up.
6. Use the module version that made the dump, or a newer one: the service migrates on start, and `restore` refuses a
   dump whose `schema_version` is newer than the binary knows.

`-allow-missing-blobs` (`can restore --allow-missing-blobs <module>`) is only right for files destroyed after the dump was taken; otherwise it means the volume
was not restored. A `restore` of files with the s3 backend checks nothing in the bucket.

## Keys a restore needs

None of these is in a module's dump. can-core exports every Secret of each module namespace into the backup
(`can-core/secrets.json`, specs/042-restore) and `can restore` re-creates them before the modules. Snapshots made
before that export hold them only inside the k3s datastore copy (`host-state`).

| Module | Secret (in `mod-<id>`) | Without the original |
|---|---|---|
| people | `can-module`: `keyring` | sealed personal fields are unreadable, permanently |
| jobs | `can-module`: `keyring`, `CAN_JOBS_HASH_KEY` | candidate data unreadable; duplicate detection splits every candidate |
| files | `can-module`: `keyring`; `CAN_FILES_LINK_SECRET` | names and descriptions unreadable; share links stop working |
| finance | `can-module`: `CAN_FINANCE_SEAL_KEY`; `CAN_FINANCE_LINK_SECRET` | sealed bank account numbers unreadable; invoice links stop working |
| equity | `can-equity-keys` (operator-made) | sealed fields unreadable |
| board, entity | `can-board-keys` (operator-made) | sealed fields unreadable |
| forms, work, provisioning | `can-module` API tokens and signing keys | data is readable; static tokens change. Connector credentials of provisioning are the operator's |

A keyring that was rotated must be restored with all its keys, not only the newest.
