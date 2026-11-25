# Research: PWA Hardening and Web Push (E19a)

**R1. PWA first, native only if proven (the decision recorded in SPEC.md section 11).** The Android
companion app's job (approvals, notifications, a few lists) is served by an installed PWA: it needs an
installable manifest, a phone layout and push alerts, and all three are web features. Web Push works in
Chrome and Firefox on Android and needs no Play Store, no Google Play Services dependency beyond the
browser's own push channel and no second codebase. The native Kotlin app stays an option, to be started
only when a concrete PWA limit is proven: for example push delivery that Doze or the browser makes
unacceptably late for approvals, a need to keep data on the device that a browser cannot protect, or an
API a PWA cannot reach. Until then there is nothing to maintain twice.

**R2. The service worker has two listeners and no more.** research R4 of
[010-admin-ui](../010-admin-ui/research.md) refused a service worker because one that caches responses
can serve one user's authenticated page to the next user of a shared device. Push needs a worker, but
not a caching one: `push` and `notificationclick` are the only listeners, there is no `fetch` listener
(requests go straight to the network exactly as today) and no Cache API. Consequences accepted: no
offline shell and no offline fallback page (a navigation that fails offline shows the browser's own
error), and no `pushsubscriptionchange` handler (a browser that rotates a subscription is re-registered
the next time the user opens the page and presses the button; until then the old one yields 404/410 and
is deleted). Chrome has not required a fetch handler for installability since version 108. A test fails
if the file mentions `fetch`, `Cache`, `caches`, `importScripts`, storage APIs or `skipWaiting`.

**R3. Only the notifications page runs script.** The CSP of every page stays `script-src 'none'`. The
`/notifications` page replaces it with `script-src 'self'; worker-src 'self'; connect-src 'self'` and
loads one external deferred script, `/static/push.js`. No nonce and no inline script are needed, no
`unsafe-*`, and there is no other source. The script only adds the "enable on this device" step: it asks
for permission, registers `/sw.js`, subscribes with the server's VAPID key and posts the subscription to
the JSON API with the CSRF header. Everything else on the page is plain forms. A single decision keeps
this small: the script never builds HTML, never reads storage and never evaluates strings.

**R4. Implement Web Push in the standard library.** The needs are small: ECDH P-256 (`crypto/ecdh`),
HKDF (`crypto/hkdf`, in the standard library since Go 1.24; no import of `golang.org/x/crypto` is needed
at all), AES-128-GCM, ECDSA P-256 signing and one HTTP POST. The package is about 400 lines including
validation and is tested against the RFC 8291 section 5 worked example byte for byte. A third-party
library (for example `SherClockHolmes/webpush-go`) would add a module for the same code. Measured cost is
in [plan.md](plan.md).

**R5. Payloads carry no data.** Push services and the browser vendor see the message envelope and could
see the payload if encryption were misused, and a lock screen shows the notification to anyone nearby.
So the message is a generic title by type, a fixed line and a path. Sensitive detail (who, how much, for
what) is shown only after sign-in on the page the path opens. The path is checked twice: on the server
(`^/[A-Za-z0-9_./-]{0,100}$`, no `//`, no `..`, else `/notifications`) and in the worker (same origin).

**R6. The endpoint is attacker-supplied.** Any signed-in user can post a subscription, and can-core will
connect to its URL. Defence in depth: validation at subscribe time (https, port 443, a DNS name with a
public-looking suffix, no address literal, userinfo, fragment or control characters, at most 500
characters), an HTTP client that refuses non-public destinations at connect time after DNS (shared
`core/server/netguard` with the webhook channel, so a name that resolves to 10.0.0.5 or 169.254.169.254
fails), no redirects, no proxy from the environment, bounded timeouts and a 4 KiB response cap, and errors
that never contain the URL (it is a capability: whoever has it can push to that browser). The endpoint is
never logged, never shown (only the push service's host) and never sent to the page. A host allowlist of
known push services was rejected: self-hosted push servers and new vendors would be locked out, and the
connect-time check covers the risk.

**R7. VAPID key storage.** One P-256 key pair per installation, generated on first use. By default the
private scalar is a row in the state database (which already holds session hashes and webhook secrets,
and is on the encrypted-backup path); `CAN_VAPID_KEY_FILE` points at a mounted Secret instead, and then
nothing is written to the database. There is no master key in can-core to seal the row with, so it is
protected by the database file's permissions and the volume, which is stated as a limit. Rotation
replaces the pair and deletes every subscription (a browser binds a subscription to the application
server key it was made with, so old ones can no longer be pushed to) and needs `rbac.manage`.

**R8. Delivery semantics.** Push is real time: quiet hours suppress it (urgent notices bypass them),
a digest setting does not, because digests batch email. A suppressed push is not queued; the inbox is
the record. A notice never waits on the push service: delivery runs in a goroutine (at most eight in
flight, otherwise `suppressed: push queue is full`), with three attempts and `Retry-After` honoured
(capped at 60 s), inside a 30 s budget. Urgent notices use `Urgency: high` and a 24 h TTL; others
`normal` and 1 h (a stale approval alert is noise).

**R9. Mobile layout without changing markup semantics.** Four tables become one card per row below 40 rem
with CSS only (`table.rows`, `td[data-label]::before`); the header row is hidden visually and kept in the
accessibility tree. Known limit: Safari historically drops table semantics when table elements get
`display: block`; Chrome and Firefox keep them. Safari on iOS is out of scope.

**R10. Static caching by content hash.** Templates link `/static/<file>?v=<first 10 hex of SHA-256>`; a
request carrying the current hash is `immutable` for a year; anything else keeps the one-hour policy. The
hashes are computed once, lazily, from the embedded files. Golden test files contain the hash, so a change
to the stylesheet or a script updates them (`go test ./core/server/adminui -run Golden -update`).
