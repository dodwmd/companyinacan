# Files

The in-house `files` module (epic E13e): document and file storage with spaces and access lists,
immutable versions, share links, a virus-scan hook, retention, legal hold and erasure. Library
`core/files`, binary `cmd/can-files`, manifest `modules/files/module.yaml`, capability contract
`schemas/capabilities/files.v1.json`, wire contract `specs/034-files/contracts/openapi.yaml`.

**Status: experimental. This is a tool, not legal advice.** What has to be kept, for how long and
when it has to be erased is the operator's responsibility. [What is verified and what is not](#what-is-verified-and-what-is-not)
is at the end; read it before relying on any of this.

## What it is for

`jobs` (resumes), `board` (packs), `finance` (receipts, contract PDFs) and `people` (employee
documents) each carry an opaque id or some metadata for a document and have nowhere to put the bytes.
`files` is that place. A module stores a file here, keeps the returned `fil_...` id (and optionally a
`ref` of its own choosing such as `jobs:application-1234` and a `subject` such as the candidate id),
and asks for it back when a person needs it. It never holds the bytes and never decides who may see
them. **Nothing in those modules calls this one yet**; see [Integration](#integration).

## Model

| Thing | What it is |
|---|---|
| Space | A container with its own access list. Whoever creates it is its manager. |
| Role | `viewer` (read, download), `editor` (add, change, delete, restore, share), `manager` (editor plus the access list and every share link). A grant goes to a user (`user:alice`) or to a directory group (an opaque id). A person's role is the strongest of their own and their groups' grants. A space always has a manager. |
| Folder | Organises files inside a space, up to `max_folder_depth` deep. No access list of its own: the space decides. |
| File | A name, a description, a type, an owner space and an ordered list of versions. Optional opaque `ref` and `subject`. |
| Version | Immutable: a new upload is a new version with its own blob, size, SHA-256 and scan result. Nothing overwrites an old one. |
| Share link | A signed, expiring, revocable link to one version, optionally one-time. |

Two gates apply to every call: the token's permission (`files.read`, `files.write` or `files.admin`;
write includes read, admin includes both) and then the role in the space. A read-only token cannot
write even as a manager; a manager's token cannot do more than its permission allows.

`files.admin` administers and does not read: it sets and lifts legal holds, erases a data subject,
pulls tombstones and reads the audit log, by id. It sees no names and no content (`GET /v1/admin/files`
returns ids, sizes and states) and it cannot open a space it is not in.

### Anti-enumeration

A space, folder, file, share or version the caller has no role for answers **404 with the same body**
as one that does not exist, for every route, a malformed id included. A caller with a role but too
little of it gets 403. Lists contain only what the caller can see (an empty list, never an error).
Ids are 96 random bits. Listings are keyset-paged, at most 200 items a page, under a 15 second
deadline.

### Per-field masking

A viewer sees id, name, description, type, size, version, state and dates. An editor adds the SHA-256,
the scan result, the uploader and the trash date. A manager adds `subject`, `ref`, the legal hold and the retention date. The
space owner's name is shown to managers only.

## Uploads

The body is the raw file (`POST /v1/spaces/{id}/files?name=...`), streamed, never held in memory, never
multipart. In order, cheapest first:

1. Permission, role, name and extension are checked before a byte is read; a declared `Content-Length`
   over the limit is refused (413) before the body is read.
2. The first 4 KiB decide the type: the extension must be on the allow-list **and** the magic bytes
   must match it (`%PDF-`, PNG, JPEG, GIF, WebP, `PK\x03\x04` for the Office and OpenDocument types,
   `{\rtf`, UTF-8 without NULs for text). The declared `Content-Type` is never used. No archive, script,
   markup (SVG, HTML) or executable type exists to be allowed. The stored type comes from the table, not
   the client.
3. The rest is copied into a private staging file on the data volume, hashed on the way, under the size
   limit and one upload deadline for the whole body.
4. The scanner reads the staged file. **Nothing reaches the backend before it passes.**
5. The blob is registered as pending, written to the backend under a fresh random key, and then one
   transaction re-checks the space's file count and the owner's quota and records the file, its version,
   the audit entry and the event. If that fails the blob is deleted; a crash in between leaves a pending
   row the sweep removes after an hour.

Names are reduced to a short single line (`SafeFilename`): path parts, control and format characters
(bidi overrides included), quotes and semicolons are dropped. A name never influences where anything is
stored: keys are random 128-bit hex and a user-supplied path cannot exist. The extension of a file
cannot change afterwards.

Limits (all configurable, all with defaults): file size (25 MiB), files per space (10,000), versions per
file (100), spaces per owner (100), members per space (500), folder depth (8), storage quota per space
owner (5 GiB, counted over the versions that have not been purged), live share links per file (20).
Refusals are 413 (too large, quota, caps), 415 (type), 422 (infected), 503 (no scanner, scanner failed).
There is no resumable or chunked upload: a failed upload is repeated. At most
`limits.max_concurrent_uploads` (4) uploads run at once, at most `limits.max_concurrent_uploads_per_subject`
(half of that, always below it unless it is 1; the static API token is one subject) of them for one subject
(503 beyond either, nothing read), and an upload the
owner's quota cannot take is refused before it is staged (the final check is in the recording transaction).

**What the type check does not do.** It looks at the extension and the leading bytes only. For the Office
and OpenDocument types it checks the ZIP local-file signature, not the entries inside: any ZIP renamed to
`.docx` (including one holding executables or macros) passes, and the archive is never opened. For text
types only the first 4 KiB must be UTF-8 without NUL bytes: binary content further in is not detected. The
virus scanner, the attachment-only download and the fixed `application/octet-stream` type are what limit
the harm; the check is not content validation.

### Virus scanning

`scanner.required` defaults to **true**: with no scanner configured **every upload is refused** (503),
and so is an upload the scanner could not scan. A file is stored only on a clean verdict, or on `skipped`
when the operator has turned the requirement off (the version is then labelled `skipped`, never `clean`).
The shipped scanner is `clamd` over TCP (`INSTREAM`): only an explicit `OK` is clean and only an explicit
`FOUND` is infected; an error line, a timeout, a dropped connection and silence are all "could not scan".
An infected upload is refused, deleted from staging, audited (`file.infected`) and announced with
`file.infected.v1` (space, size, checksum: no name).

## Downloads

- **Always an attachment of `application/octet-stream`** with `X-Content-Type-Options: nosniff`,
  `Content-Security-Policy: default-src 'none'; sandbox`, `Cache-Control: private, no-store` and a
  sanitised `Content-Disposition` (an ASCII fallback plus the RFC 5987 form).
- **No inline display by default.** `download.inline` plus `download.inline_types` (raster images only:
  png, jpg, gif, webp; PDF is never offered) let a caller ask for `?inline=1`, still sandboxed.
- **Nothing is rendered on the server**: no thumbnails, no PDF or image conversion, no unzipping.
- **The audit entry is written before the first byte**, in its own transaction; if it cannot be written
  nothing is served (503). The same holds for `GET /v1/files/{id}` (metadata) and every share download.
- **The SHA-256 is verified while streaming.** The final chunk (32 KiB) is held back until the whole
  object has been read and hashed; on a mismatch, a short or a long object the connection is aborted so
  the client sees a failed transfer, the failure is audited (`file.integrity_failed`) and logged. Memory
  is two 32 KiB buffers whatever the size.

## Share links

A token is `shr_<id>.<expiry>.<mac>`, the mac an HMAC-SHA-256 over the id and expiry under
`CAN_FILES_LINK_SECRET`. The database keeps the id, the pinned version and the state (uses, revoked)
and **never the token**; the label is sealed. A link works until it expires or is revoked, or for one
download when created `one_time`. Deleting a file revokes its live links (in the same transaction, audited);
restoring the file does not bring them back: make new ones.

- Lifetime: at least 1 minute, at most `share.max_ttl_hours` (30 days by default); the default is 24 hours.
- The use is counted and audited in one transaction **before** the content is opened, so a one-time link
  cannot be used twice and an unaudited download is not served. A download that fails after that has used
  the link.
- Every failure (forged, tampered, expired, revoked, used, deleted) is the same 404. Failures, and only
  failures, count per client address (`failures_per_minute`, default 20): from then on a failing request
  gets 429. A valid link, like a valid API token, is never refused because of the requests around it.
  `trust_proxy` takes the right-most `X-Forwarded-For` entry; leave it off unless a proxy appends it.
- The token is in the URL path and nowhere else: access metrics and errors carry the route pattern
  (`GET /s/{token}`), never the URL, and the service writes no access log of paths.
- A `HEAD` request is refused (405) and does not count as a use. A `GET` does: a mail or chat link previewer that fetches a one-time link before the recipient does will use it up, so send one-time links only over channels that do not unfurl them.
- Removing a member, or demoting them below editor, revokes in the same transaction the links that rested
  on that grant (the person's or a group's), unless another of the creator's grants still allows sharing.
  A person who leaves a directory group is **not** seen: their links last until they expire or are revoked.
- A version stored unscanned is not served (409, links answer 404) once `scanner.required` is on.
- Rotating `CAN_FILES_LINK_SECRET` ends every live link. Revoking one link is `DELETE /v1/shares/{id}`.
- Only `/s/` is exposed by the chart's ingress. The recipient needs no account and the response is the
  same attachment as any download.

## Retention, legal hold, erasure

- **Delete** moves a file to the trash (`deleted`, hidden from viewers, restorable by editors) and revokes its
  share links. A legal hold, or a retention date that has not passed, makes it a 409.
- **Retention sweep** (every `sweep_interval_hours`, or `can-files sweep`) purges files that have been in
  the trash `trash_days` (30 by default; 0 never purges), skipping held files and files before their
  retention date, finishes interrupted purges, removes pending blobs older than an hour and drops old outbox rows.
  A blob that cannot be deleted does not stop the rest: its file stays marked for purging, the other work is
  done, and every failure is reported together (and retried by the next sweep). Purging deletes
  every blob (zeroed first on a plain filesystem), blanks the sealed names, the version references and
  checksums, deletes the share links, writes tombstones and an audit entry per file and queues
  `file.deleted.v1` (`reason` `retention`). The row stays as an empty shell so ids never come back.
- **A purge is claimed, then carried out.** One transaction re-checks holds, retention dates and state,
  marks the files `purging` and writes the tombstones. From then on they are invisible and a hold, restore,
  new version or share link is refused. Then exactly the blobs of the claimed versions are deleted and the
  result recorded. A crash or storage failure in between is finished by the next sweep or a repeat.
- **Legal hold and retention date** are set only by `files.admin` (`PUT /v1/files/{id}/hold`): the people
  who manage a space cannot lift a hold placed on it. `legal_hold` is required in every request; an omitted
  `retain_until` leaves the date as it is and `null` clears it. Both block deletion, the sweep, erasure on
  request (409) and the purge route; neither blocks adding a version.
- **Erasure** (`POST /v1/subjects/{subject}/erasure`, `files.admin`) purges every file whose `subject` is
  the given opaque id (no row limit), then runs `VACUUM` and a truncating WAL checkpoint so the erased names
  are not left in free pages or the write-ahead log. A held or retention-dated file blocks the whole request
  (409). It is idempotent and a repeat finishes an interrupted erasure and retries the scrub.
- **Tombstones** (`GET /v1/erasures`, `files.admin`) are `(subject, class, date)` rows in the format of the
  backup erasure ledger (can-core collects them into it), for the classes `file-content` and `file-metadata`. An erasure request writes them
  for the subject. A retention purge writes them for the **file id** only, so a restore never removes the
  subject's other files.
- **`POST /v1/purge`** is the `purge(subject, class)` hook the restore replays: idempotent, it matches a
  subject or a file id, and an unknown subject is a no-op. An administrator gets 409 for a held or
  retention-dated file. Only the restore-only credential `CAN_FILES_RESTORE_TOKEN` (optional; it opens
  this route and nothing else) ignores them, audited as `file.restore_purge`. can-core generates it and
  uses it when `can restore` replays the ledger. Neither class is reachable on its own: both purge the whole
  file.
- Backups and snapshots taken earlier still hold the data until they expire; the erasure ledger, collected by can-core, re-applies the
  erasure after a restore. The zeroing of a deleted blob is not a guarantee on copy-on-write or SSD
  storage.
- **S3 versioned buckets.** If the bucket has object versioning (or object lock, replication or its own
  backups), `DELETE` only adds a delete marker: the bytes of a "purged" or erased file stay as a
  noncurrent version until a bucket lifecycle rule expires it. Use an unversioned bucket, or set a rule
  that expires noncurrent versions within the period you promise; this module does not and cannot check.

## Sealed metadata, audit, keys

Space, folder and file names, file descriptions and share labels are sealed with XChaCha20-Poly1305
under a keyring (`keyring_path`, a mounted file that must be 0400, 0440 or 0600 and not a symlink).
Each value is bound to its table, row and column (AAD), so a ciphertext copied to another row does not
open. `can-files rotate-keys` re-seals values written with a retired key under the newest one with a
compare-and-swap per value, so a concurrent edit is never overwritten with stale data; it prints how
many it re-sealed and the old key can go when it prints 0. The names of spaces and files are therefore
not in the database file, its WAL or the backups of it; the **file bytes are not encrypted by this
module**.

The audit log is hash-chained and append-only at the database level (triggers refuse UPDATE and
DELETE; `can-files verify-audit` recomputes the chain). It holds actor, action, resource id and field
names; never a name, a label or content. Writes record their audit entry **in the same transaction** as
the change: an action that cannot be audited does not happen. Every read that returns sealed metadata or content (spaces, folders, files and their lists, versions, share lists, member lists, downloads and share downloads) is audited before the data is returned and fails closed. Refusals are audited too,
throttled per actor. The chain protects against edits only if the latest hash is kept somewhere the
editor cannot reach.

## Events

`file.uploaded.v1`, `file.shared.v1`, `file.deleted.v1` and `file.infected.v1` carry opaque ids, sizes,
the content type and the SHA-256, and the actor. No file name, description, label, subject or content.
They are queued in a transactional outbox in the same transaction as the change and published through
can-core at least once (backoff, a seven-day window, then dead-lettered rather than dropped). With no
can-core (standalone) nothing is queued. Schemas: `schemas/events/file.*.v1.json`.

## Storage backends

| Backend | Details |
|---|---|
| `local` | Files `0600` under a `0700` directory on the PVC, sharded `ab/cd/<key>`. A write goes to a temp file in the same volume, is fsynced and renamed; the directory is fsynced. A crash leaves no partial blob at a real key. A symlinked directory is refused. |
| `s3` | Path-style, AWS Signature V4 written without an SDK (the payload hash is the file's own SHA-256, so S3 itself rejects a body that does not match), https only (`allow_insecure_http` is for a local MinIO in development), no redirects followed, credentials from the environment, errors that name neither credentials nor URLs. Server-side encryption is the bucket's setting. |

Both implement one small `Backend` interface (`Put`, `Open`, `Delete`, `Ping`) tested by one contract
suite. `/readyz` includes the backend. The module does not deduplicate: the same bytes uploaded twice
are stored twice, which keeps one person's erasure from touching another's blob and gives no way to
learn whether some content already exists (`specs/034-files/research.md` R3).

## Operating

```
can-files serve | check-config | healthcheck | verify-audit | rotate-keys | sweep | version
```

Secrets are environment variables, never config: `CAN_FILES_API_TOKEN` (the service token: it acts as
`svc:static`, holds `files.admin`, owns the spaces it creates and sees nothing it has no role in) and
`CAN_FILES_LINK_SECRET` (32+ characters, 10+ distinct), optionally `CAN_FILES_RESTORE_TOKEN` (the restore-only credential above), plus `CAN_FILES_S3_ACCESS_KEY_ID` and
`CAN_FILES_S3_SECRET_ACCESS_KEY` for S3. can-core generates the first two and the keyring. Personal access
tokens are introspected through can-core (`kind` user, subject `user:<name>`, permissions `files.*`).

The chart sets `CAN_RAM_DECLARED_MB` from the declared RAM; `/readyz` and the request metrics come from
`core/obs`. RAM is measured in `specs/034-files/research.md` R9.

## Integration

The intended flow for a module such as `jobs` is: upload the resume with `ref=jobs:application-1234` and
`subject=cand_...`, keep the file id, show a link or proxy the download with the person's own token (so
the space's access list decides), and on candidate erasure call `POST /v1/subjects/{cand}/erasure` or let
the ledger replay do it. **None of the existing modules does this yet**: their upload code and their
`purge` hooks are unchanged, and each needs its own change (and a space owned by a service identity that
can add the right people). The capability contract is what they can rely on.

## What is verified and what is not

Verified offline by unit, race and contract tests: the local backend and the S3 backend against a fake
that recomputes the Signature V4 from the request; the clamd protocol against a fake daemon; type
detection, filename and header sanitising; the role and visibility rules (404 for the invisible,
identical bodies); the upload pipeline and everything it must leave behind on failure; quota and caps;
versions; verified streaming and the aborted response; audit-before-return failing closed and same-
transaction audit; sealed fields and the key-rotation compare-and-swap; share links including the
failure limiter; trash, hold, retention, erasure, the WAL scrub and tombstones; the outbox payloads
against the published event schemas; the capability and OpenAPI contracts against the real routes;
the chart through the installer.

**Not verified, unverified or not built:**

- The image has not been built or run on a cluster; the chart has not been installed on one.
- No real S3 service (AWS, MinIO, R2) and no real ClamAV was used: the SigV4 code is checked against its
  own reading of the specification, the clamd client against a fake.
- **Groups are not resolved.** A grant to a directory group is stored and enforced, but no directory
  group lookup is wired, so only user grants apply until a `GroupResolver` is configured.
- The S3 credentials are not generated by can-core and the installer refuses another Secret, so the
  `s3` backend has no supported way to receive them on a cluster; the module namespace NetworkPolicy may
  block egress to a bucket or a scanner.
- There is no ClamAV chart: until one exists and `scanner.type: clamd` is set, the shipped default
  refuses every upload.
- Blobs are **not encrypted at rest by the module**; use an encrypted volume or bucket. A stolen volume
  exposes file content (names are sealed).
- Backup: `can-files backup` dumps the database and lists every referenced blob; the bytes are covered by the
  volume backup (local backend only, **not** an S3 bucket). A restore needs the same `keyring`
  ([backup-restore.md](backup-restore.md#in-house-modules)).
- No e-sign path: signed PDFs can be stored, but nothing here sends a document for signature or
  purges it from an e-sign app.
- No resumable or chunked upload, no folder moves, no per-folder access lists, no web UI, no search, no
  previews or thumbnails (by design), no antivirus rescan of stored files, no content deduplication.
- The existing modules do not call this one (see Integration).
- Constant-time and timing behaviour of the share-link mac was reviewed, not measured.
