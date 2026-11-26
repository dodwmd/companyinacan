# Feature 050: web UI for files (E13f)

Status: built, experimental. No browser-driven or cluster run.
Builds on `specs/034-files`, `specs/036-module-ui-sso` and `specs/040-module-routing`.

## Why

`can-files` had a JSON API only, so a business user could not use it.

## Scope

Server-rendered pages inside `can-files` (html/template, embedded stylesheet, no JavaScript, strict CSP),
off by default (`ui.enabled`):

- sign-in with a can-core launch ticket, or with a pasted token the API accepts (`ui.token_login`);
- spaces, members, folders;
- upload and new version (multipart, streamed), with the scan verdict shown; download as an attachment;
- versions, rename, describe, move, trash and restore;
- share links: create (shown once), list, revoke;
- for `files.admin`: the by-id file list, legal hold and retention date, erasure of a data subject with a
  typed confirmation and step-up.

Not in this slice: search by name (the Store has none: names are sealed, and a search would need a new
read path), renaming or moving folders (the Store has neither), previews and thumbnails (by design),
resumable uploads and progress (needs script), the audit log, erasure tombstones, running the retention
sweep and the purge hook (API only), several files in one upload.

## Requirements

- **FR-001 One authorisation path.** Every page calls the Store methods the API calls. `ui*.go` holds no SQL
  and no access decision of its own; the route table demands the permission the API's guard demands, groups
  are resolved as the API resolves them, and invisible things answer the same 404 page.
- **FR-002 Sessions.** As the other module UIs: server-side, `__Host-` cookie, HttpOnly, SameSite=Strict,
  idle and absolute timeouts, re-authentication every 5 minutes (1 for administrators), ticket sessions end
  at can-core's deadline. The restore-only credential cannot sign in.
- **FR-003 CSRF.** Every POST carries the session's token; a `Sec-Fetch-Site` other than `same-origin` or
  `none` is refused. On a multipart upload the token is the first part of the body and is checked before a
  byte of the file is read.
- **FR-004 Uploads.** The body is never parsed whole or held in memory: parts are read in order and the file
  part is handed to `Store.Upload` / `Store.UploadVersion` as a stream, so every limit, type check, scan,
  quota and concurrency rule is the API's. The permission, scanner and space role are checked and an upload
  slot is taken before a byte is read; at most 64 KiB are read before the file's own bytes (`research.md` R3).
  A declared length over `limits.max_file_bytes` plus 64 KiB of
  form overhead is refused before reading. Transfer encodings of parts are never decoded.
- **FR-005 Downloads.** The same function as the API writes them: `application/octet-stream` attachment,
  nosniff, sandboxing CSP, no-store, checksum verified while streaming, audited before the first byte. The UI
  never serves inline. `HEAD` is refused.
- **FR-006 Share links.** The token appears once, in the answer to the POST that made it.
- **FR-007 Irreversible actions.** Erasure needs `ERASE` typed and a step-up: the pasted token again, which
  must still authenticate now (introspection is asked, not its 30 second cache), or (ticket session) an admin sign-in at most 5 minutes old confirmed by
  can-core. A legal hold form preselects nothing: a post that does not say on or off is refused.
- **FR-008 Routing.** `runtime.public_ui` lists only UI paths; `/v1`, `/healthz` and metrics are never
  routed by it, and `/s/` stays the chart's own route. The UI is routed only with `modules.files.expose.ui`,
  which also turns it on and the token form off unless `token_login` is set. With `expose.public: false` the
  UI offers no share links. `public_ui` also permits `expose.api`, which publishes all of `/v1`; the
  restore-only credential is refused on proxied requests.
- **FR-009 Accessibility.** Language, one h1, landmarks, skip link, labelled controls, table captions and
  header scopes, visible focus, AA contrast in both colour schemes, usable on a phone.
- **FR-010 RAM.** Templates parse on first use; the module declares 28 MB (was 20) and stays within it and
  its 64 MiB limit (`research.md` R2).

## Contracts

`contracts/ui-routes.md`: every route, its permission and the Store methods it calls (machine checked).

## Verification

`go test ./core/files` (flows, SSO, route contract, golden pages, accessibility checklist, no-second-authz
source check), `test/contract` (routing paths, SSO routes, compose stack). RAM and size: `research.md`.
