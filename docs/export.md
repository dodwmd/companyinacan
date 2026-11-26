# Exporting your data (`can export`)

Everything the company holds, in open formats, in one file on your machine. Design and decisions:
[specs/070-company-export](../specs/070-company-export/spec.md). An export is not a backup: nothing
restores from it (for that, [backup-restore.md](backup-restore.md)), and there is no importer for it.

> Verified by unit tests and on throw-away k3d clusters in CI. **Not run on a real server.**
> What was and was not exercised is listed [below](#what-was-verified).

## Taking one

```
can export                                   # asks the server of the company directory over ssh
can export --context k3d-companyinacan-dev   # or a cluster of your own, with local kubectl
can export --encrypt                         # asks for a passphrase; writes *.tar.gz.canenc
can export --module finance --module core    # only these parts
can export --skip support                    # everything but this one
can export --include-sealed --encrypt        # with the encrypted fields in clear
can export decrypt company-export-....tar.gz.canenc
```

The owner can also download it in the admin UI (**Export**, permission `platform.export`, which only
the `owner` role holds unless an owner grants it). It asks for a recent sign-in confirmation.

- The data is streamed from the server to you. Nothing of the export is written to the server's
  disk, except that each module's Job takes a temporary copy of its database (sealed fields stay
  sealed in it) in its own scratch volume, as the backup does, and a wrapped application's Job holds
  its `pg_dump` there until it has been sent.
- One export runs at a time, and none starts while a backup dump is running. A dump that starts
  during an export may have to wait for a module's memory quota; avoid exporting around 02:30 UTC.
- Each module's Job has one hour, and it sends at the speed of your download.
- If anything fails, no file is kept (`can`), or the download breaks off (admin UI). A complete
  archive ends with `manifest.json`. A module that cannot be exported **fails the export**;
  `--skip <module>` leaves it out, and the manifest says the export is partial.
- Every export is in the control plane's audit log: `company.export` (who, which parts, whether
  sealed fields were included) before any data leaves, then `.completed` or `.failed`.

**Read the warnings.** `can export` prints them, and they are in `manifest.json`. They name what is
not in the file.

## What is in it

```
README.txt
company/company.yaml               the applied configuration (credential-like values redacted)
company/records/, schema.json      accounts, roles, approvals, website history, the audit log
site/published.tar.gz              the website as it is served
modules/<id>/records/<table>.jsonl one object per row, exact values
modules/<id>/records/<table>.csv   the same rows for a spreadsheet
modules/<id>/schema.json           tables, columns, types; what was left out and why
modules/<id>/files/                file bytes (files, jobs resumes, wrapped uploads)
modules/<id>/native/database.sql   a wrapped application's own plain SQL dump
modules/<id>/manifest.json         row and file counts, notes, warnings
manifest.json                      last entry: modules, their status, warnings
```

Conventions:

- JSON Lines hold exact values. In CSV, a text cell that a spreadsheet would run as a formula
  (starting `=`, `+`, `-`, `@`, a tab or a line break, also after spaces) starts with an apostrophe.
- Integer columns ending in `_at` (and `at`, `time`) are Unix seconds, UTC. `_minor` is the
  currency's minor unit. Binary values are base64. SQL `NULL` is JSON `null` and an empty CSV cell.
- Files: `files/<file id>/v<version>.<ext>` (names are in `records/files.*`, sealed);
  `files/resumes/<application id>.<ext>`.
- With the `files` module's `s3` backend, **the bytes are in your bucket, not in the export**.

`schema.json` is the schema of record for each export: it is written from the database that was
exported. The tables and columns each service exports are listed in
[`core/modbackup/exportspec.go`](../core/modbackup/exportspec.go), the control plane's in
[`core/server/companyexport/core.go`](../core/server/companyexport/core.go).

## What is never in it

- Passwords and password hashes, TOTP seeds, sessions, API tokens or their hashes, signing and
  webhook secrets, push keys, encryption keys and keyrings. No Kubernetes Secret is exported.
- Erased data: rows are read from the live data, so what a module erased is not there. Erasure
  bookkeeping (tombstones), event payloads, notification texts, mail queues and API replay caches are
  left out too, because they can name what was erased. Records kept by law (see SPEC section 3.7)
  are still records, and are there.
- Diversity answers of `jobs` (published only as suppressed aggregates), `board`'s open ballots and
  vote receipts, and anonymous peer feedback of `people` below the review cycle's minimum number of
  responses.

## Sealed fields

Some fields are encrypted in the database: salaries and identity details (`people`), candidate
contact details, notes and cover letters (`jobs`), file and folder names (`files`), holders' names
and tax ids (`equity`), board texts, minutes, ballots and registers (`board`, `entity`), bank account
numbers (`finance`). By default they show as `[sealed]`. `--include-sealed` (admin UI: a checkbox, a
confirmation and a required passphrase) writes them in clear. For that, and only then, a module's
export Job mounts the keyring (finance: the seal key) its service runs with; the key itself is not
exported. The `can` command writes the file unencrypted if you give no passphrase, and says so.

## Wrapped applications

Their schemas are theirs; nothing here maps them.

| Module | What you get | Left out |
|---|---|---|
| analytics (GoatCounter) | its SQLite tables as JSON Lines and CSV | accounts, API tokens, sessions |
| support (FreeScout) | `pg_dump --format=plain` and the volume's `storage/app` (attachments) | agent accounts, mailbox settings, options, sessions, jobs; the rest of the volume (its configuration file holds the application key and database password; sessions, caches, logs, uploaded add-on modules) |
| newsletter (Listmonk) | `pg_dump --format=plain` and the uploads | accounts, sessions, settings (they hold the relay password) |
| wiki, status, crm | **nothing** (the manifest says so) | their hooks are shell scripts without an export: use the application's own export |

Besides the tables above, tables named like credentials are left out, columns named like credentials
are blanked, and files such as `.env` and `*.key` are not copied. **The table and column rules go by name.** A
secret an application stores somewhere unexpected (inside a message, a JSON column, a file with
another name) would be in the export: treat the file as sensitive. Without the accounts, references
to agents by id have no name in the export. Load a dump with `psql -f database.sql` into an empty
database.

## The encrypted format

`.canenc`: the magic `CANEXPORT1\n`, then Argon2id parameters (passes, memory in KiB, threads), a
16-byte salt and a 15-byte nonce prefix, then XChaCha20-Poly1305 chunks of 64 KiB plaintext. The
nonce is the prefix, a 64-bit counter and a last-chunk byte, and the header is each chunk's
additional data, so a changed, reordered or truncated file does not decrypt
([`core/exportcrypt`](../core/exportcrypt/exportcrypt.go)). The passphrase has at least 12
characters and cannot be recovered. `can export` takes it from `--passphrase-file`,
`CAN_EXPORT_PASSPHRASE` or the terminal, never from an argument; it is not sent to the server. The
admin UI sends it to can-core over the admin site's connection, which encrypts there.

## What was verified

- Unit tests: every column of every in-house service and of the control plane is classified; sealed
  fields open with the services' own bindings; CSV injection; erased rows and files are absent;
  upload path confinement; encryption tampering; one export end to end through the module API with
  the real `can-forms export`.
- k3d in CI (`module-backup`): `can export --include-sealed` with a passphrase of forms, work and
  files. (`wrapped-backup`): analytics, support and newsletter.
- Not verified: a real server and ssh transport; people, jobs, finance, equity, board and
  provisioning export Jobs on a cluster; large data sets, memory use of the Jobs and of can-core
  under them; the admin UI download through the ingress; a browser.
