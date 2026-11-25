# Research: Files (E13e)

## R1. A new in-house module, not a wrapper

Nextcloud (about 400 MB, cannot enforce per-class access or sealed metadata, AGPL) and Filebrowser
(maintenance mode) were the SPEC.md alternatives and both fail footprint and per-class rules. A thin
service over SQLite and a blob backend is what SPEC.md section 5.1 describes. Decision: build it, in the
shape of the sibling in-house modules (one Go package, strict YAML config, one SQLite connection).

## R2. Metadata in SQLite, bytes in a backend

Putting bytes in SQLite would make every backup, VACUUM and WAL scrub copy file content and would blow the
RAM target. The database holds sealed names, ids, sizes and checksums; a `Backend` holds bytes under random
keys. Two backends (`local`, `s3`) satisfy one four-method interface and one contract suite.

## R3. No content-addressed deduplication

The brief asks for "content-addressed storage with random keys". The SHA-256 is stored and verified, and
it addresses *integrity*; the storage key is random. Deduplicating by hash was rejected: (a) an upload
that is instant when the content exists is an oracle that tells a caller whether some other space holds
the same bytes (a resume, a contract); (b) one subject's erasure would have to reference-count blobs and
could not delete a blob another space still uses; (c) a hash-derived path is a path an attacker can
steer. The cost is storage for repeated identical uploads, which is bounded by the per-owner quota.

## R4. Stage, scan, then write

The upload is copied to a private 0700 staging directory on the data volume while it is hashed, the
scanner reads that file, and only then is the backend written. This keeps infected bytes out of the
backend and gives S3 what it needs (a known length and payload hash for the signed PUT) without buffering
in memory. The staging directory is cleared at start. The alternative, writing to the backend first and
scanning there, would put infected content in a bucket and make "refused" mean "deleted later".

## R5. Verified streaming without a second read

Verifying before serving needs either a second pass over the object or a held-back tail. A second pass
doubles reads (two GETs on S3). The copier holds back the last 32 KiB chunk until the whole object has
been read and hashed: on a mismatch, or a stored length that differs from the recorded one, the last chunk
is never written and the handler panics with `http.ErrAbortHandler`, so the declared `Content-Length` is
never reached and the client sees a failed transfer. Memory is two 32 KiB buffers. A corrupted object is
therefore never delivered as complete. The earlier chunks of a corrupted object have been sent: a client
that streams to disk must treat an aborted transfer as failed (the audit entry says so).

## R6. SigV4 written in miniature

`core/backup` already has a SigV4 client for whole-object PUT and GET. Importing it would couple the
modules and it hashes whole bodies in memory; refactoring it is out of scope. The files backend copies
the signing steps (about 60 lines) and uses the file's own SHA-256 as the signed payload hash, which also
makes S3 verify the body. The fake S3 in the tests recomputes the signature from the request independently
of the client, so a drift in either shows. https is required unless `allow_insecure_http` is set (a local
MinIO), redirects are not followed, and error values name neither credentials nor URLs.

## R7. Share-link tokens

`shr_<id>.<expiry>.<mac>` with the mac an HMAC-SHA-256 over a domain label, the id and the expiry. The
token is not stored (so a database leak yields no usable links) and the state that can change (revoked,
used, expired by the database clock) lives in the row. The use is incremented and audited in one
transaction before the content opens, so a one-time link has exactly one winner under concurrency (the
`UPDATE ... WHERE uses = 0` decides) and an unauditable download is not served. All failures are one 404.
The failure limiter counts only failures per client address, bounded to 10,000 addresses, and a valid
token never consults it, so an attacker behind a shared address cannot lock a legitimate recipient out of
a valid link. Rotating the link secret ends every link: that is deliberate and documented.

## R8. Erasure, tombstones and the WAL

Same order as `core/jobs`: delete blobs first (idempotent, so a failure leaves every reference and the
whole request can be repeated), then one transaction (names, versions, links, tombstones, audit, events),
then `VACUUM` and `wal_checkpoint(TRUNCATE)`. The tombstone subject is the data subject, or the file id
for a file that has none, so the restore-time `purge(subject, class)` replay can find either. A legal
hold blocks a *request*; the replay ignores holds because the erasure was authorised when the tombstone
was written. Retention purges write tombstones too, so a restore from an old backup does not resurrect
files whose retention has ended.

## R9. RAM, measured locally

Static linux/amd64 build (`CGO_ENABLED=0 go build -trimpath -ldflags "-s -w"`), local backend,
`scanner.required: false`, on the developer machine; `VmRSS` of the running process.

| Moment | RSS |
|---|---|
| Idle after start | 12.9 MiB |
| After a 20 MB upload, three 20 MB downloads and 200 list requests | 19.8 MiB (peak, unchanged 20 s later: the Go runtime keeps its heap) |

The files were `/dev/zero` bytes turned into text, so the figure measures streaming and hashing, not
a realistic mix. It is a local measurement of the binary, not of a container on a cluster; it is within
the 20 MB target but with little headroom, and a burst of concurrent uploads would add staging buffers
(32 KiB each) rather than file-sized memory. The 64 MiB limit is the chart's hard cap.

## R10. Blobs are not encrypted at rest by the module

Per-blob encryption needs a chunked AEAD format, a key per blob and a story for key rotation of large
files; names and descriptions (what identifies a person) are sealed, content is not. Operators should use
an encrypted volume or bucket. Recorded as a gap in `docs/files.md`, not as a feature.

## R11. Admin administers, does not read

`files.admin` could have read everything; it was kept to holds, erasure, tombstones, audit and a locating
list of ids, sizes and states without names. A legal hold placed by an administrator cannot be lifted by a
space manager. Reading a file still needs a role in its space, so an administrator who needs the content
has to be added, and that is an audited grant.

## R12. Scanner contract

Only an explicit clean verdict stores a file. `skipped` exists for development and is stored as such.
The clamd client implements INSTREAM over TCP with chunking; the answer must end in ` OK` or ` FOUND`;
anything else is an error. No local executable is run and no shell is involved. The default
(`scanner.required: true`, no scanner) refuses every upload, like `core/jobs`.

## R13. Inline display

Default off. When on, only raster images the operator names, served with their stored type, `nosniff`
and a sandboxing CSP. PDF is never inline (its viewer executes script), SVG cannot be uploaded, and text
is always a download. No server-side rendering exists to exploit.

## R14. Groups

The role query already accepts group grants and the principal carries `Groups`, but no directory group
lookup exists to fill it (the people directory has no per-subject group endpoint). A `GroupResolver`
interface is the seam; wiring it to the directory is a follow-up. Until then group grants are stored and
enforced but match nobody.
