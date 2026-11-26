# Feature specification: backup and restore hooks for the in-house modules (epic E08d)

**Branch**: `epic/E08d-module-hooks` | **Input**: the gap left by specs/037-backup-dumps: the SQLite databases of
forms (in the default bundle), work, people, jobs, finance, equity, board and entity, provisioning and files were
copied raw while being written, and the `can-hooks` image the wrapped modules need was never built anywhere.

## User scenarios

1. **Nightly backup** (P1). Every enabled or disabled in-house module with data has a consistent copy of its
   database in the dump set, and the live database files are left out of the snapshot.
2. **Files that a database refers to** (P1). For files and jobs, what the dumped database refers to is what the
   volume backup holds, and a restore verifies it by checksum.
3. **Restore of one module** (P1). An operator (or `can restore`, epic E08c) turns a dump set into a working
   instance with one subcommand of the same binary.
4. **The hooks image builds** (P2). CI builds `deploy/hooks`, so a release does not meet it for the first time.

## Requirements

- **FR-001** `<binary> backup` copies the database with `VACUUM INTO` over a read-only connection of its own, runs
  `integrity_check` on the copy and uploads through `PUT /v1/dumps`. It never writes to the database.
- **FR-002** `<binary> restore` verifies size, SHA-256 and integrity before it installs anything, refuses to
  replace an existing database without `-replace`, removes the old journal files, and is safe to run again.
- **FR-003** files and jobs: the dump lists every referenced file with size and SHA-256. Backup fails if one is
  missing; restore fails unless every one is present and matches (`-allow-missing-blobs` to override).
- **FR-004** The hook runs as a Job in the module's namespace with the image of the module's Deployment, without a
  shell, a script or any Secret, restricted pod security, memory limit equal to the module's `ram_limit_mb`.
- **FR-005** A module without a database file is "no data" (exit 3): ignored if it never had a dump, reported and
  the earlier dump kept if it had one.
- **FR-006** `deploy/hooks` builds in CI (nothing pushed), and a release pushes the tag the manifests name.

- **FR-007** A module recorded before this change (can-core rewrites a record only when its version or settings
  change, and never while it is disabled) is dumped using the hooks of the manifest shipped in the can-core image.
  The module image must have the subcommand: deploy module images of the same release as can-core, or an enabled
  module's older binary rejects `backup` and fails the run.

## Success criteria

- **SC-001** Unit: every in-house service's own database survives backup and restore; a dump taken while forms is
  writing is complete and passes `integrity_check`.
- **SC-002** On k3d in CI (`module-backup`): forms, work and files are dumped by hook Jobs within their limits,
  the dumps pass `integrity_check`, and fresh instances of the same images restore them and serve the data; a
  disabled module is still dumped.

## Design choices

- **A subcommand in the module image, not `can-hooks`.** The in-house images are distroless and already hold the
  SQLite driver and are already on the node. No new image, no unpublished dependency, and the tool is the same
  version as the schema. The dump code is `core/modbackup`; each `cmd/can-*` dispatches to it.
- **Files are listed, not uploaded.** Blobs and resumes are immutable and written before the row that names them,
  so every file a dumped database refers to is on the volume when the host copies it afterwards. Uploading them
  would hold three copies on a small server and run into the 8 GiB per-module dump limit, failing every backup.
  The one way a referenced file can be absent from the snapshot is that it was destroyed (purge, erasure) between
  the dump and the restic run: such content must not come back, and `-allow-missing-blobs` restores the rest.
  With `backend.type: s3` the bucket is **not** backed up by this project and restore does not check it.
- **No admission policy change.** Nothing bounds Jobs in `mod-*` beyond Pod Security `restricted` and the
  namespace quota, which the hook Job meets. A policy on hook images would not restrain a compromised can-core,
  which writes the Deployments themselves.

## Memory

Job limit = the module's `ram_limit_mb` (forms 32 MiB, the others 64 MiB), `GOMEMLIMIT` at three quarters of it.
Measured on one developer machine (not a server), `can-forms backup` in a cgroup limited to 32 MiB with no swap:
a 10 MB database peaked at 24.5 MiB; a 418 MB database finished in 11 s without an OOM kill (peak at the limit,
which includes reclaimable page cache). Restore is not limited by a Job yet.

## Not verified / out of scope

- jobs, people, finance, provisioning on a cluster (their databases are covered by unit tests); equity, board and
  entity have no chart templates, so can-core cannot install them and their hook Job has never run.
- entity and board are meant to be one release with one database (docs/entity.md); can-core does not merge them,
  and their hooks assume each module's own volume.
- A real server, large file stores, the s3 backend, ClamAV.
- `can restore` itself and can-core's restore mode: epic E08c, specs/042-restore.
- Publishing `can-hooks` or any image. Until a release publishes `can-hooks`, enabling analytics, crm,
  newsletter, status, support or wiki still stops backups; disabling it brings them back.
