---
description: "Task list for Epic E15b: Finance web UI"
---

# Tasks: Finance web UI (Epic E15b)

**Input**: [spec.md](spec.md), [plan.md](plan.md), [research.md](research.md), [data-model.md](data-model.md), [contracts/](contracts/)

**Tests**: Constitution III. Test tasks precede their implementation. `[V]` marks work that needs a
browser or cluster and is **not** done.

## Phase 1: Skeleton and sessions (US1)

- [x] T001 Tests (`ui_test.go`: lifecycle, fixation, logout replay, timeouts, revocation, CSRF, Sec-Fetch-Site, throttling, secure cookies, link token not logged) then `ui.go`, `ui_session.go`
- [x] T002 `ui:` config block (`enabled`, `insecure_cookies`, `force_secure`, `notifications_url`, timeouts), mounted in `NewApp`; `allowedBy` shared with the API guard
- [x] T003 Embedded templates, stylesheet, manifest, icon; CSP `script-src 'none'`, `no-store`; access log redacts `/public/invoices/{token}`

## Phase 2: Money and forms (US4)

- [x] T004 Tests then `ui_form.go`: `ParseMinor` (JPY/USD/KWD/negative round trip, refusals), `ParseMilli`, `ParseBP`, `ParseHM`, line rows, idempotency key, version

## Phase 3: Pages on the Store (US2-US7)

- [x] T005 Dashboard (`Store.Dashboard`) and approver queue (own work left out)
- [x] T006 Parties: list, search, create, edit, party page; erase flow with the retention explanation and the service's report
- [x] T007 Invoices: filtered paged list, draft create/edit/delete with the lines editor, approve (SoD visible and enforced), request approval (can-core), send, payments, void, credit notes, public link create/revoke (shown once), printable view, CSV exports
- [x] T008 Recurring schedules, contracts from templates (variables, versions read-only, send, dates, terminate), purchase orders
- [x] T009 Timesheets (weekly grid, submit, approve/reject, period lock shown) and expenses (receipt metadata, approval, reimbursement)
- [x] T010 Reports (aging per currency, statement) and read-only settings (`Store.ListSeries`, `PartyNames`)

## Phase 4: Quality

- [x] T011 Route contract test (`ui-routes.md` against `UIRoutes()`; every session route redirects without a session; every route enforces its scope; every named Store method exists)
- [x] T012 Message catalogue completeness (no unused entry, no literal text in templates, dynamic status families)
- [x] T013 Static a11y checklist over every page; stylesheet contrast in both schemes; reduced motion; print styles
- [x] T014 Golden HTML for 49 pages (`go test ./core/finance -run TestUIGoldenPages -update`)
- [x] T015 Source guard: UI files contain no `authorizeApproval(`, SQL or database handles; permission checks only in the route table
- [x] T016 Hostile text in text, attribute and query contexts; own-only and scoped tokens; bounded lists; deadline mapping; concurrent session use under `-race`

## Phase 5: Wiring and docs

- [x] T017 Dev compose enables the UI (`dev/compose/config/finance.yaml`); `dev/compose/smoke.sh` checks (login page, redirect, stylesheet, wrong token 401, API and public link path unchanged)
- [x] T018 Footprint measured idle and after load, templates parsed on first use, budget raised to 28/64 (`research.md` R2); `module.yaml` and chart comments
- [x] T019 `docs/finance-ui.md`, `docs/finance.md` and `docs/local-compose.md` links, `docs/epics/README.md`

## Not done

- [V] T020 Browser pass: real keyboard, screen reader and phone check; PWA install; visual review; print preview of the lines table
- [V] T021 Run the compose smoke and a sign-in with a real can-core personal access token
- [ ] T022 Chart: expose `ui.*` and route a host (needs a module subdomain; the installer refuses a route without one)
- [ ] T023 SSO-integrated sign-in (needs a can-core endpoint to redeem a one-time code for a module session)
- [ ] T024 Pages for: recording a manual signature, new contract versions and the template editor, recurring-schedule edit, tax-rate, token, audit-log, period-lock and dead-event administration (the API has them)
- [ ] T025 Server-side line count beyond the rows offered (the form offers 3 to 5 blank rows; more lines: save and edit again)
- [ ] T026 Sessions survive a restart (optional; today a restart signs everyone out)
- [x] T027 Party picker: search box and a notice when the list is cut at 100
