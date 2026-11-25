# Installing the admin UI on a phone, and push notifications

The admin UI is a progressive web app (PWA). On Android it installs from Chrome, opens full screen, and can
send push alerts. This is the first mobile story; a native Android app stays an option, to be started only
if a limit of the PWA is proven (SPEC.md section 11). iOS and macOS are out of scope (SPEC.md 6.6).

Built offline and tested with fakes only. Nothing here has been tried on a real phone, a real browser or
the real push services; see [Not verified](#not-verified).

## Install on Android (Chrome)

1. Open `https://admin.<your-domain>/` in Chrome and sign in. The site must be served over HTTPS (the
   ingress does this; plain HTTP does not install and cannot use push).
2. Menu (three dots), then "Install app" (or "Add to Home screen").
3. Open it from the home screen. Long-press the icon for the shortcuts "Approvals" and "Notifications".

The app opens the live site: there is no offline mode, by design. Every page is live data behind a login,
so the service worker caches nothing and pages are `no-store`; if you are offline a page fails to load as
in a browser tab. Pages are server-rendered and small (the stylesheet is about 12 KiB, there is no
JavaScript on any page except Notifications), which is what keeps them quick on a slow connection.

Phone layout: the approvals inbox, notifications, push devices and module catalogue tables become one
card per row below 40 rem; controls are at least 44 px high on touch screens; content stays clear of the
notch and gesture bar; the light and dark colour schemes follow the phone.

## Turn on push

1. Open Notifications in the installed app (or in Chrome or Firefox for Android).
2. Under "Push notifications" press "Enable push on this device" and allow notifications.
3. The device appears in "Your push devices". Press "Send a test notification" (one per 30 seconds).

Each device is enabled on its own; remove one with its Remove button, or with "Disable push on this
device". Up to 10 devices per user. Enabling needs JavaScript and a browser with Web Push; the list,
remove and test controls are ordinary forms and work without JavaScript.

What you get: a short alert (for example "Approval waiting") that opens `/approvals/<id>` when tapped,
signing you in first if needed. The alert never contains names, amounts, titles or request text, because it
can show on a lock screen and passes through the browser vendor's push service. Details appear only after
you sign in.

Rules it follows (same engine as email, see [notifications.md](notifications.md)):

- Per-type switches under "Preferences" (Push on my devices). Default on once a device exists.
- Quiet hours hold push for ordinary notices (they stay in the inbox); urgent notices still arrive.
- A digest setting batches email only; push stays immediate.
- The same hourly limit per person as other external channels.
- A device whose push service says it is gone (HTTP 404 or 410) is removed; a device that fails ten times in
  a row is removed. Enable push again on it.

## For the operator

- The server key (VAPID) is created the first time anyone enables push and then kept. By default it is a row
  in the state database (`push_vapid`; protect and back up the database as you already do). To keep it in a
  Secret instead, mount a file holding the private key (base64url, 32 bytes) and set `CAN_VAPID_KEY_FILE`
  to its path; nothing is then written to the database, and a missing or bad file fails push loudly rather
  than falling back to a generated key.
- **Production: use `CAN_VAPID_KEY_FILE`.** A key kept in a mounted Secret never touches the database or its
  backups and is rotated like any other Secret. The default (a row in the state database) is convenient for
  trials; there is no master key in can-core to seal it with.
- **What the database holds in plain text.** Each push device row stores the browser's push `endpoint` (a
  capability URL: whoever has it can send that browser pushes, though only can-core holds the VAPID key that
  push services require) and the browser's `auth` secret and public key (with the VAPID private key, enough to
  read or forge messages for that device). Protect the state database file's permissions and use volume
  encryption; treat a copy of the database as sensitive. Devices are erased when their user is deleted or
  unlinked (SSO), so a user provisioned again with the same name starts with none.
- **Optional endpoint allowlist: `CAN_PUSH_ENDPOINT_ALLOW`.** Comma-separated host suffixes a push endpoint
  must match (a host equal to a suffix or a subdomain of it). Default empty: any public https host on port 443.
  It is checked when a device is added and again before every send. Common suffixes: `googleapis.com`
  (Chrome and Android via FCM, `fcm.googleapis.com`), `push.services.mozilla.com` (Firefox), `push.apple.com`
  (Safari, not supported here), `notify.windows.com` (Edge via WNS). Example for Chrome and Firefox only:
  `CAN_PUSH_ENDPOINT_ALLOW=googleapis.com,push.services.mozilla.com`. A self-hosted UnifiedPush server needs its
  own suffix added. Existing devices that no longer match fail and are dropped after ten failures.
- Rotating the key (`POST /api/v1/push/vapid-key/rotate`, needs `rbac.manage`, audited) deletes every
  subscription, because browsers bind a subscription to the key; users enable push again on each device.
  With a mounted key, rotate the Secret instead, and expect the same.
- `CAN_COMPANY_DOMAIN`, when set, is sent to push services as the contact (`https://<domain>`, the VAPID
  `sub` claim); otherwise the project URL.
- can-core connects out to whatever endpoint a browser gives it, so outbound HTTPS to the browser vendors'
  push services (for Chrome, `fcm.googleapis.com`; for Firefox, `*.push.services.mozilla.com`) must be
  allowed by your network policy. Endpoints must be https on port 443 with a public DNS name; names that
  resolve to private, loopback or link-local addresses are refused at connect time; redirects are not
  followed.
- Footprint: nothing push-related runs until the first subscription. See the measurements in
  `specs/025-pwa-webpush/plan.md`.

## Limitations

- **Doze and battery.** Android delays background network traffic when the phone is idle. Urgent notices
  use the highest push urgency, normal ones the default; delivery can still be seconds to minutes late (or
  until the phone is next used), and the manufacturer's battery savers may stop the browser entirely. If
  approvals must reach someone within a minute, that is the case for the native app. Time to deliver was not
  measured.
- **Requires Google's or Mozilla's push service.** A browser's push goes through its vendor (Chrome through
  FCM). The payload is end-to-end encrypted, and carries no data anyway, but the vendor sees that a message
  was sent and when. A privacy-first alternative (UnifiedPush) needs the native app.
- **No iOS or macOS.** Out of scope for now.
- **No offline mode.** No caching of any authenticated page, ever (the service worker has no `fetch`
  handler and no cache).
- **A browser that replaces its subscription** (rare) is not re-registered in the background; open
  Notifications and press the button again. Until then it is cleaned up by the push service's 404/410.
- **Table layout on phones** keeps the table semantics in Chrome and Firefox; Safari may read the cards
  as plain text (not a supported client).
- **Lighthouse.** The repository tests check what can be checked statically (manifest, icons, viewport,
  contrast, labels, touch targets in CSS). A Lighthouse run on a real deployment has not been done.

## Not verified

No real browser, Android device, FCM or Mozilla push service was used. The encryption is checked against the
RFC 8291 worked example and the request shape against a fake push service. Not known until tried: whether
Chrome offers the install prompt and shows the shortcuts and maskable icon as intended; whether the real
services accept our VAPID token and message; Doze latency; whether tapping an alert focuses the installed
app window; Firefox for Android behaviour.
