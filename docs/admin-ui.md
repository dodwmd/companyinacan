# Admin web UI

The admin UI is served by can-core on the admin listener (`admin.<domain>`), next to the JSON API. It is
server-rendered Go templates with the stylesheet and icons embedded in the binary: no JavaScript, no
framework, no build step. Source: `core/server/adminui`. Specification: `specs/010-admin-ui/`.

## Pages

| Path | What it shows |
|---|---|
| `/login` | Username, password and authentication code. Same throttling and error text as the JSON API |
| `/overview` | **Business overview** (E17e), where sign-in lands: what needs attention and a few totals per module (new leads, overdue invoices and receivables by currency, contracts, urgent work, headcount and who is off today, open requisitions, new applications and interviews, grants waiting and vesting due, storage use and retention dates, filings, last database dump). Each person sees only what their roles allow in each module; a module that does not answer in 2 seconds is shown as unavailable. [`specs/053-business-overview/`](../specs/053-business-overview/spec.md) |
| `/dashboard` | Dashboard: company, module cards (enabled, failed, last error), capability map, webhook delivery health with a requeue button per dead delivery, recent audit entries |
| `/modules`, `/modules/{id}` | Installed modules with state, version, licence (a **Source-available** badge when `licence_class: source-available`), declared RAM and its total; detail shows the manifest summary |
| `/audit` | Audit log, newest first, filter by actor, action prefix and target; **Verify chain**; **Export as JSON lines** (both are POST buttons; only one runs at a time) |
| `/events`, `/events/{id}` | Event deliveries and the attempt log; requeue for dead deliveries. Event payloads are deliberately not shown (they can hold personal data) |
| `/leads`, `/leads/{id}` | Read-only list of form submissions with a status filter, and a status change form |
| `/email` | Outbound email delivery status (`notifications.admin`): the relay, and per message the sending module, masked recipient, status, attempts and last error; **Send again** for dead letters (`platform.operate`). See [`email.md`](email.md) |
| `/settings` | Change password, rotate the authentication (TOTP) secret, list and revoke sessions. For users who signed in through single sign-on the password and TOTP sections are replaced by a note |
| `/users`, `/users/{name}` | **Users** (E11e, `rbac.manage`): add, disable, enable, delete; roles and what each grants; reset the authentication code or password; sessions, personal access tokens, single sign-on identity. See [below](#users-and-roles-epic-e11e) |
| `/set-password` | Public page behind a one-time link: choose a password and add the authenticator |
| `/reauth` | Confirm password and code again before a sensitive change |
| `/access` | Role assignments and definitions; owners can override what a role grants |
| `/settings/sso` | **Single sign-on** (E11b, `rbac.manage`): status without secrets (issuer, client id, the *name* of the client secret, redirect URI, policy, group to role table, effective local-login mode), **Test discovery**, and the known identities with **Link** (to an existing local account) and **Unlink** (owners only). Configured in `company.yaml`, see [`sso.md`](sso.md) |
| `/apps` | **Apps** (E11c): the enabled modules with a web page in which your roles give you a permission, each with an **Open** button (a POST) that signs you in to that module with a one-time launch ticket instead of a pasted token. A module appears only when its UI address is configured. See [`sso.md`](sso.md#opening-module-web-pages-from-the-admin-ui-e11c) |
| `/auth/oidc/login`, `/auth/oidc/callback` | The OpenID Connect sign-in flow. When `sso.enabled`, `/login` also shows **Sign in with single sign-on** above the password form |

| `/catalogue`, `/catalogue/{id}` | **Module catalogue** (E12b): every manifest with status badge (`ready`, `experimental`, `not-deployable`), licence class and a source-available warning, declared RAM request and limit, the footprint total against the server size declared in `company.yaml`, requires/provides (planned capabilities read "not yet served"), exclusive-capability conflicts, and the selection form. The module page generates a settings form from the module's `settings.schema.json` |
| `/changes/{id}`, `/runs/{id}` | Preview of a change (`company.yaml` diff, reconcile plan, what is created or kept) and the apply progress page |
| `/status` | Module readiness, last reconcile result, certificate and ingress status, disk usage |
| `/company` | The stored `company.yaml` and its version |
| `/catalogue/{id}/purge` | Destroy a disabled module's data (separate permission, typed confirmation, password) |
| `/setup/{step}` | **Onboarding wizard** (first login, owner only) |

The full route table, with methods and fields, is [`specs/010-admin-ui/contracts/ui-routes.md`](../specs/010-admin-ui/contracts/ui-routes.md).
Unknown paths show a styled 404; failures show a styled error page with a request id you can look up in
the can-core log.

## Users and roles (epic E11e)

Design, rules and threat model: [`specs/044-users-roles/spec.md`](../specs/044-users-roles/spec.md).

- **Add a person.** `/users`: a user name (it cannot be changed, and is never reused after deletion) and an
  optional email address. The account has no role. They set it up through a **one-time link**: they add the
  secret to an authenticator app, choose a password and type a code. With an email relay configured the link
  is mailed (to the company domain or `email.notification_recipients`) and you never see it; otherwise it is
  shown to you once. It lasts 72 hours; send a new one from their page.
- **Roles.** Their page lists each role with the permissions it grants. You are offered only roles you could
  give: nobody hands out more than they hold, and only an owner changes an owner. Removing a role signs the
  person out and revokes their access tokens.
- **Lost phone or password.** *Reset the authentication code* keeps the password, which they must type when
  using the link. *Reset password and code* replaces both. Both sign them out at once.
- **Leaving.** *Disable* stops sign-in and ends sessions, module sessions and tokens, keeping roles; only an
  owner can enable again. *Delete* removes the account and roles; the audit log keeps its history.
- **Confirming it is you.** Sensitive changes need a sign-in or confirmation within the last 10 minutes.
- **Recovery** stays with `can-core admin` inside the pod (`roles`, `reset-password`, `enable`, `unlock`).

Not checked in a real browser or with a real mail relay.

## Single sign-on (epic E11b)

With an `sso:` block in `company.yaml` the login page offers sign-in through your OpenID Connect provider;
the password form stays for the break-glass owner. SSO users are created on first sign-in without any role;
roles come from the group mapping on every sign-in. The pages above need no JavaScript: the callback and the
provider logout are pages that move on by `<meta http-equiv="refresh">` (a redirect would not carry the
SameSite=Strict session cookie after a cross-site navigation, and `form-action 'self'` stops a POST from
redirecting to the provider). Setup, security properties and what is not verified: [`sso.md`](sso.md).

## Managing modules (epic E12b)

Everything below needs module management, which is on when `CAN_INSTALLER=kubernetes` (the reconcile loop
exists). Otherwise these pages answer 503. Design: [`specs/017-module-ui-wizard/`](../specs/017-module-ui-wizard/).

**Where changes go.** `company.yaml` stays the single source of truth. The UI edits the document can-core
applies (the ConfigMap `can-system/company`; the file `CAN_COMPANY_FILE` in development), as a small
allow-listed patch (`name`, `domain`, `provider.options.server_ram_mb`, `primary.*`,
`modules.<id>.enabled` and `.settings`). Comments and other keys are kept. A change is stored first, shown as
a diff, and written only when you press **Apply**. Each stored document has a version; a change prepared against
an older version is refused (409), so two browsers, or the UI and `can apply`, cannot silently overwrite each
other. **`can apply` overwrites everything the UI changed**: it writes the ConfigMap with server-side apply and
`--force-conflicts`, with no version check, so a `company.yaml` from your repository that predates a UI change
silently undoes it. If you use both, copy the stored document from `/company` into your repository before every
`can apply`, or stop using `can apply` for that company. The UI also refuses to edit a document that uses YAML
aliases or anchors.

**Preview and apply.** The preview lists the diff, the reconcile actions (install, upgrade, enable, disable),
for each module the namespace `mod-<id>`, its Secrets, ingress host, declared disk and database, and for a
disable what is kept (namespace, volumes, Secrets, database). Apply audits the intent, writes the document,
then reconciles; one apply runs at a time. The progress page reloads every three seconds (stop it with the
link; there is no JavaScript) and shows each module's state and a sanitised last error.

**Upgrades.** Installed modules are compared by content, not by version number: a change of the manifest,
the chart, its image references or can-core's own module templates (a can-core upgrade brings all of these)
upgrades the module. Comments in `module.yaml` and the chart's documentation do not count. Pods restart only
when what is rendered for them changes. The first reconcile after upgrading can-core from a release without
this applies every installed module once, to record what it has. `can up --images-from` replaces the
running can-core (and site) pod whenever the image tarball differs, although the tag stays `can-core:dev`.
A module image rebuilt under the same tag does not restart its module: change the tag in the chart.

An upgrade of a running module that would let it **do more than before** waits for your approval: a new
permission or role grant, an event it may publish or receives, a capability it serves or uses, a database, a
public host or path, internet access (not for a module installed before manifests declared it: every module had
it), a shorter approval chain, a new notification recipient, a new hook, hook command or hook image, signing
users into its web UI, or a changed data class. Until then the running release and its recorded manifest stay.
**What takes something away is still applied to that release**: secret rotation, switching exposure off and
mail relay changes (including opt-out). Settings and newly exposed routes wait with the upgrade; the app's own
configuration for a removed route is only rewritten by the upgrade. If these cannot be applied (no record of the
installed release), `can status` says so. Backups of a waiting module use the hooks of what is installed (a
module installed before hook scripts were recorded uses the shipped script). Approve on the catalogue page, with
the tick on a change's apply form, or with `can apply --approve <module>=<code>`, where the code is the one the
plan shows; `can status` and `can up` name the modules that wait. The approval covers exactly the list shown. A
restore applies without asking.
`can apply` prints can-core's plan before it syncs `company.yaml`; `can apply --plan` prints it and changes
nothing (`--dry-run` plans against the local state file, not the cluster).

An upgrade that fails is rolled back and tried again after 1, 2, 4 ... minutes, at most hourly; a different
target (a new release or settings) is tried at once, and so is everything after can-core restarts.

**Opt-ins.** A source-available module and an experimental module each need their own tick. A
not-deployable module cannot be ticked. A selection with an unmet or only-planned requirement, or two
providers of an exclusive capability without a primary, is refused.

**Settings forms** render strings, integers, booleans, enums and `format: secret-ref` properties. The server
validates the result with the module's own JSON Schema. Other properties are preserved and listed. A
secret-ref is the *name* of a Kubernetes Secret (optionally `name/key`), never the secret; names starting `can-` or `kubernetes.io` (the Secrets can-core owns) are refused. Settings can only be changed for a module that is enabled, so editing them never turns a module on. An installation that already runs modules is not sent to the wizard.

**Footprint.** Declared figures: 650 MB platform baseline plus declared module RAM plus 150 MB once if a
module needs Postgres or MariaDB, compared with `provider.options.server_ram_mb`. These are estimates, not
measurements of your server; without a declared size the page says so instead of claiming a fit.

**Destroying data.** Disabling keeps everything. Purge needs `platform.purge`, typing `purge <id>`, and your
password and a fresh authentication code; it is audited before and after, and a success does not reset the account's failed-attempt count. This build ships no purge executor, so the page says it is not
available.

**Wizard.** At first login the owner (`platform.setup`) is sent to `/setup`: company, template (starter or
blank; the starter bundle is the landing site and forms, and a bundle's not-yet-deployable modules are listed
as left out), modules with the meter, email
relay (host, port, user, password, from; the password goes into the Secret `can-system/can-smtp-relay` and
is never shown again), the DNS records to create plus the sslip.io instant URL, account hardening (the page
reads the audit log to see whether you rotated the password and authentication secret; the recovery kit is
made with `can backup kit`) and review. Answers are saved per step, so you can leave and return; **Apply**
is idempotent. **Close setup** dismisses it.

**Modules that stop backups.** Enabling a module whose backup hook cannot run (`wiki`, `status` and `crm`, until the
`can-hooks` image is published) stops every backup. The change page and the wizard's review say so, name the
module, add that a database in the shared Postgres is not backed up, and refuse to apply until a box is
ticked for each such module.

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
| `CAN_LEADS_TOKEN_FILE` | File with the bearer credential can-core uses to read the `leads` capability. Re-read on every request. Unset with `CAN_INSTALLER=kubernetes` (as `can up` installs it): can-core reads the forms module's generated `CAN_FORMS_API_TOKEN` from `mod-forms/can-module`, only while forms is the provider of `leads`. Otherwise, without it the leads page says leads are not available. With the forms module's `CAN_FORMS_API_TOKEN` the UI can also erase leads (users with `leads.erase`); with the read token it cannot |
| `CAN_PUBLIC_IPS` | Comma-separated public addresses of the server, listed in the wizard's DNS step. Unset: the step says the address is not known |
| `CAN_COOKIE_SECURE`, `CAN_TRUST_PROXY` | As for the API (see `specs/005-can-core-mvp/deployment.md`) |

## Security model

- One session for UI and API: signing in to either signs you in to both (`can_session`, HttpOnly,
  SameSite=Strict, Secure).
- Every page needs a session (otherwise 303 to `/login`). Every form carries a CSRF token (the session's
  for signed-in pages; a double-submit cookie for the login form). Requests the browser marks
  `Sec-Fetch-Site: cross-site` or `same-site` are refused.
- Content-Security-Policy `default-src 'none'; script-src 'none'; style-src 'self'; ...; frame-ancestors 'none'`.
  There is no script on any page except Notifications, so a template injection bug cannot run code there.
  Notifications alone allows `'self'` scripts, workers and connections (`script-src 'self'; worker-src 'self';
  connect-src 'self'`) to load `/static/push.js`, which only enables push on the current device; the page works
  without it. No inline script, no nonce, no `unsafe-*`. Everything that comes from
  modules, audit entries or leads is HTML-escaped by `html/template` (tested with hostile data).
- Changing the password or the authentication secret needs the current password **and** a fresh authentication
  code (5 wrong attempts lock account changes for 15 minutes; each attempt is counted before it is checked)
  and signs you out everywhere. A code can be used once, so right after signing in you wait for the next one. A new secret takes effect only after you
  confirm it with a code generated from it.
- The login CSRF cookie is `__Host-` prefixed when cookies are Secure (plain-HTTP development uses a plain name).
- **HSTS is not sent by can-core.** The ingress (Traefik) must send `Strict-Transport-Security` for the admin host;
  without it a browser's first request can still be downgraded.
- Authenticated pages are `Cache-Control: no-store`. The only service worker (`/sw.js`) handles push
  notifications and nothing else: no request interception and no cache, so nothing authenticated is ever
  cached by the app (a test fails if it grows a `fetch` listener or touches the Cache API).
- Static files are linked with a content hash (`?v=...`) and cached immutably for that hash.

## Installing on Android (PWA)

Open `https://admin.<domain>/` in Chrome on Android, sign in, then use the menu entry "Install app" (or
"Add to Home screen"). The manifest (`/manifest.webmanifest`) gives it a name, icons and standalone
display, adaptive icons and shortcuts to Approvals and Notifications. It opens the live site; there is no
offline mode by design. Push alerts and the phone layout are described in [pwa.md](pwa.md).

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
