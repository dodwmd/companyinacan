# Data model: Files (E13e)

SQLite, one writer connection, WAL, `secure_delete(ON)`. Timestamps are unix seconds. Columns ending in
`_sealed` hold `v1.<key-id>.<base64url>` XChaCha20-Poly1305 ciphertext with the AAD
`<table>:<id>:<column>`. File bytes are never in the database. Source:
`core/files/migrations/001_files.sql`.

| Table | Purpose | Notes |
|---|---|---|
| `spaces` | A container. `id spc_<24 hex>`, `name_sealed`, `owner`, `created_at` | The owner's quota counts every non-purged version in the owner's spaces |
| `space_members` | The access list: `(space_id, principal)` with `principal` `user\|<subject>` or `group\|<opaque id>` and a role `viewer`, `editor` or `manager` | Index on `(principal, space_id)` serves the visibility join; a space keeps at least one manager |
| `folders` | `id fld_...`, `space_id`, `parent_id`, `depth`, `name_sealed` | Depth bounded by `max_folder_depth`; removed only when empty |
| `files` | `id fil_...`, `space_id`, `folder_id`, `name_sealed`, `description_sealed`, `ext`, `subject`, `ref`, `status` (`active`, `deleted`, `purging`, `purged`), `purge_reason`, `purge_actor`, `purge_action`, `current_version`, `rev`, `legal_hold`, `retain_until`, timestamps | `subject` and `ref` are opaque ids (never names). A purged file stays as an empty shell so ids are never reused. `rev` is the optimistic-concurrency counter and the ETag |
| `versions` | `(file_id, n)`, `blob_key`, `size`, `sha256`, `content_type`, `scan` (`clean`, `skipped`), `created_by`, `purged_at` | Immutable; a purge blanks `blob_key` and `sha256`. `blob_key` is a random 128-bit hex string |
| `shares` | `id shr_...`, `file_id`, `version`, `label_sealed`, `expires_at`, `one_time`, `uses`, `created_by`, `revoked_at` | The token is `id.expiry.mac` and is **not stored**; the mac is an HMAC under `CAN_FILES_LINK_SECRET` |
| `share_grants` | `(share_id, principal)`: the access-list entries that let the creator share | A link is revoked when none of them is still an editor or manager |
| `pending_blobs` | `blob_key`, `created_at` | A blob is registered before it is written and released in the transaction that records its version; the sweep deletes the stragglers |
| `erasures` | Tombstones `(subject, class, date)`, unique per pair | The format of core/backup's erasure ledger; `class` is `file-content` or `file-metadata`; no personal data |
| `outbox` | `event_id`, `type`, `payload`, `next_attempt`, `attempts`, `delivered_at`, `dead_at` | Written in the transaction of the change; delivered at least once |
| `audit_log` | Hash chain `(seq, at, actor, action, resource, fields, outcome, prev, hash)` | Triggers refuse UPDATE and DELETE; ids and field names only |
| `schema_migrations` | Applied versions | Read by the readiness check |

## State of a file

```text
active --delete--> deleted --restore--> active
deleted --sweep claims (trash_days, no hold, retain_until passed)--> purging
active|deleted --erasure or purge hook claims--> purging
purging --blobs deleted and recorded--> purged
```

A legal hold or an unpassed `retain_until` blocks `delete`, the sweep, erasure by request and the purge
route (only the restore-only credential ignores them). The claim is one transaction; a `purging` file is
invisible, cannot be held, restored, versioned or shared, and is finished by the next sweep if
interrupted. A purged file has no blob, no sealed name, no share links.

## Visibility rules (what the SQL enforces)

Every read of files joins the caller's memberships (`user|<subject>` and each `group|<id>`) and takes the
strongest role per space; a file outside that join is not returned and a single-object read of it is
"not found". Deleted files need an editor. Every clause that combines with the visibility test is
parenthesised.
