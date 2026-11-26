# Contract: files UI routes

Served by `can-files` on the same listener as the JSON API when `ui.enabled: true` (compose dev stack:
`http://localhost:8185/` and `http://files.localhost:8080/`). `/v1/*`, `GET /s/{token}`, `GET /healthz` and
`/readyz` belong to the API; everything else is the UI. Source: `core/files/ui*.go`. Tests:
`TestUIRoutesMatchContract` (this table against `files.UIRoutes()`), `TestUISessionRoutesRequireASession`,
`TestUIContractRoutesEnforceTheirScope`, `TestUIEveryPostNeedsTheCSRFToken`,
`TestUIContractNamesRealStoreMethods`.

**Authorisation.** The UI has none of its own. **Scope** is the permission the API's guard demands for the
same operation (`read`, `write`, `admin`; `session` means any signed-in person; write includes read, admin
includes both). The **Store methods** are the calls the route makes; they apply the space roles, answer one
404 for everything the caller cannot see, and audit exactly what they audit for the API.

**Unsafe requests** carry the field `csrf`. They are `application/x-www-form-urlencoded`, except the two
uploads, which are `multipart/form-data` with `csrf` as the first part and `file` after the other fields.
Before a byte of an upload is read the route's permission is checked and the Store's `ReserveUpload` /
`ReserveVersion` check the scanner and the space role and take an upload slot; then at most 64 KiB are read
before the file's own bytes (preamble, part headers and fields together; 413 beyond).
A `Sec-Fetch-Site` header, when sent, must be `same-origin` or `none`. Failures are 403. Unauthenticated
requests to a session route get `303 /login`.

**Statuses of failures:** 403 missing permission or role, 404 not found or not visible, 409 conflict (hold,
retention date, stale form, not empty), 413 too large or quota, 415 type not allowed, 422 invalid input or
refused by the virus scanner, 429 too many requests, 503 no scanner, scanner failed, storage or audit
unavailable.

| Route | Session | Scope | Store methods | Purpose |
|---|---|---|---|---|
| `GET /login` | no | - | - | Sign-in form; sets the login CSRF cookie |
| `POST /login` | no | - | `Authenticator` | Fields `token`, `csrf`. 303 `/`; 401 generic, 403 stale form or token login off, 429 after repeated failures (a valid token is never throttled) |
| `GET /login/ticket` | no | - | - | Single sign-on landing (`specs/036-module-ui-sso`): shows a Continue form, never consumes the ticket. 400 malformed, 404 when not under can-core |
| `POST /login/ticket` | no | - | - | Redeems the ticket (this module is the audience), 303 to a clean URL with a new session |
| `GET /static/{file}` | no | - | - | Embedded stylesheet and icon |
| `GET /manifest.webmanifest` | no | - | - | Web app manifest |
| `POST /logout` | yes | session | - | Ends the caller's session |
| `GET /{$}` | yes | read | `ListSpaces` | Spaces; create form for writers |
| `POST /spaces` | yes | write | `CreateSpace` | Field `name` |
| `GET /spaces/{id}` | yes | read | `GetSpace`, `ListFolders`, `ListFiles` | A space or, with `folder`, a folder of it; cursor paged |
| `GET /spaces/{id}/members` | yes | read | `GetSpace`, `ListMembers` | Access list (managers) |
| `POST /spaces/{id}/members` | yes | write | `SetMember` | Fields `kind`, `principal`, `role` |
| `POST /spaces/{id}/members/remove` | yes | write | `RemoveMember` | Fields `kind`, `principal` |
| `POST /spaces/{id}/folders` | yes | write | `CreateFolder` | Fields `name`, `parent` |
| `POST /folders/{id}/delete` | yes | write | `DeleteFolder` | Fields `space`, `parent` (where to return to) |
| `GET /spaces/{id}/upload` | yes | write | `GetSpace`, `ListFolders` | Upload form (editors) |
| `POST /spaces/{id}/files` | yes | write | `ReserveUpload`, `Upload` | Multipart: `csrf`, `folder`, `description`, `subject`, `ref`, `file` |
| `GET /spaces/{id}/trash` | yes | read | `GetSpace`, `ListFiles` | Deleted files (editors) |
| `GET /files/{id}` | yes | read | `GetFile`, `GetSpace`, `ListFolders`, `ListVersions`, `ListShares` | Details, scan verdict, versions, links, forms; links only for editors |
| `GET /files/{id}/content` | yes | read | `OpenContent` | Download (`version` optional): always an attachment |
| `POST /files/{id}/update` | yes | write | `UpdateFile` | Fields `rev` (required), `name`, `description`, `folder` |
| `POST /files/{id}/versions` | yes | write | `ReserveVersion`, `UploadVersion` | Multipart: `csrf`, `file` |
| `POST /files/{id}/delete` | yes | write | `DeleteFile` | To the trash |
| `POST /files/{id}/restore` | yes | write | `RestoreFile` | Out of the trash |
| `POST /files/{id}/shares` | yes | write | `CreateShare` | Fields `label`, `hours`, `version`, `one_time`; the page shows the link once |
| `POST /shares/{id}/revoke` | yes | write | `RevokeShare` | Field `file` (where to return to) |
| `GET /admin/files` | yes | admin | `AdminFiles` | Ids, states, sizes, holds; `subject` filter. No names |
| `GET /admin/files/{id}/hold` | yes | admin | - | Hold and retention form |
| `POST /admin/files/{id}/hold` | yes | admin | `SetHold` | Fields `hold` (`on`/`off`, required), `retain_until`, `clear_retention` |
| `GET /admin/erasure` | yes | admin | `AdminFiles` | With `subject`: what would be destroyed, and the confirmation form |
| `POST /admin/erasure` | yes | admin | `EraseSubject` | Fields `subject`, `confirm` = `ERASE`, and `token` (token sessions); step-up for ticket sessions |
