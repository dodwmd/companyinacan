---

description: "Task list for Epic E12a: Admin Web UI, first slice"
---

# Tasks: Admin Web UI, first slice (Epic E12a)

**Input**: [spec.md](spec.md), [plan.md](plan.md), [research.md](research.md), [data-model.md](data-model.md), [contracts/](contracts/)

**Tests**: Constitution III. Test tasks precede their implementation tasks. `[V]` marks work that needs
a browser or cluster and is **not** done.

## Phase 1: Backend helpers

- [x] T001 Tests then `store/sessions.go` (ListSessions, RevokeSession, PublicSessionID), `store/delivery_get.go`
- [x] T002 Tests then `audit/query.go` (Filter, Query newest first, literal prefix)
- [x] T003 Tests then `auth/account.go` (ChangePassword, RotateTOTP, ConfirmPassword, per-account throttle, audit)

## Phase 2: UI skeleton and login (US1)

- [x] T004 Handler harness over the real `auth.Service` (`harness_test.go`)
- [x] T005 Tests: every page requires a session, CSRF on every POST, cross-site refused, login CSRF, indistinguishable failures, 429 with Retry-After, logout, security headers, no scripts or inline styles (`handlers_test.go`)
- [x] T006 `adminui.go` (routes, session, CSRF, headers, rendering, static), `layout.html`, `login.html`, `error.html`, `admin.css`

## Phase 3: Pages (US2 to US7)

- [x] T007 Dashboard, modules, module detail (+ tests, hostile data)
- [x] T008 Audit viewer, verify, export (+ tampering test)
- [x] T009 Events, event detail, requeue (+ audit, conflict, open-redirect tests)
- [x] T010 `LeadsClient`, `HTTPLeads`, leads list/detail/status (+ fake and gateway contract tests)
- [x] T011 Settings: password, TOTP rotation with confirmation, sessions

## Phase 4: Quality gates (US8)

- [x] T012 Golden HTML for 24 page states (`golden_test.go`, `testdata/golden/`)
- [x] T013 Static accessibility checklist + checker self-test; stylesheet contrast test (`a11y_test.go`)
- [x] T014 Catalogue test; asset budget test; route contract test (`messages_test.go`, `handlers_test.go`)
- [x] T015 Manifest, icons (PWA basics); no service worker (research R4)

## Phase 5: Integration and docs

- [x] T016 Mount in `runtime` (+ test that the UI is on the admin listener only), env vars, `deployment.md`
- [x] T017 `docs/admin-ui.md`, this spec folder
- [ ] T018 [V] Manual pass: browser, keyboard, screen reader, 200% zoom, Android install
- [ ] T019 [V] Provide the leads credential (chart + gateway introspection) and company env from `company.yaml`
- [ ] T020 JSON endpoints for account operations in `adminapi` + OpenAPI (needed by E19)
