---

description: "Task list for Epic E03 — can-core MVP"
---

# Tasks: can-core MVP (Epic E03)

**Input**: [spec.md](spec.md), [plan.md](plan.md), [research.md](research.md), [data-model.md](data-model.md), [contracts/](contracts/)

**Tests**: Test-first (Constitution III). In every group the test task precedes its implementation task.

**Part 1** merged as `epic/E03a-can-core`. **Part 2** is `epic/E03b-k8s-integration`.

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

## Part 2 (`epic/E03b-k8s-integration`)

Evidence for the cluster tasks is the `dev-cluster` workflow (a real k3d on the GitHub runner); the PR records which
steps ran. Nothing below was run against a cluster locally.

### Installer (US7)
- [x] T030a Research: measure the Helm SDK against client-go and a hand-rolled client; decide (research R16)
- [x] T030b Test then `core/server/kube`: SSA, create, get, list, delete, merge-patch over `net/http`, a fake API
- [x] T030c Test then `core/server/installer/chart.go`: Helm-compatible template subset (`include`, `toYaml`, `default`, `required`, ...), `values.tmpl.yaml`
- [x] T030d Test then `core/server/installer`: namespace `mod-<id>` + NetworkPolicies + quota + RoleBinding from `deploy/templates`; workload hardening (own SA, `automountServiceAccountToken: false`, projected `can` token, Secrets as env); chart guard rails; rollout wait, rollback to the previous release or removal of a failed first install, prune (never PVCs or Secrets); disable scales to zero and deletes nothing
- [x] T030e Runtime wiring: `CAN_INSTALLER=kubernetes`, reconcile loop over the mounted `company.yaml` (`core/server/runtime/sync.go`)
- [ ] T030f Upstream-chart support: the full Helm engine for charts that use `lookup`, `tpl`, subcharts or hooks (see R16)

### Databases (US7)
- [x] T031a Test then `core/server/dbprov`: `Provisioner` interface, fake, `Unconfigured`
- [x] T031b Postgres implementation (role, database, `REVOKE ... FROM PUBLIC`, password re-assert) and the `can-db` Secret; password reused across enables
- [x] T031c k3d test against a throw-away Postgres pod (`TestPostgresProvisioner`)
- [ ] T031d MariaDB implementation

### Identity (US2)
- [x] T035a `tokenreview.Cached`: positive cache (capped by the token's `exp`), negative cache, global and per-source rate limit, bounded; module API answers 429 (R15)
- [x] T035b Real TokenReview proven by the stub module's call with a projected token, and a wrong-audience token refused (`TestCanCoreLifecycle`)

### Admission (closes the RBAC residual risk)
- [x] T038a `deploy/base/admission-policy.yaml`: ValidatingAdmissionPolicy + bindings for can-core's RoleBindings and namespaces
- [x] T038b Offline contract test (`test/contract/cancore_manifests_test.go`) and k3d tests (`TestAdmissionPolicy*`): a bind into `kube-system` is denied by the policy, a bad namespace is denied, the canonical bind is allowed, administrators are unaffected

### Deployment and the epic's "done when" (US6, US7)
- [x] T033a `can apply` syncs `company.yaml` into ConfigMap `can-system/company` (kubectl, server-side apply); can-core reconciles it
- [x] T036a `deploy/can-core` (Deployment, Service, PVC; non-root, read-only rootfs, 48/96 MiB, probes, `CAN_TRUST_PROXY=true`) and `deploy/can-core/Dockerfile`
- [x] T035c `TestCanCoreLifecycle`: can-core built from this checkout and imported into k3d; `can apply` -> namespace, policies, quota, workload -> injected config -> module API call with the real projected token -> settings upgrade -> disable (PVC and Secrets kept) -> re-enable
- [ ] T033b `can apply --dry-run` against cluster state (needs a read path into can-core; today `--dry-run` plans against a local state file)
- [ ] T036b CI footprint check on can-core's cluster RSS (the workflow prints the node-side RSS; it does not fail on it yet)

### Still open
- [ ] T032 `email.send` and SMTP credential injection
- [ ] T034 Admin enable/disable endpoints; minimal admin UI
- [ ] T037 Uninstall with keep/purge (the installer has no code path that deletes a PVC, Secret or namespace)
