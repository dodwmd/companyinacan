# Contract: admin UI routes

Served on the admin listener next to the JSON API (`/api/v1/*` and `/healthz` belong to the API, see
`specs/005-can-core-mvp/contracts/admin-api.yaml`). A contract test (`TestRoutesMatchContract`) compares
this table with `adminui.Routes()`.

Unsafe requests are `application/x-www-form-urlencoded` posts carrying the hidden field `csrf` (the
session's CSRF token; on the login form, the double-submit token from the `can_login_csrf` cookie).
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
| `GET /audit/export` | yes | JSON lines download (audited) |
| `GET /events` | yes | Recent deliveries |
| `GET /events/{id}` | yes | Delivery detail and attempts |
| `POST /events/{id}/requeue` | yes | Requeue a dead delivery; field `next` is `/` or `/events/{id}` |
| `GET /leads` | yes | Leads list. Query `status`, `cursor` |
| `GET /leads/{id}` | yes | Lead detail with status form |
| `POST /leads/{id}/status` | yes | Field `status` (`new`, `contacted`, `qualified`, `closed`, `spam`) |
| `GET /settings` | yes | Account, sessions |
| `POST /settings/password` | yes | Fields `current`, `new`, `confirm` |
| `POST /settings/totp` | yes | Field `current`; shows a new secret to confirm |
| `POST /settings/totp/confirm` | yes | Fields `current`, `secret`, `code` |
| `POST /settings/sessions/{sid}/revoke` | yes | Revoke one session (16 character public id) |
| `GET /static/{file}` | no | Embedded stylesheet and icons (immutable cache) |
| `GET /manifest.webmanifest` | no | Web app manifest |

Status codes on pages: 200, 303, 400 (bad form), 403 (CSRF / cross-site), 404, 409 (requeue of a non-dead
delivery), 429, 502 (leads gateway), 500, 503.

## Response headers (all responses)

`Content-Security-Policy: default-src 'none'; script-src 'none'; style-src 'self'; img-src 'self'; font-src 'self'; manifest-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'`,
`Referrer-Policy: no-referrer`, `X-Content-Type-Options: nosniff`, `Permissions-Policy: camera=(), microphone=(), geolocation=()`,
`Cache-Control: no-store` (static assets: `public, max-age=3600`).
