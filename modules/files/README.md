# files

In-house document and file storage: spaces with access lists, folders, immutable versions, share
links, a virus-scan hook, retention with a trash period, legal hold and erasure. Library
`core/files`, binary `cmd/can-files`, image `deploy/files`. It provides the `files` capability
(`schemas/capabilities/files.v1.json`) and publishes `file.uploaded.v1`, `file.shared.v1`,
`file.deleted.v1` and `file.infected.v1` (ids, sizes and a checksum only).

The modules that today only carry an opaque id for a document (jobs resumes, board packs, finance
receipts and contract PDFs, people employee documents) can reference a file here by its id and an
optional `ref` of their own choosing (`jobs:application-1234`) and `subject` (the opaque id of the data
subject, which erasure uses). They never hold the bytes. Nothing in those modules calls this one yet:
see [`docs/files.md`](../../docs/files.md), "Integration".

**Status: experimental.** Built and tested offline (unit, race and contract tests, a fake S3 that
verifies the AWS Signature V4 independently, a fake clamd) and measured locally (see
`specs/034-files/research.md` R9). The chart installs through can-core's installer in unit tests; the
image has not been built or run on a cluster; no real S3 service and no real ClamAV was used; the
blobs are not encrypted at rest by the module; directory groups are not resolved yet. There is a
server-rendered web UI for people (off by default; no real browser has been used on it): see
[`docs/files.md`](../../docs/files.md), "Web UI".

**Safety defaults.** Uploads are refused until a real scanner is configured (`scanner.required`).
Downloads are always attachments of `application/octet-stream` with `nosniff` and a sandboxing CSP;
inline display exists only for raster images and only when the operator turns it on. The SHA-256 is
checked while streaming and the response is aborted on a mismatch. Storage keys are random: no name or
path a user sends reaches the filesystem or the bucket.

**Installed under can-core** (see [`docs/module-wiring.md`](../../docs/module-wiring.md)): can-core
generates the API token, the share-link secret and the sealed-field keyring once. Personal access
tokens are introspected through can-core: `files.read`, `files.write` and `files.admin` are the coarse
gate and the role in the space decides the rest. A space the caller has no role in answers 404, the
same as one that does not exist.

The full guide is [`docs/files.md`](../../docs/files.md); the wire contract is
[`specs/034-files/contracts/openapi.yaml`](../../specs/034-files/contracts/openapi.yaml).

What is unverified or not built is listed under "What is verified and what is not" in
[`docs/files.md`](../../docs/files.md).

Not legal advice: what has to be kept, for how long and when it has to be erased is the operator's
responsibility.

## Business overview

The module provides `summary` (`GET /v1/summary`, single-use grants from can-core only): storage used and files whose retention date ends within 30 days (administrators: everything; others: the spaces they manage).
Counts only. See [`specs/053-business-overview/`](../../specs/053-business-overview/spec.md).
