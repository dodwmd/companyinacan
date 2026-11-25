---
description: "Task list for Epic E13c: People self-service web UI"
---

# Tasks: People web UI (Epic E13c)

**Input**: [spec.md](spec.md), [plan.md](plan.md), [research.md](research.md), [data-model.md](data-model.md), [contracts/](contracts/)

**Tests**: Constitution III. Test tasks precede their implementation. `[V]` marks work that needs a
browser or cluster and is **not** done.

## Phase 1: Skeleton and sessions (US1)

- [x] T001 Tests (`ui_test.go`: lifecycle, fixation, logout replay, timeouts, CSRF, Sec-Fetch-Site, throttling) then `ui.go`, `ui_session.go`
- [x] T002 `Actor.Active`; leaver loses every route (test first)
- [x] T003 Embedded templates, stylesheet, manifest, icon; CSP `script-src 'none'`, `no-store`

## Phase 2: Pages on E13a services (US2-US5)

- [x] T004 Home, tasks, approvals seam (only when wired), notifications link
- [x] T005 Profile edit; sensitive screen (`SensitiveFieldsFor`, audited reveal by POST)
- [x] T006 Directory search, person page, org chart (bounded, acyclic), team page; leave masked
- [x] T007 1:1 series, meetings, agenda, shared and manager-private notes, actions
- [x] T008 Goals, key results, progress log, alignment, visibility (`CanWriteGoal`)

## Phase 3: Time off and reviews on E14b services (US6, US7)

- [x] T009 Tests (`ui_e14b_test.go`) then `ui_pages_e14b.go`: balances, request with half days, submit, cancel, detail, manager queue and decision, masked calendar
- [x] T010 Reviews list and page, self and manager forms, nominations and their approval, acknowledgement, `ReviewCapabilities`
- [x] T011 Feedback inbox and form; no counts or reviewer state while the cycle is open; calibration never drawn for the subject

## Phase 4: Quality

- [x] T012 Route contract test (`ui-routes.md` against `UIRoutes()`; every session route redirects without a session; gates name real Service methods)
- [x] T013 Message catalogue completeness; no literal text in templates
- [x] T014 Static a11y checklist over every page; stylesheet contrast in both schemes; reduced motion
- [x] T015 Golden HTML for every page (`go test ./core/people -run TestUIGoldenPages -update`)
- [x] T016 Source guard: UI files contain no `Decide(`, `FieldAccess(`, role checks or SQL

## Phase 5: Wiring and docs

- [x] T017 `ui:` config block, mounted in `cmd.go`; dev compose enables it (`dev/compose/config/people.yaml`)
- [x] T018 `dev/compose/smoke.sh` UI check (login page, redirect, wrong token 401, API unchanged)
- [x] T019 `docs/people-ui.md`, `docs/people.md` link, `docs/epics/README.md`

## Not done

- [V] T020 Browser pass: real keyboard, screen reader and phone check; PWA install; visual review
- [V] T021 Run the compose smoke and a login with a real PAT against can-core
- [ ] T022 SSO-integrated login (needs a can-core endpoint to redeem a one-time code for a module session)
- [ ] T023 HR administration screens (cycles, templates, calibration, release, leave configuration)
- [ ] T024 Chart: expose `ui.*` through `modules/people/chart` values and a route on `people.<domain>`
- [ ] T025 Sessions survive a restart (optional; today a restart signs everyone out)
