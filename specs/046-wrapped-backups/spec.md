# Feature specification: backup and restore of the wrapped modules without a new image (epic E08e)

**Branch**: `epic/E08e-wrapped-backups` | **Input**: the gap in specs/041-wrapped-modules-live and
specs/043-module-backup-hooks: the hooks of analytics, support and newsletter named the unpublished `can-hooks`
image, so enabling any of them stopped **every** backup, and the Postgres in `can-data` was backed up by nothing.

## User scenarios

1. **Nightly backup** (P1). With analytics, support or newsletter enabled, the dump run completes and its set holds
   the module's database and files.
2. **Restore** (P1). A restore puts a module's database into a fresh (or used) database, and its files onto its
   volume, from that set.
3. **A module that cannot be dumped** (P1) is never counted as backed up.
4. **A server installed before this change** (P2) is covered without a module upgrade.

## Requirements

- **FR-001** No image is needed that is not already on the node: `pg_dump`/`pg_restore` run in the Postgres image
  of `deploy/templates/postgres.yaml` (pinned by digest), everything else in the image the running can-core names.
- **FR-002** The Job runs in `mod-<id>` under Pod Security `restricted` with memory limits (database container
  128 MiB, can-core container 96 MiB). Only the database container gets `can-db`; only the can-core container gets
  the projected token and the volume.
- **FR-003** No dump is held in memory: `pg_dump` writes to a disk-backed `emptyDir`; the volume archive is
  streamed into the upload. An upload that breaks off is not kept by can-core.
- **FR-004** The database is dumped first, the files are archived after it, in the same hook turn. Every file a
  dumped row refers to is in the archive unless the application deleted it in between.
- **FR-005** The restore downloads and verifies every part (size and SHA-256 against `manifest.json`) before it
  changes anything. The database is replaced in one transaction; the volume's content is unpacked beside the old
  content and then swapped in. It is safe to run again.
- **FR-006** A missing `can-db` Secret, a missing volume of a database module, a missing SQLite file on a volume that
  holds other things, an unreadable file, or a MariaDB database fails the dump run. Modules this cannot dump (wiki,
  status, crm) keep their old hooks, so enabling one still stops backups, loudly.
- **FR-007** A recorded manifest with the old script hooks is replaced, for the dump and restore, by the shipped
  `hooks` of the same provider.

## Manifest

```yaml
hooks:
  backup: backup      # words, not paths
  restore: restore
  builtin: {sqlite: goatcounter.sqlite3}   # or {files: true}
```

A module with `requires.database: postgres` has its database dumped as well.

| Module | Dumped |
|---|---|
| analytics | `database.sqlite3.gz` (`VACUUM INTO`, `integrity_check`) |
| support | `database.pgdump` (custom format), `files.tar.gz` (the whole volume) |
| newsletter | `database.pgdump`, `files.tar.gz` (uploads) |

plus `manifest.json`, uploaded last (`format` 1, `binary` `can-core`; sizes and hashes are of the files as
uploaded, unlike the in-house sets).

## Restore contract (specs/042-restore, epic E08c)

`KubeHooks.RunRestore(module)`, which `can restore` calls, is the restore hook: the module at 0 replicas, its database
provisioned, can-core in restore mode. The Job is `can-core module-data restore` (init container for a database module) followed by
`pg_restore | psql` in the Postgres image. It drops the schema `public` with its content inside the same transaction
that loads the dump, so it works on a fresh database and on one the application already wrote to. A raw volume copy
restored before it is replaced by the archive, which belongs to the database dump.

## Postgres versions

Server major **17** only, the major of the pinned image, for the managed server and for an external one
(`CAN_POSTGRES_ADMIN_URL`). `pg_dump` 17 refuses a newer server; what `pg_restore` 17 writes does not load into 16 or
older (`SET transaction_timeout`), and replacing the schema `public` needs 15's ownership. The dump and the restore
check `server_version_num` first and fail with the reason. A restore does it in a first init container
(`database-check`), before the files are downloaded or replaced. So a company on an external 15 or 16 server has **no**
backup while such a module is enabled, and is told so by every dump run.

## Disk

The work volume (`emptyDir`, node disk) is bounded at 9 GiB: the dump store's 8 GiB per module plus a margin. A restore
downloads the whole set into it (compressed, as stored). It then unpacks files or the SQLite database beside the old
content on the module's volume before swapping, so that volume holds both, the new one uncompressed; and the Postgres
load is one transaction, so the database server holds old and new together plus its write-ahead log. The node needs
free disk for all of it at once. Eviction and Unschedulable reasons are part of the hook's error.

## Design choices

- **can-core's image, not `can-hooks` and not the module image.** The upstream images have no uploader; publishing
  one needs a release. The image can-core runs is on the node by definition and has the SQLite driver.
- **A file in `emptyDir` for `pg_dump`, not a pipe between containers.** A pipe cannot tell the uploader that
  `pg_dump` failed; an init container that fails stops the pod. The cost is a transient second copy on disk.
- **Files are in the dump** (unlike specs/043, which only lists them): nothing here records checksums per file, and the
  raw volume copy is taken about an hour later, after the application may have deleted what the database dump
  refers to. The cost: the files are on the disk twice (volume and dumps directory), restic deduplicates the
  compressed archive poorly, and the per-module limits apply (4 GiB per file, 8 GiB per run). A larger volume
  fails the dump run, loudly.

## Success criteria

- **SC-001** Unit: files, SQLite and database sets survive upload, download and restore; a damaged part or an
  unreadable file changes nothing and keeps nothing.
- **SC-002** On k3d in CI (`wrapped-backup`): data made in analytics, support and newsletter is dumped, removed, restored
  onto nothing (module namespaces and database server deleted) by can-core's restore operations, and read back.

Measured by that job (run 45758426280, GitHub runner, data of a few hundred KB): dump run with the three modules
22.7 s, their restore hooks 24 s. No figure for large data.

## Known limit found here

A dump run made before the kubelet has mounted a newly created `company` ConfigMap left `company.yaml` out and still
completed; `can restore` could not use the set. Since specs/048-erasure-collection such a run fails and writes no
completion marker. The k3d test restarts can-core after creating the ConfigMap.

## Not verified / out of scope

- A real server, large databases or volumes, an external Postgres (the version check is tested with stand-in tools, not
  against a 15 or 16 server).
- wiki, status (image digests are still placeholders, so they cannot be verified) and crm (not deployable, MariaDB).
- A restore through restic of these modules (`restore-e2e` does not install them).
- Quiescing the application during the dump (FR-004's window stays).
- Purge (erasure) hooks for these modules.
