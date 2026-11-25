---
description: "Task list for Epic E16b: recruiting web UI"
---

# Tasks: Recruiting web UI (Epic E16b)

**Input**: [spec.md](spec.md), [plan.md](plan.md), [research.md](research.md), [data-model.md](data-model.md), [contracts/](contracts/)

**Tests**: Constitution III. Test tasks precede their implementation. `[V]` marks work that needs a
browser, a device or a cluster and is **not** done.

## Phase 1: Service additions (tests first)

- [x] T001 Tests (`svc_web_test.go`: funnel scope, my interviews and removed assignments, move reason sealed, bounded, role-limited, purged on erasure) then `svc_web.go`, migration 004, `MoveApplicationNote`, `Caps`, `Require`
- [x] T002 `SearchTalentPool`, `GetCandidateSummary`, `IsAssigned`; migration test updated for migration 004

## Phase 2: Skeleton and sessions

- [x] T003 Tests (`ui_test.go`: lifecycle, fixation, logout replay, no oracle between unknown and revoked tokens, timeouts, revocation within a minute for writers, session cap, CSRF on every POST, Sec-Fetch-Site, throttling that never blocks valid tokens) then `ui.go`, `ui_session.go`, `ui.enabled` config and mounting in `NewApp`
- [x] T004 Embedded templates, stylesheet, manifest, icon; CSP `script-src 'none'`, `no-store`; UI off by default, API and careers unaffected

## Phase 3: Pages on the Store

- [x] T005 Scope tests (hiring manager, interviewer, job-scoped token; identical 404) then requisition list, create, edit with version, lifecycle, approval request, salary publish toggle
- [x] T006 Job page with the pipeline board (forms, no drag and drop; at most 25 cards per stage), funnel counts, careers preview link for open jobs only
- [x] T007 Application page: summary, resume metadata and safe download, history with reasons, interviewer assignment and the self-unassign refusal, move, reject, hire (outbox event checked)
- [x] T008 Reveal of sealed fields (POST, audited, rate limited, role-limited), recruiter notes, candidate page (tags, talent pool), talent-pool search
- [x] T009 Blind feedback: scorecard form, constant notice, released after submit, removed interviewers stay blind and cannot submit
- [x] T010 Dashboards by capability, my interviews, diversity report (closed months, suppression drops answer names), read-only settings

## Phase 4: Quality

- [x] T011 Route contract test (`ui-routes.md` against `UIRoutes()`; every session route redirects without a session; gates name real Store methods; roles column equals the permission matrix and is probed with a token of each role)
- [x] T012 Message catalogue completeness; no literal text in templates; no inline code, `data:` or `url()`
- [x] T013 Static a11y checklist over every page; stylesheet contrast in both schemes; reduced motion
- [x] T014 Golden HTML for every page (`go test ./core/jobs -run TestUIGoldenPages -update`)
- [x] T015 Source guard: `ui*.go` contain no `Can(`, role comparison, predicate, SQL or `template.HTML`
- [x] T016 Hostile candidate names escaped on every page; 503 page on an expired deadline

## Phase 5: Wiring and docs

- [x] T017 `dev/compose/config/jobs.yaml` turns the UI on; `dev/compose/smoke.sh` checks login page, redirect, wrong token (shellchecked)
- [x] T018 `docs/recruiting-ui.md`, `docs/recruiting.md`, `docs/README.md`, `docs/local-compose.md`, epics table
- [ ] T019 [V] Run `make compose-test` against a rebuilt stack and sign in with a real token
- [ ] T020 [V] Browser, phone and screen-reader pass; verify the manifest installs
- [ ] T021 Helm: expose the `ui` block and a route (follow-up, as for people)
- [ ] T022 Shared `core/uikit` package for session, headers and the quality checkers (proposal in `research.md`; own epic)
- [x] T023 QA round 1: metadata read that decrypts nothing, reasons behind the reveal, `from` on moves, notes version, hiring-manager recheck, 503 instead of 404 on deadline
- [ ] T024 Approval decision and stage editing screens
