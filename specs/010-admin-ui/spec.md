# Feature Specification: Admin Web UI, first slice (Epic E12a)

**Feature Branch**: `epic/E12a-admin-ui`

**Created**: 2026-11-25

**Status**: Implemented offline (unit, handler, golden and static accessibility tests); not yet exercised in
a browser against a live cluster (see [plan.md](plan.md), Known gaps)

**Input**: SPEC.md section 6.3 (admin dashboard), section 6.7 (clients: web primary, Android companion),
Epic E12 (dashboard and onboarding wizard) and E19 (PWA hardening). This slice is the part of the admin
UI that sits on APIs that already exist (E03 can-core, E05 forms `leads`): sign in, see what is running,
inspect modules, audit log, event deliveries and leads, and manage the operator's own credentials. The
onboarding wizard, module store/enable-disable, settings forms driven by `settings.schema.json`, DNS and
backup screens, and the attention inbox are later slices.

## Principles for this slice

- **Footprint is a feature.** Server-rendered Go `html/template` with embedded static assets. No JS
  framework, no build step, **no JavaScript at all**: every action is a plain form post, so the UI works
  without scripting and the CSP can forbid scripts entirely.
- **Same UI on phone and desktop.** One responsive layout; a web app manifest makes it installable on
  Android. The Android companion app (E19) talks to the documented JSON API, not to these pages.
- **No private back door.** The UI performs the same operations the admin API contract exposes, through
  the same services (`auth.Service`, audit log, store, registry). Account operations that have no JSON
  endpoint yet (change password, rotate TOTP, list/revoke sessions) are listed under Known gaps in the
  plan.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Sign in safely (Priority: P1)

The operator opens `admin.<domain>`, enters username, password and a TOTP code, and lands on the
dashboard.

**Acceptance Scenarios**:

1. **Given** no session, **When** any page other than the login page, the manifest or static assets is
   requested, **Then** the response is a redirect to `/login` (never page content).
2. **Given** valid credentials and code, **When** the form is posted, **Then** a `can_session` cookie
   (HttpOnly, SameSite=Strict, Secure unless plain-HTTP dev) is set and the browser is redirected to `/`.
3. **Given** an unknown user, a wrong password, a wrong or reused code, **When** the form is posted,
   **Then** the page is re-shown with the *same* message ("Invalid username, password or authentication
   code.") and status 401; the username is kept, the password never echoed.
4. **Given** the throttle in `auth.Service` refuses the attempt, **Then** the page shows a "too many
   failed attempts" message with status 429 and a `Retry-After` header; when the service is busy, 503.
5. **Given** the login form, **When** it is posted without the matching login CSRF cookie and field,
   **Then** it is rejected with 403 before any credential is checked.

### User Story 2 - See the state of the company at a glance (Priority: P1)

The dashboard shows company name and domain, one card per module (enabled/failed/disabled, last error),
the capability map (who provides what), the most recent audit entries, and webhook delivery health with a
button to requeue each dead-lettered delivery.

**Acceptance Scenarios**:

1. **Given** no modules, **Then** an empty state explains how to enable one (`make enable <module>`).
2. **Given** a failed module, **Then** its card shows the state with text (not colour alone) and the last
   error, escaped.
3. **Given** a dead delivery, **When** "Requeue" is pressed, **Then** the delivery returns to pending, an
   audit entry `delivery.requeue` is written in the same transaction, and the operator is redirected back
   with a confirmation message.

### User Story 3 - Inspect modules (Priority: P2)

A modules page lists every installed module with state, version, licence class and declared RAM (and the
total of enabled modules). The detail page shows the manifest summary: provider and licence (a
**source-available** badge when `licence_class` says so), runtime, resources, provides, consumes,
requires, publishes, subscribes, permissions, data classes.

### User Story 4 - Audit log (Priority: P2)

A viewer lists audit entries newest first with filters (actor, action prefix, target) and paging; a
"Verify chain" button runs the hash-chain verification and reports OK with entry count and head, or the
first violation; an export link downloads the whole log as JSON lines.

### User Story 5 - Events and deliveries (Priority: P2)

A list of recent deliveries with status, attempts, next attempt; a detail page with the attempt log and a
requeue button for dead deliveries.

### User Story 6 - Leads (Priority: P2)

A read-only list of captured form submissions with a status filter and cursor paging, a detail page, and
a status change form. Data is read **through the `leads` capability** (resolved by the registry) via a
`LeadsClient` interface; when no provider is enabled or the client is not configured, an empty state
says so. Lead content is untrusted and always escaped.

### User Story 7 - Manage my account (Priority: P2)

A settings page to change the password (current password required), rotate the TOTP secret (current
password required; the new secret is shown once and must be confirmed with a code generated from it, so
a mistyped or unsaved secret cannot lock the operator out), list active sessions and revoke any of them.
Changing a credential ends all sessions; the operator signs in again.

### User Story 8 - Installable and accessible (Priority: P2)

The UI has a manifest (name, icons, display `standalone`, theme colour) so Chrome on Android offers
"Install app". No service worker is registered (see research R4); authenticated pages are never cached.
Keyboard-only use works (skip link, visible focus), landmarks (`header`, `nav`, `main`, `footer`) and
headings are correct, colours meet WCAG AA contrast, `prefers-reduced-motion` and `prefers-color-scheme`
are honoured, and text can be zoomed to 200% without loss.

### Edge Cases

- Unknown URL: a styled 404 page (inside the layout when signed in, minimal otherwise).
- Hostile data (module names, last errors, lead fields, audit targets containing `<script>` or
  attribute-breaking quotes) is HTML-escaped everywhere; a test posts such data and asserts no raw
  markup reaches the page.
- Every unsafe request needs the session CSRF token as a form field (`csrf`) and is rejected with 403
  otherwise; requests whose `Sec-Fetch-Site` is `cross-site` are rejected too.
- Session expires while a form is open: the post redirects to `/login`.
- Upstream failures (store error, leads gateway down) produce a styled 500/502 page with the request id,
  never a stack trace or internal error text.

## Requirements *(mandatory)*

- **FR-001**: The UI MUST be package `core/server/adminui`, mounted by can-core on the admin listener next
  to the JSON API (`/api/v1/*`, `/healthz` go to the API; everything else to the UI).
- **FR-002**: Templates and static assets MUST be embedded (`go:embed`); no runtime file access, no build
  step, no JavaScript.
- **FR-003**: Every response MUST carry a CSP without `unsafe-inline` for scripts (`script-src 'none'`),
  `frame-ancestors 'none'`, `form-action 'self'`, `base-uri 'none'`, `Referrer-Policy: no-referrer`,
  `X-Content-Type-Options: nosniff`; authenticated pages `Cache-Control: no-store`.
- **FR-004**: Every page except `/login`, `/static/*` and `/manifest.webmanifest` MUST require a valid
  session; every POST MUST verify the CSRF token in constant time.
- **FR-005**: Login MUST reuse `auth.Service.Login` unchanged so throttling, timing equalisation,
  single-use TOTP and non-enumerating errors are exactly those of the JSON API.
- **FR-006**: Security-relevant UI actions (requeue, password change, TOTP rotation, session revoke, lead
  status change, audit verify and export) MUST be audited with the actor.
- **FR-007**: All user-visible strings MUST come from a message catalogue (`t "key"`), English only; a
  test fails when a template references a missing key or the catalogue holds an unused key.
- **FR-008**: The pages MUST pass the static accessibility checklist test (lang attribute, one `h1`, no
  skipped heading levels, labelled form controls, unique ids, landmarks, skip link, table headers, button
  names, no positive tabindex) and the stylesheet's colour pairs MUST meet WCAG AA contrast (tested).
- **FR-009**: Routes MUST match the table in [contracts/ui-routes.md](contracts/ui-routes.md) (contract
  test).
- **FR-010**: The leads view MUST read through the registered `leads` capability using the
  `LeadsClient` interface (fake in tests) and send only `status` changes (not notes) to the gateway.

## Success Criteria

- **SC-001**: All tests pass under `go test -race ./core/...`; no new third-party dependency.
- **SC-002**: The embedded assets (templates, CSS, icons) total under 100 KiB; CSS under 12 KiB.
- **SC-003**: A page render adds no measurable idle RAM (no caches beyond parsed templates).
