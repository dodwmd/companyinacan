---

description: "Task list for Epic E05 — Forms gateway and leads capability"
---

# Tasks: Forms Gateway and `leads` Capability (Epic E05)

**Input**: Design documents from `/specs/003-forms-gateway/`

**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/

**Tests**: Required (Constitution III). In every group the test task is written and observed
failing (compile error or red) before its implementation task.

## Phase 1: Setup

- [x] T001 Add `modernc.org/sqlite` and `go mod tidy`; confirm `can licences --check` is unaffected (research R1)
- [x] T002 Create `core/forms/`, `cmd/can-forms/`, `modules/forms/`, `schemas/events/`, `deploy/forms/`

## Phase 2: Foundations (US1, US2)

- [x] T003 [P] Tests then `core/forms/config.go`: strict YAML, defaults, all-problems validation (`config_test.go`)
- [x] T004 [P] Tests then `core/forms/fields.go`: body parsing (urlencoded, JSON, limits) and field validation (`fields_test.go`)
- [x] T005 [P] Tests then `core/forms/spam.go`: timing token, bounded rate limiter, captcha verifier (`spam_test.go`)
- [x] T006 [P] Tests then `core/forms/csv.go`: CSV writer with formula-injection protection (`csv_test.go`)
- [x] T007 Tests then `core/forms/store.go` and `migrations/`: leads, indexes, retention purge, status update, deliveries, idempotent migrations (`store_test.go`)

## Phase 3: User Story 1 and 2 — submit and spam control

- [x] T008 Test harness with fake clock, mailer, captcha (`harness_test.go`)
- [x] T009 Tests then `core/forms/public.go`: routing with prefix stripping, origin/CORS, pipeline, responses (`public_test.go`)
- [x] T010 Tests then `core/forms/notify.go`: email builder, `HTTPMailer`, dispatcher with bounded queue and drain on shutdown (`notify_test.go`)

## Phase 4: User Story 3 — leads capability

- [x] T011 Tests then `core/forms/auth.go` and `api.go`: bearer auth, permissions, list/get/patch/export (`api_test.go`)

## Phase 5: User Story 4 — event and webhook

- [x] T012 Tests then `core/forms/event.go` and the webhook sender in `notify.go`: signing, retries, no redirects (`event_test.go`, `notify_test.go`)
- [x] T013 `schemas/events/lead.created.v1.json`, fixtures under `testdata/events/lead.created.v1/`, contract tests in `test/contract/forms_test.go`

## Phase 6: User Story 5 — run it

- [x] T014 Tests then `core/forms/app.go`, `cmd.go`, `cmd/can-forms/main.go`: secrets, wiring, graceful shutdown, `check-config`, `healthcheck` (`app_test.go`)
- [x] T015 `modules/forms/module.yaml`, `settings.schema.json`, `chart/values.yaml`, README; manifest contract test
- [x] T016 `deploy/forms/Dockerfile` (distroless, digest placeholders) and README
- [x] T017 Measure idle RSS of the static binary and record it (research R7)

## Phase 7: Polish

- [x] T018 `make test` green (lint, unit, contract, licences); race detector clean
- [x] T019 Update `docs/epics/README.md` status for E05
- [ ] T020 CI green on the pull request

## Known gaps (recorded, not hidden)

- Helm templates, Traefik `IngressRoute` priority and the `NetworkPolicy` for this module belong to
  E03/E06; `chart/values.yaml` fixes the contract only.
- Admin leads view is E12; the capability it needs is complete.
- Token introspection against can-core (E04) is not wired; `Authenticator` is the seam.
- Backup/restore hook scripts are E08; the manifest does not declare them yet.
- `email.send` request shape is provisional until E04 defines it (`HTTPMailer` is the only code to change).
