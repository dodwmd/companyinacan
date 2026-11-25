# Tasks (E19a), all done unless marked

Test-first: each task's tests were written and seen failing (not compiling or asserting) before its code.

- [x] T01 `netguard`: shared public-address denylist; notify's webhook channel delegates to it (`netguard_test.go`, existing `publicaddr_test.go` unchanged)
- [x] T02 `webpush`: RFC 8291 encryption against the section 5 vector, VAPID ES256 header, key marshal, endpoint and key validation, client (headers, status mapping, no redirects, private destinations refused) (`webpush_test.go`)
- [x] T03 Migration 0006 `push_subscriptions`, `push_vapid`
- [x] T04 notify: subscribe with cap and ownership, lazy VAPID key (database or mounted file), rotation, delivery with preferences, quiet hours, urgent bypass, rate limit, retries, 404/410 cleanup, failure drop, generic payload, test send with rate limit, device labels (`push_test.go`)
- [x] T05 admin API: `/api/v1/push/...` with CSRF, caps, ownership, rotation permission (`adminapi/push_test.go`)
- [x] T06 OpenAPI: routes, schemas, `webpush` preference channel; contract test green
- [x] T07 Service worker `static/sw.js` and its served headers; test that it has exactly two listeners and no interception or cache (`pwa_test.go`)
- [x] T08 Notifications page: push section, devices table, test and remove forms, `webpush` preference table, `static/push.js`, page-local CSP; every other page keeps `script-src 'none'` (`pwa_test.go`)
- [x] T09 Manifest (maskable icons, shortcuts, colours equal to CSS), head tags, safe-area and coarse-pointer CSS, stacked tables, content-hashed immutable assets (`pwa_test.go`, a11y checks)
- [x] T10 `ui-routes.md`, `docs/pwa.md`, `docs/admin-ui.md`, `docs/notifications.md`, SPEC.md section 11, epics README, research R4 note in 010
- [x] T11 Runtime wiring: `CAN_VAPID_KEY_FILE`, VAPID contact from the company domain
- [x] T12 Binary size and idle RSS measured (plan.md)
- [ ] T13 Verify on a real Android device with Chrome and Firefox against the real FCM and autopush services; measure Doze latency
- [ ] T14 Decide whether `pushsubscriptionchange` handling and per-device preferences are worth their code
- [ ] T15 Lighthouse run on a real deployment (the static checks here are not a substitute)
