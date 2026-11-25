# Tasks: Files (E13e)

Status: built offline. `[x]` = done and covered by a test.

## Storage and checks
- [x] T01 `Backend` interface; local backend (0700, sharded, temp + fsync + rename, zeroing delete); contract suite
- [x] T02 S3 backend (SigV4 path-style, https only, payload hash = file SHA-256); fake S3 recomputing the signature
- [x] T03 Type detection (allow-list by extension and magic bytes), filename and Content-Disposition sanitising
- [x] T04 Scanner hook, fake and clamd INSTREAM client, fails closed
- [x] T05 Verified streaming copy with held-back tail, bounded memory

## Service
- [x] T10 Store: SQLite, migrations, hash-chained append-only audit, sealing, key rotation CAS, scrub
- [x] T11 Spaces, members, folders; role resolution with groups; 404 for the invisible
- [x] T12 Upload and versions: stage, scan, pending blob, same-transaction record, quota and caps
- [x] T13 Views per role (per-field masking), metadata change with rev, listing with keyset paging
- [x] T14 Download with audit before return and verified streaming; integrity failure audited
- [x] T15 Share links: signed tokens, one-time, revoke, failure limiter
- [x] T16 Trash, restore, retention sweep, legal hold, retention date, erasure, tombstones, purge hook, WAL scrub
- [x] T17 Outbox and dispatcher for the four events
- [x] T18 API, authentication (service token, introspected PATs), config, `Main`, sweeper loop, healthcheck

## Platform
- [x] T20 `module.yaml`, chart (keyring, `CAN_RAM_DECLARED_MB`, `/s/` only), settings schema, Dockerfile fragment
- [x] T21 `files.v1`, event schemas and fixtures, embed, secrets table entry, observability wiring test
- [x] T22 Contract tests: manifest, fixtures, OpenAPI vs routes, service output vs schemas, chart, docs honesty
- [x] T23 Release wiring: build script, image matrix, dependabot, SECURITY.md, releasing doc
- [x] T24 Compose service, Caddy route, smoke check, e2e scenario
- [x] T25 Docs: `docs/files.md`, module and chart READMEs, catalogue, epics, index, wiring
- [x] T26 RAM measured locally (research R9)

## Not done (documented gaps, `docs/files.md`)
- [ ] G1 directory group resolver (interface only)
- [ ] G2 S3 credentials provisioning by can-core; NetworkPolicy egress to a bucket or scanner
- [ ] G3 a ClamAV service and chart; a run against a real clamd and a real S3
- [ ] G4 image build and cluster install
- [ ] G5 encryption of blobs at rest by the module; backup and restore hooks
- [ ] G6 e-sign path; callers in jobs, board, finance, people
- [ ] G7 resumable uploads, per-folder access lists, web UI
