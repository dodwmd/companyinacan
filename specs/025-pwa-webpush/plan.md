# Implementation Plan: PWA Hardening and Web Push (Epic E19a)

**Branch**: `epic/E19a-pwa-webpush` | **Date**: 2026-11-25 | **Spec**: [spec.md](spec.md)

## Summary

Make the admin UI a good installed app on Android (manifest, phone layout, hashed static assets) and add
a `webpush` channel to the notifications engine, with a push-only service worker and one page-local
script. Web Push (RFC 8030, 8291, 8292) is implemented with the standard library. Decisions:
[research.md](research.md); tables: [data-model.md](data-model.md); contracts:
[contracts/push-payload.md](contracts/push-payload.md) and the extended OpenAPI and `ui-routes.md`.

## Technical Context

**Language/Version**: Go 1.27 (existing module). **Dependencies**: none added (`crypto/ecdh`,
`crypto/hkdf`, `crypto/ecdsa`, `net/http`).
**Storage**: SQLite migration 0006 (`push_subscriptions`, `push_vapid`).
**Testing**: `go test -race`; an `httptest` TLS server stands in for the push service; the RFC 8291
section 5 vector pins the encryption; fakes for the sender in the engine and UI tests. No test contacts a
real push service, resolves a real name or opens a browser.
**Constraints**: idle footprint must not grow meaningfully (can-core idles near its 48 MiB request); no
JavaScript outside two small files; nothing authenticated is cached.

## Constitution Check

| Principle | Assessment |
|---|---|
| I. Spec-first | SPEC.md 6.6 names web and Android clients and push; section 11's open risk is now decided and recorded there. This feature restates only what it builds. |
| II. Footprint is a feature | Push machinery is lazy: no goroutine, key, client or map exists until a user subscribes or asks for the key; a notice for a user without devices costs one indexed query. No dependency added. Measured below; the idle RSS delta is reported, not hidden. |
| III. Test-first | Tests for the encryption (RFC vector), VAPID, endpoint validation, client, engine delivery, API, and the PWA surface were written before the code and failed to compile or run until it existed (`webpush`, `push.go`, `push.go` in adminapi, `pwa_test.go`). |
| IV. Stable module contracts | No module contract changes. The admin API gains routes and the `webpush` value of the preference channel (additive); `notify_prefs`/`notify_deliveries` channel values are free text. |
| V. Secure and private by default | CSRF on every unsafe route; endpoints validated at subscribe time and again at connect time, no redirects, no proxy; per-user cap and body caps; payloads carry no data; the service worker cannot cache or intercept; only `/notifications` relaxes the CSP and only to `'self'`; VAPID private key never leaves the server and is never logged; endpoints never logged or shown. |
| VI. Open-source hygiene | No new dependency (licence file unchanged); commits signed off. |
| VII. Simplicity and measured claims | Binary and idle RSS measured below. Everything that needs a real browser, push service or phone is listed under "Not verified". |

**Result**: pass, no violations to justify.

## Project Structure

```text
core/server/netguard/            shared public-address denylist (moved out of notify; the webhook channel uses it)
core/server/notify/webpush/      RFC 8291 encryption, VAPID (RFC 8292), endpoint validation, client (RFC 8030)
core/server/notify/push.go       subscriptions, VAPID key lifecycle, delivery, test send, device labels
core/server/adminapi/push.go     /api/v1/push/...
core/server/adminui/             sw.js, push.js, manifest, notifications page, hashed assets, CSS
core/server/store/migrations/0006_webpush.sql
docs/pwa.md
```

## Measurements (this machine, 2026-11-25, `go build -trimpath -ldflags="-s -w"`, base = origin/main including SSO)

| | before (origin/main) | after | delta |
|---|---|---|---|
| `can-core` binary | 19,394,823 bytes | 19,534,087 bytes | +139,264 bytes (+0.7%) |
| idle VmRSS, `can-core serve`, empty database, 6 s after start, 4 runs each | 49,280 / 48,408 / 48,288 / 48,688 kB (median about 47.4 MiB) | 48,384 / 48,536 / 48,536 / 48,968 kB (median about 47.4 MiB) | within run-to-run noise (about 0.5 MiB); an earlier 3-run comparison on the pre-SSO base showed +0.6 MiB (46.4 to 47.1 MiB) |

So the honest statement is: binary +0.14 MB, idle RSS between 0 and about +0.6 MiB with no subscription
(larger binary pages, the new migration; no push state exists). The cost with active subscriptions
(one HTTP transport, a few KiB per device, at most eight delivery goroutines) was not measured under
load. These runs use the fake token reviewer and no modules, so the absolute numbers differ from the
50.6 MiB PR #30 measured in its environment; only the before/after pair is meaningful.

## Not verified

No real browser, FCM, Mozilla autopush or Android device was used. Unverified: that Chrome accepts this
manifest as installable and shows the shortcuts and maskable icon as intended; that a real push service
accepts our VAPID JWT and encrypted body (the encryption is verified against the RFC vector and the
request shape against a fake); how late Doze delays high-urgency pushes; that the stacked-table layout
reads well on a phone; that the service worker's `notificationclick` focuses and navigates an installed
PWA window; battery and data cost; Firefox Android behaviour.

## Complexity Tracking

None.
