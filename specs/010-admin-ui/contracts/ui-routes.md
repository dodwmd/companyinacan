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
| `GET /catalogue` | yes (`platform.view`) | Module catalogue (E12b): every manifest with status badge (`ready`, `experimental`, `not-deployable`), licence and source-available warning, declared RAM request and limit, the footprint total against the declared server size, requires/provides (planned capabilities shown as "not yet served"), exclusive-capability conflicts, and the selection form. 503 without module management |
| `GET /catalogue/{id}` | yes (`platform.view`) | One module: summary, requires/provides, and the settings form generated from its `settings.schema.json` (shown to `platform.operate`). 404 for an unknown id |
| `POST /catalogue/{id}/settings` | yes (`platform.operate`) | Fields `s.<property>` for each rendered property. Validated server side with the module's schema; 400 re-renders the form with per-field errors. 303 to `/changes/{cid}` (a stored proposal, not yet applied) |
| `GET /catalogue/{id}/purge` | yes (`platform.purge`) | Purge confirmation page. 409 unless the module is recorded as disabled |
| `POST /catalogue/{id}/purge` | yes (`platform.purge`) | Fields `confirm` (exactly `purge <id>`), `password` (the caller's current password). 400 wrong confirmation, 401/429 wrong password, 409 not disabled, 503 no purge executor. Audited before and after |
| `POST /changes` | yes (`platform.operate`) | Catalogue selection: fields `version` (the company.yaml version the page showed), repeated `enable` (module ids), repeated `ack` (`licence:<id>`, `experimental:<id>`). 303 to `/changes/{cid}`; 409 when `version` is stale; 400 when a module is not deployable, an acknowledgement is missing, or the selection is invalid or cannot work |
| `GET /changes/{id}` | yes (`platform.operate`) | Preview of a stored proposal: company.yaml diff, reconcile plan, what will be created (namespace, Secrets, volumes, ingress host, database) and what disable keeps, footprint, graph, issues, and the apply form. Marks a proposal stale |
| `POST /changes/{id}/apply` | yes (`platform.operate`) | Writes company.yaml (optimistic concurrency) and starts the reconcile. 303 to `/runs/{rid}`; applying an already applied change returns its run; 409 stale or expired; 503 with `Retry-After` while another apply runs |
| `GET /runs/{id}` | yes (`platform.view`) | Apply progress and result. `<meta http-equiv="refresh" content="3">` while running; query `refresh=0` turns the reload off. Errors shown are sanitised |
| `GET /status` | yes (`platform.view`) | Module readiness, last reconcile result, certificate/ingress status and disk usage (each marked unavailable when its source is not configured) |
| `GET /company` | yes (`platform.operate`) | The stored company.yaml and its version |
| `GET /setup` | yes (`platform.setup`) | 303 to `/setup/company` |
| `GET /setup/{step}` | yes (`platform.setup`) | Wizard step: `company`, `template`, `modules`, `email`, `dns`, `account`, `review`. 404 otherwise |
| `POST /setup/{step}` | yes (`platform.setup`) | Steps `company` (`name`, `domain`, `server_ram_mb`), `template` (`template`), `modules` (`enable`, `ack`), `email` (`host`, `port`, `username`, `password`, `from`: the password goes to a Secret and is never echoed), `account` (`kit`), plus `apply` (applies the saved answers; idempotent) and `dismiss`. 303 back to the step; 400 re-renders it |
| `GET /static/{file}` | no | Embedded stylesheet and icons (immutable cache) |
| `GET /manifest.webmanifest` | no | Web app manifest |

Status codes on pages: 200, 303, 400 (bad form), 403 (CSRF / cross-site), 404, 409 (requeue of a non-dead
delivery), 429, 502 (leads gateway), 500, 503.

## Response headers (all responses)

`Content-Security-Policy: default-src 'none'; script-src 'none'; style-src 'self'; img-src 'self'; font-src 'self'; manifest-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'`,
`Referrer-Policy: no-referrer`, `X-Content-Type-Options: nosniff`, `Permissions-Policy: camera=(), microphone=(), geolocation=()`,
`Cache-Control: no-store` (static assets: `public, max-age=3600`).
