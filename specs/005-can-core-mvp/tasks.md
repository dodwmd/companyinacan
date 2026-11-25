---

description: "Task list for Epic E03 — can-core MVP"
---

# Tasks: can-core MVP (Epic E03)

**Input**: [spec.md](spec.md), [plan.md](plan.md), [research.md](research.md), [data-model.md](data-model.md), [contracts/](contracts/)

**Tests**: Test-first (Constitution III). In every group the test task precedes its implementation task.

**Part 1** is this PR (`epic/E03a-can-core`). **Part 2** is open.

## Part 1

### Setup
- [x] T001 Add `modernc.org/sqlite` and `golang.org/x/crypto`; justify in research.md R1, R2
- [x] T002 Spec artifacts: spec, plan, research, data-model, contracts, tasks

### State (US1-US6 foundation)
- [x] T003 Test: migrations apply once and keep data, admin uniqueness, TOTP replay guard, sessions, module/provider/subscription replace semantics, outbox due/backoff (`core/server/store/store_test.go`)
- [x] T004 `core/server/store`: open, migrations `0001_init.sql`, admin, sessions, login attempts, modules, providers, subscriptions, events, deliveries

### US4 Audit
- [x] T005 Test: chain links; modified, deleted, deleted+renumbered, reordered, rehashed, forged prev_hash all detected; truncation only with anchored head; concurrent appends; export (`audit_test.go`)
- [x] T006 `core/server/audit`

### US1 Admin auth
- [x] T007 Test: argon2id format/params/rehash; RFC 6238 vectors (SHA1, SHA256, SHA512); TOTP window and single use; uniform login failure; lockout per user and per IP and expiry; sessions, expiry, logout, CSRF, cookie attributes (`auth_test.go`)
- [x] T008 `core/server/auth` (password, totp, service)

### US2 Module auth
- [x] T009 Test: fake and HTTP TokenReview (audience, non-SA, API errors); token -> module mapping, disabled module (`tokenreview_test.go`, `modauth_test.go`)
- [x] T010 `core/server/tokenreview`, `core/server/modauth`

### US3 Registry
- [x] T011 Test: single, exclusive conflict and `primary`, aggregate, disabled, platform, re-register, manifest defaults, `Declares`
- [x] T012 `core/server/registry`

### US5 Events and webhooks
- [x] T013 Test: catalogue and name validation; fan-out to enabled matching subscribers only; persistence before delivery; in-process handlers and panic isolation; wake (`events_test.go`)
- [x] T014 `core/server/events`
- [x] T015 Test: sign/verify, replay window, timestamp swap, backoff, retry then success, dead after max, no unsigned send, no redirects, run loop (`webhook_test.go`)
- [x] T016 `core/server/webhook`

### US6 Reconcile
- [x] T017 Test: enable injects config then disable; idempotent; upgrade; removal disables; dependency order; dry run; failure isolation and retry; primaries; audit trail; LoadDesired; RecordOnly (`reconcile_test.go`)
- [x] T018 `core/server/reconcile` (`ModuleInstaller`, `FakeInstaller`, `RecordOnly`)
- [x] T019 Test then implement `can apply [--dry-run|--record-only]` (surgical change to `core/cli/apply.go`, `root.go`; existing not-implemented test updated)

### HTTP, runtime, CLI
- [x] T020 OpenAPI 3.1 contracts `contracts/admin-api.yaml`, `contracts/module-api.yaml`
- [x] T021 Test: route parity, operation ids, `$ref`s, webhook headers (`test/contract/cancore_api_test.go`)
- [x] T022 Tests then `core/server/httpx`, `adminapi`, `moduleapi` (anonymous rejection on every route, CSRF, cookie flags, no secret exposure, no credentials in logs, introspection, capability authorisation, publish authorisation)
- [x] T023 Tests then `core/server/runtime` (env config, two separate listeners, graceful shutdown, fail fast) and `core/server/corecli` + `cmd/can-core` (`serve`, `audit verify|head|export`, `admin bootstrap`, `version`)
- [x] T024 Stub-module lifecycle test, in-process (`core/server/lifecycle_test.go`)
- [x] T025 Measure idle RAM ([measurements.md](measurements.md)); `make build` also builds `bin/can-core`
- [x] T026 `make test` green; push; CI green (see measurements.md)

## Part 2 (open)

- [ ] T030 Helm-SDK `ModuleInstaller` (`helm upgrade --install --atomic`, values mapping from `values.tmpl.yaml`, namespaces, NetworkPolicies, Secrets incl. webhook HMAC, `automountServiceAccountToken: false`, projected `can` token volume)
- [ ] T031 Database creation and credential injection from `requires.database`
- [ ] T032 `email.send` (SMTP relay wrapper) and SMTP credential injection
- [ ] T033 In-cluster applied `company.yaml` ConfigMap; `can apply` against the cluster; drift warning
- [ ] T034 Admin enable/disable endpoints; minimal admin UI
- [ ] T035 k3d integration tests: real TokenReview, NetworkPolicy enforcement, Helm upgrade/rollback, stub module enable/disable (Constitution III)
- [ ] T036 can-core chart with declared `ram_mb: 48`, `ram_limit_mb: 96`; CI footprint check
- [ ] T037 Uninstall with keep/purge
