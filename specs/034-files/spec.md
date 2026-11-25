# Feature Specification: Files (Epic E13e)

**Branch**: `epic/E13e-files` | **Date**: 2026-11-25 | **Status**: built offline, experimental

SPEC.md references: the `files` capability (4.3, 5.1: "store, fetch and export files with per-class
access rules"), document storage and the `documents` adapter returning `files` references for sensitive
classes (5.1), object storage or PVC backends, retention, legal hold and erasure tombstones (3.7), the
e-sign path (signed PDFs purged from the e-sign app after signing: **not built here**). Resume upload
checks: `specs/013-jobs-recruiting/`. Policy layer, sealed fields, audit and tombstones:
`specs/012-people-directory/`, `specs/014-finance-basics/`, `specs/018-equity/`,
`specs/028-identity-provision/`. Module wiring and tokens: `specs/016-module-wiring/`. Release and
supply chain: `specs/024-release-security/`. Observability: `specs/030-observability/`. Not legal advice.

## Summary

A new in-house module `files` provides the `files` capability: spaces with access lists, folders,
immutable versions, share links, a fail-closed virus-scan hook, retention with a trash period, legal
hold and erasure with tombstones, on a local volume or an S3 bucket. The modules that today only carry
an opaque id for a document (jobs resumes, board packs, finance receipts and contract PDFs, people
employee documents) get somewhere to put the bytes and a contract to reference them by.

## User scenarios

1. **A team shares documents.** Alice creates a space, adds Bob as viewer and the `grp-legal` directory
   group as editor, and uploads a contract PDF. Bob can read and download it; he cannot change it. Carol,
   who is in neither, cannot tell the space exists.
2. **A new version.** Alice uploads a corrected PDF. Version 1 stays downloadable by number; the latest
   is what a plain download returns.
3. **An outsider gets a copy.** Alice makes a one-time link that expires in two days. The recipient opens
   it without an account and gets an attachment. A second attempt, a tampered link or the expired link
   all say the same 404; Alice can revoke it earlier.
4. **A bad upload.** A `.pdf` that is an executable, a 40 MB file, a file that the scanner flags and a
   file when the scanner is down are each refused, leaving nothing behind; the flagged one is announced
   as an event with its checksum and no name.
5. **A hold.** Legal asks for a hold on a file. Nobody in the space can delete it or let the sweep purge
   it until an administrator lifts the hold.
6. **An erasure.** A candidate asks to be erased. An administrator erases the subject: the blobs go, the
   names are blanked, the database file and its WAL are scrubbed, and tombstones are written so a restore
   from an older backup erases them again.
7. **Corruption.** A stored object is damaged. The download is aborted before the client has the whole
   body, the failure is audited, and nobody is given bad bytes as a good file.

## Requirements

**Functional**

- FR-1 Spaces with an access list of users and directory groups with roles viewer, editor, manager; a
  person's role is the strongest of their own and their groups'; a space keeps at least one manager.
- FR-2 Folders (bounded depth) inside a space; no access list of their own.
- FR-3 Upload as a raw streamed body; allow-list by extension **and** magic bytes; size, count, version
  and per-owner quota limits; the declared Content-Type is never used.
- FR-4 Immutable versions, each with its own blob, size, SHA-256 and scan result; download by number.
- FR-5 Metadata change (name, description, folder) with optimistic concurrency; the extension is fixed.
- FR-6 Soft delete into a trash, restore, purge after `trash_days`; legal hold and retention date
  block deletion, erasure and purge; a purge is claimed in one transaction before anything is deleted.
- FR-7 Share links: signed, expiring, revocable, optionally one-time, pinned to a version, public route
  `GET /s/{token}`.
- FR-8 A scanner hook (`Scanner`) that fails closed; a clamd client; `scanner.required` defaults to true.
- FR-9 Backends `local` (sharded directory, atomic writes) and `s3` (SigV4, path-style, https) behind one
  interface.
- FR-10 Erasure by data subject with tombstones for the backup ledger; the `purge(subject, class)` hook;
  a retention sweep; key rotation; audit verification.
- FR-11 Events `file.uploaded.v1`, `file.shared.v1`, `file.deleted.v1`, `file.infected.v1` through
  can-core's outbox path, ids only.
- FR-12 Capability `files.v1`, manifest, chart, image fragment, release wiring, compose service.

**Security and privacy (Principle V)**

- SR-1 What a caller may not see answers 404 with the same body as what does not exist, on every route.
- SR-2 Permissions are a coarse gate and the space role the rule; `files.admin` administers by id and
  reads nothing; a hold can be lifted only by `files.admin`.
- SR-3 Names, descriptions and share labels are sealed with a per-row AAD; keys rotate with a
  compare-and-swap; erasure scrubs the WAL.
- SR-4 Audit before return for every read of sealed metadata or content, lists included (fail closed), in the same transaction for writes, hash-chained and
  append-only; entries carry ids and field names only.
- SR-5 Storage keys are random; no user-controlled path exists; names are sanitised for display and headers.
- SR-6 Downloads are attachments of `application/octet-stream`, nosniff, sandboxed CSP, never cached; no
  inline by default; nothing is rendered or unpacked on the server.
- SR-7 The SHA-256 is verified on every download before the last chunk is released; the response is
  aborted on a mismatch.
- SR-8 Uploads are bounded in size, time and memory; a declared oversize body is refused unread.
- SR-9 Share tokens are HMAC-signed, never stored, never logged; only failures are rate limited and a
  valid credential is never throttled.
- SR-10 Every list is bounded and paged; every statement and request has a deadline; no quadratic work.
- SR-11 Events carry no name, label, subject or content.

**Non-functional**: idle RSS at most 20 MiB (measured in research R9), one static binary, no new
dependency, no unzip, no image or PDF rendering, no network call except the configured backend,
scanner and can-core.

## Out of scope

E-signature and its purge path, previews and thumbnails, full-text search, per-folder access lists,
resumable or chunked uploads, deduplication, a web UI, antivirus rescans, encryption of blobs at rest by
the module, a directory group resolver (the interface exists), calls from the existing modules, backup
hooks. These are listed as gaps in `docs/files.md`.

## Success criteria

- SC-1 `go test -race ./core/files/... ./test/...` and `make test` pass.
- SC-2 The OpenAPI document and the capability schema match the routes the service registers; every
  event the service produces validates against its published schema.
- SC-3 No test contacts a real S3, ClamAV or cluster; everything not run against one is listed in
  `docs/files.md` under "What is verified and what is not" (this document is a tool, not legal advice, and
  lists what is unverified).
