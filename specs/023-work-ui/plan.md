# Implementation Plan: Work Tracker Web UI (Epic E14c)

**Branch**: `epic/E14c-work-ui` | **Date**: 2026-11-25 | **Spec**: [spec.md](spec.md)

**Input**: Feature specification from `/specs/023-work-ui/spec.md`

## Summary

Server-rendered HTML inside `can-work` (`core/work`, files `ui*.go`, assets in
`core/work/webui/`), mounted on the same mux as the JSON API under `/ui/`. The UI is a thin layer
over the `Store`: it authenticates with the existing `Authenticator` chain (bootstrap token, stored
tokens, can-core personal access tokens), keeps the presented token in an in-memory session, and
calls the same store methods the API calls with the same `Principal`, so project scope and
permissions are not re-implemented. Choices are in [research.md](research.md), runtime state in
[data-model.md](data-model.md), routes in [contracts/ui-routes.md](contracts/ui-routes.md).

## Technical Context

**Language/Version**: Go 1.27 (existing module)

**Primary Dependencies**: standard library only (`html/template`, `embed`, `net/http`); no new
dependency, no JavaScript, no font or image downloads

**Storage**: none new. Sessions are in memory; everything else is the tracker's SQLite database

**Testing**: `go test` with `httptest` over the real handler and a cookie-keeping test browser;
golden HTML files; a static accessibility checklist; stylesheet contrast check; XSS and CSRF
tests; the route table is a test against the contract document

**Target Platform**: any modern browser including phone browsers (PWA manifest); linux static binary

**Performance Goals**: human speed; every page is a handful of indexed queries, each page and
action bounded (see FR-008)

**Constraints**: strict CSP (`default-src 'none'; style-src 'self'; ...`, no scripts), no inline
styles, tokens never leave server memory, embedded assets about 45 KB

**Scale/Scope**: about 2.3 k lines of Go (of which 340 are the message catalogue), 11 templates, one stylesheet,
and about 1.5 k lines of tests

## Constitution Check

| Principle | Status | Notes |
|---|---|---|
| I. Spec-first | PASS | SPEC.md 3.3 and the clients line (web and Android). The epic README row E14 lists the web UI as pending; this slice closes it. No SPEC.md change needed |
| II. Footprint | PASS, declared request raised | No new process or dependency. Static linux/amd64 build (`CGO_ENABLED=0 -trimpath -ldflags "-s -w"`) on the development machine, same script before and after (`VmRSS`): idle 13.4 MiB without the UI, 16.6 MiB with it; 27.7 MiB after 400 list and board renders over 50 issues (the baseline binary, which has no UI, reached 21.1 MiB from the fixture creates alone); 31.0 MiB after 30 renders of an issue with a 50 KB description and 30 comments of 19 KB (about 5 ms a page). Binary 12.4 MB to 15.1 MB: most of it is `html/template`, whose use of reflection stops the linker from dropping unused methods (the admin UI pays the same). `GOGC=25` and `GOMEMLIMIT=20MiB` did not help (the memory is live heap and SQLite page cache), so the module now declares `ram_mb: 28` and `ram_limit_mb: 64` (module.yaml and chart) instead of 20 and 48. The render buffer is pooled. Sessions are a bounded map (5000 entries). Not measured on a cluster |
| III. Test-first | PASS | The route contract, CSRF, scope, session and XSS tests and the markdown tests were written and run red (build failure, then failing assertions) before the handlers and templates existed; golden files are generated from the finished pages and then reviewed |
| IV. Stable module contracts | PASS | The `work` capability, the API and its OpenAPI document are untouched; `/ui/` is an additional, documented surface. The module manifest is unchanged |
| V. Secure and private by default | PASS | Scope and permission per route (404 for invisible), CSRF with `Sec-Fetch-Site` and `Origin` checks, session fixation, `HttpOnly` `SameSite=Strict` cookies, failure-only throttling, strict CSP, no secret echo, escaped output with a safe markdown subset, bounded work with deadlines. Residual risks in research.md R8 |
| VI. Open-source hygiene | PASS | Apache-2.0, no new dependency, DCO sign-off |
| VII. Simplicity and measured claims | PASS | No JavaScript framework; one page per concern; claims about accessibility are limited to what the static checklist and the contrast test prove, plus a stated manual pass still to do |

## Project Structure

```text
specs/023-work-ui/        spec, plan, research, data-model, tasks, contracts/ui-routes.md
core/work/ui.go           routes, middleware (session, CSRF, headers), error mapping
core/work/ui_auth.go      login, logout
core/work/ui_session.go   in-memory session table
core/work/ui_pages.go     project list, backlog and filters, create, dashboard, epics
core/work/ui_issue.go     issue page and its actions
core/work/ui_board.go     board, sprint planning
core/work/ui_markdown.go  safe text renderer and URL gate
core/work/ui_messages.go  message catalogue and template functions
core/work/webui/          templates and static files (embedded)
core/work/testdata/ui-golden/   golden pages
docs/work-ui.md           user and operator documentation
dev/compose/smoke.sh      UI sign-in check
```

## Complexity Tracking

| Choice | Why | Simpler option rejected because |
|---|---|---|
| Same package as the API (`core/work`) with `ui_` files | The UI needs the store, principal and limiter; a separate package would need exports or an import cycle through `app.go` | A `core/work/ui` package cannot be mounted by `NewApp` without a cycle |
| In-memory sessions holding the token | Immediate revocation and no token at rest | Signed cookies carrying the token (token in the browser) or a database table (token or its hash at rest, and no way to re-authenticate from a hash) |
