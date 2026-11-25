# Contract: admin UI routes

Served on the admin listener next to the JSON API (`/api/v1/*` and `/healthz` belong to the API, see
`specs/005-can-core-mvp/contracts/admin-api.yaml`). A contract test (`TestRoutesMatchContract`) compares
this table with `adminui.Routes()`.

Unsafe requests are `application/x-www-form-urlencoded` posts carrying the hidden field `csrf` (the
session's CSRF token; on the login form, the double-submit token from the login cookie: `__Host-can_login_csrf` when cookies are Secure, `can_login_csrf` on plain-HTTP development).
Unauthenticated requests to a session route get `303 /login`. Unknown paths get the 404 page.

| Route | Session | Purpose |
|---|---|---|
| `GET /login` | no | Login form; sets the login CSRF cookie |
| `POST /login` | no | Fields `username`, `password`, `totp`, `csrf`. 303 `/` on success; 401, 403, 429, 503 re-render the form |
| `POST /logout` | yes | Ends the session, 303 `/login` |
| `GET /{$}` | yes | Dashboard |
| `GET /modules` | yes | Module list with RAM total |
| `GET /modules/{id}` | yes | Manifest summary |
| `GET /audit` | yes | Audit viewer. Query `actor`, `action` (prefix), `target`, `before` (seq) |
| `POST /audit/verify` | yes | Verify the hash chain, shows the result |
| `POST /audit/export` | yes | JSON lines download (audited; a POST because it writes an audit entry). `POST /audit/verify` and export share a single-flight lock: a second request while one runs gets 503 with `Retry-After` |
| `GET /events` | yes | Recent deliveries |
| `GET /events/{id}` | yes | Delivery detail and attempts |
| `POST /events/{id}/requeue` | yes | Requeue a dead delivery; field `next` is `/` or `/events/{id}` |
| `GET /leads` | yes | Leads list. Query `status`, `cursor` |
| `GET /leads/{id}` | yes | Lead detail with status form |
| `POST /leads/{id}/status` | yes | Field `status` (`new`, `contacted`, `qualified`, `closed`, `spam`) |
| `GET /settings` | yes | Account, sessions |
| `POST /settings/password` | yes | Fields `current` (password), `otp` (current authentication code, single use), `new`, `confirm` |
| `POST /settings/totp` | yes | Field `current`; shows a new secret to confirm |
| `POST /settings/totp/confirm` | yes | Fields `current`, `otp` (current code), `secret`, `code` (code from the new secret) |
| `POST /settings/sessions/{sid}/revoke` | yes | Revoke one session (16 character public id) |
| `GET /approvals` | yes | Approvals inbox: waiting on me, my requests, all requests (approvals admins; query `status`), out-of-office delegations |
| `GET /approvals/{id}` | yes | Request detail with steps and decision history; 404 for anyone who may not see it. Audited (`approval.read`) |
| `POST /approvals/{id}/decide` | yes | Fields `decision` (`approve`, `reject`), `comment`. 403 for the requester (separation of duties) |
| `POST /approvals/{id}/comment` | yes | Field `comment` |
| `POST /approvals/{id}/cancel` | yes | Withdraw; field `reason`. Requester or approvals admin |
| `POST /approvals/delegate` | yes | Fields `delegate` (user name), `until` (date, UTC) |
| `POST /approvals/delegations/{did}/revoke` | yes | End one of your delegations |
| `GET /notifications` | yes | Inbox, delivery settings and preferences. Query `before` (id) |
| `POST /notifications/read` | yes | Mark all read |
| `POST /notifications/settings` | yes | Fields `email`, `quiet_start`, `quiet_end` (HH:MM or empty), `tz`, `digest` |
| `POST /notifications/prefs` | yes | Fields `type`, `channel` (`email`, `inapp`), `enabled` (absent = off) |
| `POST /notifications/{id}/read` | yes | Mark one of your own read; 404 otherwise |
| `GET /access` | yes (`rbac.manage`) | Users, role assignments and role definitions |
| `POST /access/{username}/grant` | yes (`rbac.manage`) | Fields `role`, `scope` |
| `POST /access/{username}/revoke` | yes (`rbac.manage`) | Fields `role`, `scope`; 409 for the last owner |
| `GET /static/{file}` | no | Embedded stylesheet and icons (immutable cache) |
| `GET /manifest.webmanifest` | no | Web app manifest |

Status codes on pages: 200, 303, 400 (bad form), 403 (CSRF / cross-site), 404, 409 (requeue of a non-dead
delivery), 429, 502 (leads gateway), 500, 503.

## Response headers (all responses)

`Content-Security-Policy: default-src 'none'; script-src 'none'; style-src 'self'; img-src 'self'; font-src 'self'; manifest-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'`,
`Referrer-Policy: no-referrer`, `X-Content-Type-Options: nosniff`, `Permissions-Policy: camera=(), microphone=(), geolocation=()`,
`Cache-Control: no-store` (static assets: `public, max-age=3600`).
