---

description: "Task list for Epic E14a: Work tracker, first slice"
---

# Tasks: Work Tracker, first slice (Epic E14a)

**Input**: Design documents from `/specs/011-work-tracker/`

**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/

**Tests**: Required (Constitution III). Pure-logic units (rank, query, workflow) had their tests
written and run before the code. The store, API, token, CLI and contract tests were written in the
same sessions as the code in small increments (see plan.md, Constitution Check III).

## Phase 1: Setup

- [x] T001 Create `core/work/`, `cmd/can-work/`, `modules/work/`, `deploy/work/`, `specs/011-work-tracker/`; no new dependency (modernc sqlite, yaml and cobra are already in go.mod)
- [x] T002 `core/work/migrations/001_work.sql`: projects, issues, labels, links, comments, activity, watchers, attachments, idempotency, tokens, audit, outbox

## Phase 2: Foundations (pure rules)

- [x] T003 [P] Tests then `rank.go`: fractional ranks, property test of random inserts (`rank_test.go`)
- [x] T004 [P] Tests then `query.go`: search language to parameterised SQL, escaping, rejects (`query_test.go`)
- [x] T005 [P] Tests then `workflow.go`: default workflow, validation, categories, SLA hints (`workflow_test.go`)
- [x] T006 `model.go`: types, limits, validation, `Opt[T]` for PATCH null versus absent

## Phase 3: User Stories 1 to 5: the store

- [x] T007 `store.go`, `issues.go`: projects, create (auto keys, hierarchy, labels, watchers), idempotency, bulk create with `$N` parents, update with versions, transition, bulk update, search (`store_test.go`)
- [x] T008 `extras.go`: comments, activity, links with cycle checks, watchers, attachment metadata (`store_test.go`)
- [x] T009 `sprints.go`: sprints, board data, rank moves (`store_test.go`)
- [x] T010 `retention.go`: purge by retention (`store_test.go`)

## Phase 4: User Story 6: API, tokens and security

- [x] T011 `auth.go`, `tokens.go`: Principal, `Authenticator`, static and stored tokens, scopes, project scope, expiry, revocation, last used
- [x] T012 `httputil.go`, `api.go`: routes, strict decoding, ETag and `If-Match`, rate limits, size limits, error mapping (`api_test.go`)
- [x] T013 Security tests: IDOR across projects (store and HTTP), scope enforcement per route, mass assignment, injection, body and rate limits, token lifecycle and creation rules, audit contents (`store_test.go`, `api_test.go`)

## Phase 5: User Story 7: events

- [x] T014 `events.go`: outbox, signed dispatcher with backoff (`api_test.go`)
- [x] T015 `schemas/events/work.*.v1.json` (+ copies under `modules/work/events/`), fixtures under `testdata/events/`, contract tests
- [x] T016 `schemas/capabilities/work.v1.json`, `projects.v1.json`, fixtures, `schemas/names.yaml` (`projects`), SPEC.md capability row

## Phase 6: Run it

- [x] T017 `config.go`, `app.go`, `cmd.go`, `cmd/can-work/main.go`: config, secrets, wiring, graceful shutdown, `check-config`, `healthcheck`
- [x] T018 `modules/work/module.yaml` (in-house, `experimental`), `settings.schema.json`, `chart/values.yaml`, README; manifest contract test
- [x] T019 `deploy/work/Dockerfile` (distroless, digest placeholders) and README
- [x] T020 Measure idle RSS of the static binary and record it (research R7): 13.6 MiB idle, 20.4 MiB after load
- [x] T021 `contracts/openapi.yaml` (3.1) and the drift test against the router and capability verbs

## Phase 7: Clients and docs

- [x] T022 `client.go` and `core/cli/work.go`: `can work create|bulk-create|get|list|update|transition|comment` with token from the environment, idempotent creates, safe retries (`core/cli/work_test.go`)
- [x] T023 `docs/work-tracker.md` with curl examples (run against a real process before committing), docs index, epics README row

## Phase 8: Polish

- [x] T024 `make test` green (lint, unit, contract, licences); `go test -race` clean
- [x] T025 CI green on the pull request
- [x] T026 QA round 1 fixes: parenthesised search clauses and an always-ANDed scope with a fuzz test (blocker); auth-failure throttle no longer rejects valid tokens; read-only connection pool, query deadline, free-text and offset caps; outbox with 7-day capped-backoff retries, dead-letter list and replay; walk of every OpenAPI operation for 401 and 403; `is:blocked` scoped; sprint completion records activity and events; rank keeps ETags, is logged and respaced; archived link targets refused; purge keeps parents of surviving children; workflow dead-end check; watchers self-only unless admin; `bootstrap-admin` reserved

## Known gaps (recorded, not hidden)

- **Not run on a cluster.** The image has not been built (digests are placeholders), the Helm
  templates, `IngressRoute`/`NetworkPolicy` and the can-core enable path belong to the module
  deployment work (E03/E06); `chart/values.yaml` fixes the contract only. Module status is
  `experimental` until a cluster check passes.
- **No web UI or board UI.** The admin UI (E12) and a work UI come later; the board endpoint returns the
  data they need.
- **Tokens are minted by an admin.** Personal tokens that people create for themselves need SSO
  (E11). can-core token introspection is the `Authenticator` seam; it is not wired.
- **Authorisation is token-scoped, not role- or reporting-line-aware.** SPEC.md's manager visibility
  into reports' work needs `directory` (E13) and the RBAC engine (E11). Assignees and reporters are
  free-form identifiers (an email or directory id), not validated against a party model.
- **No attachment storage.** Metadata only, until `files` (E13) exists.
- **Event titles.** Events carry issue titles (not descriptions or comments); a receiver must be trusted
  with them. Omitting them would need a schema change and is a decision for the event-bus work.
- **Events: no `issue.priority_changed` event and no manager notifications.** `work.issue.updated.v1`
  carries `changes.priority`; `notify` integration and ageing views come with the notification
  and reporting work. Webhooks go to one URL; the can-core bus replaces the dispatcher later.
- **Search is AND-only** (no OR or parentheses), no saved filters, no components, no full-text index
  (substring match), offset paging (can shift under concurrent creates).
- **No time tracking, SCM links, MCP server, per-person workload views, Plane wrapper, `performance` or
  timesheets** (the rest of E14).
- **Event ordering** is not guaranteed across retries (documented; consumers use `version` and `time`).
- **Single SQLite writer.** Fine for one company; backup and restore arrive with E08 (SQLite online
  copy), and the retention purge is the only background job besides webhook delivery.
- **Performance was measured only for idle and a short load** on a development machine, not on a small
  server and not with tens of thousands of issues.
