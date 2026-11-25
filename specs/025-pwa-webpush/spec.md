# Feature Specification: PWA Hardening and Web Push (Epic E19a)

**Feature Branch**: `epic/E19a-pwa-webpush`

**Created**: 2026-11-25

**Status**: Implemented offline against `httptest` fakes. No real browser, no FCM or Mozilla autopush
service and no Android device has been used (see the unverified list in [plan.md](plan.md)).

**Input**: Epic E19 in `docs/epics/README.md` ("Android companion app ...; PWA hardening for the web
UI") and SPEC.md section 6.6 ("Clients: web and Android"). The open risk "Android app scope and stack"
is decided here: **PWA first, native Kotlin only if PWA limits are proven**. Builds on
[E12a](../010-admin-ui/spec.md) (admin UI, manifest, CSP `script-src 'none'`) and
[E11a](../015-approvals-notifications/spec.md) (notifications engine, channels, preferences, quiet
hours, digests, rate limits).

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Install the admin UI on an Android phone (Priority: P1)

An operator opens the admin site in Chrome on Android, signs in and installs it to the home screen. It
opens full screen with the right colours, a proper adaptive icon, shortcuts to Approvals and
Notifications, content that respects the notch and gesture bar, tables that read as cards on a narrow
screen and controls big enough for a thumb.

**Independent Test**: `core/server/adminui/pwa_test.go`: manifest validity (id, scope, start URL,
standalone, colours equal to the stylesheet's, separate `any` and `maskable` icons that exist and match
their declared sizes, shortcuts that are served in-scope paths, no screenshots), viewport and theme
meta tags on every page, stylesheet rules (safe-area insets, coarse-pointer 44 px targets, stacked
tables), content-hashed immutable static assets, and the accessibility checklist over the changed pages.

**Acceptance Scenarios**:

1. **Given** a page, **When** it is rendered, **Then** it has `viewport` with `viewport-fit=cover`,
   light and dark `theme-color`, `color-scheme`, the manifest link and `lang`.
2. **Given** the manifest, **When** it is read, **Then** `theme_color` and `background_color` equal the
   stylesheet's `--accent` and `--bg`, and every icon file exists with the declared size.
3. **Given** `/static/admin.css?v=<hash>` with the current hash, **When** requested, **Then** the answer
   is `public, max-age=31536000, immutable`; without the hash or with a stale one, one hour.

### User Story 2 - Get a push alert when an approval is waiting (Priority: P1)

A user opens Notifications, taps "Enable push on this device", allows notifications and sees the device
listed. When an approval is assigned to them the phone shows "Approval waiting"; tapping it opens
`/approvals/<id>` in the installed app (after sign-in if the session ended).

**Independent Test**: `core/server/notify/push_test.go` (delivery, payload, retries, cleanup),
`core/server/notify/webpush/webpush_test.go` (RFC 8291 vector, VAPID, endpoint validation, client),
`core/server/adminapi/push_test.go` (routes, CSRF, caps) and `core/server/adminui/pwa_test.go` (page).

**Acceptance Scenarios**:

1. **Given** a subscribed device, **When** a notice is created for its user, **Then** one encrypted
   message is posted to the device's push service with VAPID authorisation, `TTL` and `Urgency`, and the
   delivery log records `sent`.
2. **Given** the push service answers 404 or 410, **When** a delivery is attempted, **Then** that
   subscription is deleted; **given** 429 or 5xx, **Then** it is retried up to three times honouring
   `Retry-After`; **given** another 4xx, **Then** it is not retried and counts against the device.
3. **Given** quiet hours, **When** a non-urgent notice arrives, **Then** no push is sent (`suppressed`,
   the inbox still has it); urgent notices bypass quiet hours; a digest setting never delays push.
4. **Given** a per-type `webpush` preference set to off, **Then** that type is not pushed.
5. **Given** the payload, **Then** it carries only a generic title by type, a fixed line, a safe site
   path and the notification number: never names, amounts, titles or text.

### User Story 3 - Manage push devices, with or without JavaScript (Priority: P2)

On Notifications a user sees their devices (coarse label, push service host, added, last delivered),
removes one, and sends a test notification (one per 30 seconds). Only turning push on for the current
device needs script.

**Acceptance Scenarios**:

1. **Given** JavaScript is off, **When** the page loads, **Then** the devices, the remove buttons and the
   test button work as plain forms; the enable button stays hidden.
2. **Given** a signed-in user, **When** they post or delete a subscription, **Then** the CSRF token is
   required, the endpoint is validated (https, port 443, public DNS name), at most 10 devices per user
   are kept, and nobody can remove another user's device.

### User Story 4 - Safe by construction (Priority: P1)

The service worker cannot cache or intercept anything, only the notifications page may run a script, and
the push endpoint a user supplies cannot be used to probe the cluster.

**Acceptance Scenarios**:

1. **Given** `/sw.js`, **Then** it has exactly the listeners `push` and `notificationclick`, no
   interception of requests, no cache storage, and it only opens same-origin paths.
2. **Given** any page except `/notifications`, **Then** its CSP is unchanged (`script-src 'none'`) and
   it contains no script element; `/notifications` allows `'self'` scripts, workers and connections
   only, with no inline script.
3. **Given** a subscription whose endpoint is `http:`, an address literal, loopback, link-local,
   internal, has userinfo or a non-443 port, **Then** it is refused; **given** a public-looking name that
   resolves to a private address, **Then** the connection is refused at connect time; redirects are never
   followed.

## Requirements *(mandatory)*

- **FR-001** The manifest declares `id`, `scope`, `start_url`, `display: standalone`, theme and
  background colours equal to the CSS, separate `any`/`maskable` icons (192, 512) and shortcuts to
  `/approvals` and `/notifications`; no screenshots.
- **FR-002** Every page has `viewport-fit=cover` and light/dark `theme-color`; the stylesheet handles
  safe-area insets, `prefers-color-scheme`, 44 px coarse-pointer targets and stacked rows below 40 rem
  for the inbox, approvals, devices and catalogue tables.
- **FR-003** Static assets are linked with a content hash and served immutable for that hash.
- **FR-004** `/sw.js` (scope `/`, `Service-Worker-Allowed: /`, `Cache-Control: no-cache`, CSP
  `default-src 'none'`) handles only `push` and `notificationclick`; no `fetch` listener, no Cache API.
- **FR-005** `webpush` is a notification channel: per-type preference (default on once a device exists),
  quiet hours (urgent bypass), no digest, per-user hourly rate limit shared with the engine's limit,
  delivery-log rows, `notification.sent.v1` on success.
- **FR-006** Delivery encrypts per RFC 8291 (`aes128gcm`), authorises per RFC 8292 (VAPID, ES256) and
  sets `TTL` and `Urgency` per RFC 8030; retries 429/5xx up to three attempts; deletes 404/410
  subscriptions; drops a device after 10 consecutive failures.
- **FR-007** API: `GET /api/v1/push/vapid-public-key`, `GET|POST|DELETE /api/v1/push/subscriptions`,
  `POST /api/v1/push/test`, `POST /api/v1/push/vapid-key/rotate` (needs `rbac.manage`); CSRF on every
  unsafe method; request bodies capped at 8 KiB; 10 devices per user; documented in the OpenAPI file.
- **FR-008** UI: devices table, remove and test forms (no JavaScript needed), a `webpush` preference
  table, `static/push.js` for enabling the current device.
- **FR-009** The VAPID key pair is created on first use and persists; `CAN_VAPID_KEY_FILE` supplies it
  from a mounted Secret instead; rotation drops every subscription; nothing is created, started or
  allocated for users who never subscribe.
- **FR-010** Payloads never contain notification data, only the generic fields of
  [contracts/push-payload.md](contracts/push-payload.md).

## Out of Scope

iOS and macOS (SPEC 6.6), an offline mode or any caching of authenticated pages, background sync,
`pushsubscriptionchange` handling, badge counts, rich notifications with actions, UnifiedPush/FCM
native delivery (the native Android app stays a later option), per-device preferences, and any real
push-service, browser or device verification.
