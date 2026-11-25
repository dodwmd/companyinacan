# Contract: push payload, service worker and routes

HTTP routes are documented where the repository's contract tests read them:

- JSON API: `specs/005-can-core-mvp/contracts/admin-api.yaml` (`/api/v1/push/...`, the `PushSubscription`,
  `PushDevice` and `PushPublicKey` schemas, and `webpush` in the `Preference` channel enum).
- UI: `specs/010-admin-ui/contracts/ui-routes.md` (`GET /sw.js`, `POST /notifications/push/test`,
  `POST /notifications/push/{id}/remove`, the static asset caching rule and the `/notifications` CSP).

`go test ./test/contract` and `go test ./core/server/adminui` fail when a served route is missing from
those files or the reverse.

## Push message (before encryption)

JSON, at most a few hundred bytes (the encrypted record limit is 3993 bytes of plaintext):

| Field | Type | Content |
|---|---|---|
| `t` | string | Title chosen by notification **type**: `Approval waiting`, `Approval escalated`, `Approval decided`, `New lead`, otherwise `New notification`; `Test notification` for the test button |
| `b` | string | Fixed text `Open the console to review.` (`Push is working on this device.` for the test) |
| `u` | string | A site path matching `^/[A-Za-z0-9_./-]{0,100}$` without `//` or `..`, otherwise `/notifications`. Normally `/approvals/<id>` |
| `i` | integer | The notification number (0 for the test); used only as the notification `tag` |

Nothing from the notification's data (names, amounts, titles, request text, lead details) is ever put in
a payload. The service worker also trims `t` to 80 and `b` to 120 characters.

## Transport headers (RFC 8030 / 8291 / 8292)

`Authorization: vapid t=<ES256 JWT>, k=<public key>` (JWT claims `aud` = push service origin, `exp` = now
+ 12 h, `sub` = `https://<company domain>` or the project URL), `Content-Encoding: aes128gcm`,
`Content-Type: application/octet-stream`, `TTL` (3600 s; 86400 s for urgent), `Urgency` (`normal`;
`high` for urgent and the test), `Topic: test` for the test only.

## Service worker

`GET /sw.js`, same origin, scope `/`:

- `Content-Type: text/javascript; charset=utf-8`, `Cache-Control: no-cache`, `Service-Worker-Allowed: /`,
  `Content-Security-Policy: default-src 'none'`.
- Listeners: `push` (always shows one notification) and `notificationclick` (closes it, focuses and
  navigates an open window of this origin, or opens one). The target is the payload's `u` only when it is
  a path on this origin, else `/notifications`.
- No other listener; no request interception; no Cache API, `indexedDB`, storage, `importScripts`.
  A test fails if any of these words appear in the file.
