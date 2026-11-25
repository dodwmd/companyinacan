---

description: "Task list for Epic E14c: Work tracker web UI"
---

# Tasks: Work Tracker Web UI (Epic E14c)

**Input**: `/specs/023-work-ui/` (spec, plan, research, data-model, contracts)

**Tests**: Required (Constitution III). Red first: the test files were written and failed to build
(`UIRoutes` undefined), then failed on behaviour, before the handlers existed. Golden files are
generated from the finished pages and reviewed.

## Phase 1: Contracts and tests first

- [x] T001 `contracts/ui-routes.md`: route, session and permission table
- [x] T002 Tests (red): `ui_test.go` (login, fixation, revocation, throttling, CSRF, scope on every route, read-only, headers, issue lifecycle with stale versions, board, sprints, filters and pagination, epics, dashboard, hostile content, oversized forms, session bounds), `ui_routes_test.go` (contract and permission table), `ui_markdown_test.go`

## Phase 2: Implementation

- [x] T003 `ui_session.go`: bounded in-memory sessions
- [x] T004 `ui.go`: routes, middleware (session, per-subject limit, Fetch Metadata and Origin, form limit, CSRF, permission), headers, error mapping, static files, 404/405
- [x] T005 `ui_auth.go`: login with double-submit cookie, failure-only throttling, new session id on every login, logout
- [x] T006 `ui_markdown.go`: safe text subset and URL gate
- [x] T007 `ui_pages.go`: project list, backlog with filters and pagination, idempotent create, dashboard, epics
- [x] T008 `ui_issue.go`: issue page, edit, transition, priority, assignee, sprint, rank, comments, links, watch, conflict page
- [x] T009 `ui_board.go`: board with move and rank buttons, sprint planning
- [x] T010 `webui/templates`, `webui/static/work.css`, icon, manifest; `ui_messages.go` catalogue
- [x] T011 Mount in `app.go` and `api.go` (the API handler and its tests are unchanged)

## Phase 3: Quality gates

- [x] T012 Golden pages (28) `ui_golden_test.go`, static accessibility checklist and contrast test `ui_a11y_test.go`, catalogue test `ui_messages_test.go`
- [x] T013 `go test -race ./core/work/... ./test/...` and `make test`
- [x] T014 `docs/work-ui.md`, updates to `docs/work-tracker.md`, `docs/local-compose.md`, `docs/epics/README.md`, `modules/work/README.md`
- [x] T015 `dev/compose/smoke.sh`: sign in through the edge and read the SMOKE project

## Gaps (not done, not hidden)

- [ ] G001 SSO sign-in to the module UI (waits for the SSO epic; design in research.md R2)
- [ ] G002 A public route for `/ui/` through can-core (the work module has no `runtime.subdomain`; the chart's `ingress.pathPrefixes` can carry `/ui` once it does)
- [ ] G003 Manual screen-reader, keyboard and real-phone pass; the checklist is static
- [ ] G004 Create projects, edit workflows, manage tokens and upload attachments from the UI
- [ ] G005 Keyboard shortcuts, named saved filters, bulk edit, people picker, burn-down chart
- [ ] G006 PNG icons for installable PWA on every platform (the manifest carries an SVG icon)
- [ ] G007 Run the compose stack's UI check in CI (the compose stack is not part of the CI jobs)
