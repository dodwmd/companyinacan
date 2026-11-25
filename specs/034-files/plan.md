# Implementation Plan: Files (Epic E13e)

**Branch**: `epic/E13e-files` | **Date**: 2026-11-25 | **Spec**: [spec.md](spec.md)

## Summary

An in-house Go module over SQLite with a pluggable blob backend (local directory, S3). Decisions:
[research.md](research.md); tables: [data-model.md](data-model.md); contract:
[contracts/openapi.yaml](contracts/openapi.yaml) (checked against the registered routes by
`test/contract/files_test.go`).

## Technical Context

**Language/Version**: Go 1.27. **Dependencies**: none added (`modernc.org/sqlite`, `go.yaml.in/yaml/v3`
and `golang.org/x/crypto` are already in `go.mod`; the SigV4 signer is written in 60 lines, not
imported from `core/backup`, which stays untouched).
**Storage**: SQLite (`core/files/migrations/001_files.sql`, `002_audit_append_only.sql`), one writer
connection; file bytes in the backend, never in the database.
**Testing**: `go test -race`; a `Backend` contract suite run against the local and the S3 backends; a
fake S3 that recomputes the signature; a fake clamd; `httptest` for the API; fixtures for events and the
capability. No test contacts a real service.
**Constraints**: idle RSS at most 20 MiB; uploads and downloads stream through two fixed buffers; every
statement and request has a deadline; no goroutine per request beyond net/http's.

## Constitution Check

| Principle | Assessment |
|---|---|
| I. Spec-first | SPEC.md defines the `files` capability (4.3, 5.1), per-class access, retention and erasure tombstones (3.7). This feature restates only what it builds. Deviations, each in research.md: no deduplication (R3), blobs not encrypted by the module (R10), the e-sign path is not built (spec "Out of scope"). |
| II. Footprint is a feature | One static binary, one SQLite connection, no cache of file data, no image or PDF rendering, no unzip, no new dependency. 12.9 MiB idle and 19.8 MiB peak after streaming 20 MB files four times and 200 list requests (R9). The declared RAM (20 MB) is exported as `can_ram_declared_bytes`. |
| III. Test-first | The backend suite, the store tests (access control, upload, quota, versions, verified download, audit failing closed, sealing, rotation, shares, retention, erasure) and the API tests were written first and did not compile until the code existed. The config, command and contract tests, the chart and the documents followed the code; this plan does not claim more. |
| IV. Stable module contracts | `files.v1` and four event schemas are versioned, fixture-tested (valid and invalid) and checked against real service output. The OpenAPI document is compared to the routes. |
| V. Secure and private by default | See the spec's SR list and research R2 to R8. Fail closed everywhere (scanner, audit, integrity), 404 for the invisible, sealed metadata, random keys, attachment-only downloads, ids-only events, erasure with a WAL scrub. |
| VI. Open-source hygiene | No new dependency; Apache-2.0; commits signed off; release wiring (binary, image matrix, dependabot, SECURITY.md) follows the sibling modules. |
| VII. Simplicity and measured claims | One interface for backends, plain functions for the pipeline, no plugin system, no DSL. Everything not run against a real service or cluster is listed in `docs/files.md`. |

**Result**: pass, with the deviations above recorded.

## Project Structure

```text
core/files/                    config, model, store, spaces/ACL, files, shares, retention, events, API, auth, cmd
core/files/migrations/         001_files.sql, 002_audit_append_only.sql
core/files/{backend,s3,scan,detect}.go   Backend interface, local + S3, scanner hook + clamd, type and name checks, verified copy
cmd/can-files/                 main
modules/files/                 module.yaml, chart, events, settings schema
schemas/capabilities/files.v1.json, schemas/events/file.*.v1.json
deploy/files/                  Dockerfile fragment, README
docs/files.md
test/contract/files_test.go, test/wiring (observability), core/server/installer (chart)
docker-compose.yml, dev/compose/*  compose service, Caddy route, smoke check, e2e scenario
```

## Not verified

Listed in `docs/files.md`: no real S3, no ClamAV, no cluster, no group resolver, S3 credentials and
NetworkPolicy gaps, blobs not encrypted at rest, no backup hook, no e-sign path, no caller among the
existing modules.
