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
| `/audit` | Audit log, newest first, filter by actor, action prefix and target; **Verify chain**; **Export as JSON lines** (both are POST buttons; only one runs at a time) |
| `/events`, `/events/{id}` | Event deliveries and the attempt log; requeue for dead deliveries. Event payloads are deliberately not shown (they can hold personal data) |
| `/leads`, `/leads/{id}` | Read-only list of form submissions with a status filter, and a status change form |
| `/settings` | Change password, rotate the authentication (TOTP) secret, list and revoke sessions |

| `/catalogue`, `/catalogue/{id}` | **Module catalogue** (E12b): every manifest with status badge (`ready`, `experimental`, `not-deployable`), licence class and a source-available warning, declared RAM request and limit, the footprint total against the server size declared in `company.yaml`, requires/provides (planned capabilities read "not yet served"), exclusive-capability conflicts, and the selection form. The module page generates a settings form from the module's `settings.schema.json` |
| `/changes/{id}`, `/runs/{id}` | Preview of a change (`company.yaml` diff, reconcile plan, what is created or kept) and the apply progress page |
| `/status` | Module readiness, last reconcile result, certificate and ingress status, disk usage |
| `/company` | The stored `company.yaml` and its version |
| `/catalogue/{id}/purge` | Destroy a disabled module's data (separate permission, typed confirmation, password) |
| `/setup/{step}` | **Onboarding wizard** (first login, owner only) |

The full route table, with methods and fields, is [`specs/010-admin-ui/contracts/ui-routes.md`](../specs/010-admin-ui/contracts/ui-routes.md).
Unknown paths show a styled 404; failures show a styled error page with a request id you can look up in
the can-core log.

## Managing modules (epic E12b)

Everything below needs module management, which is on when `CAN_INSTALLER=kubernetes` (the reconcile loop
exists). Otherwise these pages answer 503. Design: [`specs/017-module-ui-wizard/`](../specs/017-module-ui-wizard/).

**Where changes go.** `company.yaml` stays the single source of truth. The UI edits the document can-core
applies (the ConfigMap `can-system/company`; the file `CAN_COMPANY_FILE` in development), as a small
allow-listed patch (`name`, `domain`, `provider.options.server_ram_mb`, `primary.*`,
`modules.<id>.enabled` and `.settings`). Comments and other keys are kept. A change is stored first, shown as
a diff, and written only when you press **Apply**. Each stored document has a version; a change prepared against
an older version is refused (409), so two browsers, or the UI and `can apply`, cannot silently overwrite each
other. `can apply` from a repository **does** overwrite the stored document: copy `/company` into your
repository first if you use both.

**Preview and apply.** The preview lists the diff, the reconcile actions (install, upgrade, enable, disable),
for each module the namespace `mod-<id>`, its Secrets, ingress host, declared disk and database, and for a
disable what is kept (namespace, volumes, Secrets, database). Apply audits the intent, writes the document,
then reconciles; one apply runs at a time. The progress page reloads every three seconds (stop it with the
link; there is no JavaScript) and shows each module's state and a sanitised last error.

**Opt-ins.** A source-available module and an experimental module each need their own tick. A
not-deployable module cannot be ticked. A selection with an unmet or only-planned requirement, or two
providers of an exclusive capability without a primary, is refused.

**Settings forms** render strings, integers, booleans, enums and `format: secret-ref` properties. The server
validates the result with the module's own JSON Schema. Other properties are preserved and listed. A
secret-ref is the *name* of a Kubernetes Secret, never the secret.

**Footprint.** Declared figures: 650 MB platform baseline plus declared module RAM plus 150 MB once if a
module needs Postgres or MariaDB, compared with `provider.options.server_ram_mb`. These are estimates, not
measurements of your server; without a declared size the page says so instead of claiming a fit.

**Destroying data.** Disabling keeps everything. Purge needs `platform.purge`, typing `purge <id>`, and your
password; it is audited before and after. This build ships no purge executor, so the page says it is not
available.

**Wizard.** At first login the owner (`platform.setup`) is sent to `/setup`: company, template (starter or
blank; the starter bundle's not-yet-deployable modules are listed as left out), modules with the meter, email
relay (host, port, user, password, from; the password goes into the Secret `can-system/can-smtp-relay` and
is never shown again), the DNS records to create plus the sslip.io instant URL, account hardening (the page
reads the audit log to see whether you rotated the password and authentication secret; the recovery kit is
made with `can backup kit`) and review. Answers are saved per step, so you can leave and return; **Apply**
is idempotent. **Close setup** dismisses it.

**Permissions.** `platform.view` (catalogue, status, progress), `platform.operate` (preview, apply,
settings), `platform.setup` and `platform.purge` (new; owner only unless you grant them).

**API.** `GET /api/v1/catalogue` and `GET /api/v1/status` (read-only; see the OpenAPI file).

**Not verified.** None of this has run against a real cluster; the installer, ConfigMap, certificate, disk and
DNS probes are behind interfaces and tested with fakes. Not wired in this build: the purge executor, the
certificate/ingress reader (the status page says "not available"), pod readiness, and the sslip.io probe.

## Configuration

| Variable | Effect |
|---|---|
| `CAN_COMPANY_NAME`, `CAN_COMPANY_DOMAIN` | Shown on the dashboard |
| `CAN_LEADS_TOKEN_FILE` | File with the bearer credential can-core uses to read the `leads` capability. Re-read on every request. Without it the leads page says leads are not available |
| `CAN_PUBLIC_IPS` | Comma-separated public addresses of the server, listed in the wizard's DNS step. Unset: the step says the address is not known |
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
- Changing the password or the authentication secret needs the current password **and** a fresh authentication
  code (5 wrong attempts lock account changes for 15 minutes; each attempt is counted before it is checked)
  and signs you out everywhere. A code can be used once, so right after signing in you wait for the next one. A new secret takes effect only after you
  confirm it with a code generated from it.
- The login CSRF cookie is `__Host-` prefixed when cookies are Secure (plain-HTTP development uses a plain name).
- **HSTS is not sent by can-core.** The ingress (Traefik) must send `Strict-Transport-Security` for the admin host;
  without it a browser's first request can still be downgraded.
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
automated run in `go test ./core/server/adminui` (`a11y_test.go`); a manual screen-reader pass (and a read-through of these docs by someone who did not write them) is still
owed (see the plan's known gaps).

## Working on it

- Strings live in `messages.go` (`{{t "key"}}` in templates). A test fails on unknown or unused keys.
- Templates are in `templates/`, one file per page plus `layout.html`. The golden files in
  `testdata/golden/` pin the rendered HTML; after an intended change run
  `go test ./core/server/adminui -run Golden -update` and review the diff.
- New routes must be added to the route table in the spec (a test compares).
