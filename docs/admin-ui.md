# Admin web UI

The admin UI is served by can-core on the admin listener (`admin.<domain>`), next to the JSON API. It is
server-rendered Go templates with the stylesheet and icons embedded in the binary: no JavaScript, no
framework, no build step. Source: `core/server/adminui`. Specification: `specs/010-admin-ui/`.

## Pages

| Path | What it shows |
|---|---|
| `/login` | Username, password and authentication code. Same throttling and error text as the JSON API |
| `/` | Dashboard: company, module cards (enabled, failed, last error), capability map, webhook delivery health with a requeue button per dead delivery, recent audit entries |
| `/modules`, `/modules/{id}` | Installed modules with state, version, licence (a **Source-available** badge when `licence_class: source-available`), declared RAM and its total; detail shows the manifest summary |
| `/audit` | Audit log, newest first, filter by actor, action prefix and target; **Verify chain**; **Export as JSON lines** |
| `/events`, `/events/{id}` | Event deliveries and the attempt log; requeue for dead deliveries. Event payloads are deliberately not shown (they can hold personal data) |
| `/leads`, `/leads/{id}` | Read-only list of form submissions with a status filter, and a status change form |
| `/settings` | Change password, rotate the authentication (TOTP) secret, list and revoke sessions |

The full route table, with methods and fields, is [`specs/010-admin-ui/contracts/ui-routes.md`](../specs/010-admin-ui/contracts/ui-routes.md).
Unknown paths show a styled 404; failures show a styled error page with a request id you can look up in
the can-core log.

## Configuration

| Variable | Effect |
|---|---|
| `CAN_COMPANY_NAME`, `CAN_COMPANY_DOMAIN` | Shown on the dashboard |
| `CAN_LEADS_TOKEN_FILE` | File with the bearer credential can-core uses to read the `leads` capability. Re-read on every request. Without it the leads page says leads are not available |
| `CAN_COOKIE_SECURE`, `CAN_TRUST_PROXY` | As for the API (see `specs/005-can-core-mvp/deployment.md`) |

## Security model

- One session for UI and API: signing in to either signs you in to both (`can_session`, HttpOnly,
  SameSite=Strict, Secure).
- Every page needs a session (otherwise 303 to `/login`). Every form carries a CSRF token (the session's
  for signed-in pages; a double-submit cookie for the login form). Requests the browser marks
  `Sec-Fetch-Site: cross-site` or `same-site` are refused.
- Content-Security-Policy `default-src 'none'; script-src 'none'; style-src 'self'; ...; frame-ancestors 'none'`.
  There is no script on any page, so a template injection bug cannot run code. Everything that comes from
  modules, audit entries or leads is HTML-escaped by `html/template` (tested with hostile data).
- Changing the password or the authentication secret needs the current password (5 wrong attempts lock
  account changes for 15 minutes) and signs you out everywhere. A new secret takes effect only after you
  confirm it with a code generated from it.
- Authenticated pages are `Cache-Control: no-store`. There is no service worker, so nothing authenticated
  is ever cached by the app.

## Installing on Android (PWA)

Open `https://admin.<domain>/` in Chrome on Android, sign in, then use the menu entry "Install app" (or
"Add to Home screen"). The manifest (`/manifest.webmanifest`) gives it a name, icons and standalone
display. It opens the live site; there is no offline mode by design.

## Accessibility

Skip link, landmarks, one `h1` per page and ordered headings, labelled controls, table captions and
header scopes, visible focus, 44 px touch targets, colour pairs checked against WCAG AA in light and dark
schemes, `prefers-reduced-motion` and `forced-colors` honoured, usable at 200% zoom. The checks that can be
automated run in `go test ./core/server/adminui` (`a11y_test.go`); a manual screen-reader pass is still
owed (see the plan's known gaps).

## Working on it

- Strings live in `messages.go` (`{{t "key"}}` in templates). A test fails on unknown or unused keys.
- Templates are in `templates/`, one file per page plus `layout.html`. The golden files in
  `testdata/golden/` pin the rendered HTML; after an intended change run
  `go test ./core/server/adminui -run Golden -update` and review the diff.
- New routes must be added to the route table in the spec (a test compares).
