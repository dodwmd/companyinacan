---

description: "Task list for Epic E12b: module management and onboarding wizard"
---

# Tasks: Module management and onboarding wizard (Epic E12b)

**Input**: [spec.md](spec.md), [plan.md](plan.md), [research.md](research.md), [data-model.md](data-model.md), [contracts/](contracts/)

**Tests**: Constitution III. `[V]` marks work that needs a real cluster, browser or provider and is **not** done.

## Phase 1: Foundations

- [x] T001 Tests (red) then `validate.CompanyBytes`, `reconcile.LoadDesiredBytes`, `config.Module.SettingsSchema`
- [x] T002 Migration `0003_module_management.sql`, `store.DeleteDisabledModule`
- [x] T003 `rbac.PermSetup`, `rbac.PermPurge` (owner only by default)

## Phase 2: `modmgr` (tests first, seen red)

- [x] T004 Catalogue loader: bounded, skips `_`/`.`/symlinks, invalid manifests listed, schema path confined (`catalogue_test.go`)
- [x] T005 Footprint verdicts, requires/provides analysis, planned capabilities, exclusive conflicts
- [x] T006 Settings renderer and validator: safe subset, preserved unsupported keys, no external `$ref`, secret-ref shape (`settings_test.go`)
- [x] T007 Patch allow-list, comment-preserving apply, JSON round trip, diff, `FileStore` and `KubeStore` optimistic concurrency, concurrent writers (`company_test.go`)
- [x] T008 Propose / preview / apply: acknowledgements, not-deployable, blockers, stale, expired, single flight, audit before write (write refused if audit fails), idempotent apply, sanitised errors, bounded proposals (`service_test.go`)
- [x] T009 Purge: disabled only, typed confirmation, audited, fail closed without an executor
- [x] T010 Wizard: seeded, resumable, idempotent, templates, relay Secret and no echo, DNS plan, hardening from audit, status aggregation (`wizard_test.go`)

## Phase 3: Admin UI

- [x] T011 Contract: `ui-routes.md` extended (test red until routes documented); message catalogue test red until strings added
- [x] T012 Catalogue, module page with generated form, change preview, run progress (meta refresh), status, company, purge pages and templates
- [x] T013 Wizard handlers and `setup.html` (seven steps), first-login redirect for the owner only
- [x] T014 Tests: every route gated (enumerated table fails for an ungated route), CSRF and cross-site on every new POST, flows, opt-ins, stale, hostile manifest, schema and error strings, relay password never echoed or audited, purge gates
- [x] T015 Golden HTML for 37 page states and the static accessibility checklist for all of them

## Phase 4: API, runtime, docs

- [x] T016 `GET /api/v1/catalogue`, `GET /api/v1/status` with tests; OpenAPI extended; contract test green
- [x] T017 Runtime wiring: `modmgr.Service`, `KubeStore`/`FileStore`, `Runner` shared with the loop, store-first reconcile (test), recovery of interrupted runs, `CAN_PUBLIC_IPS`
- [x] T018 `docs/admin-ui.md`, `docs/epics/README.md`, this spec

## Not done

- [V] T019 Run against a real k3s/k3d cluster: apply through the real installer, ConfigMap resourceVersion conflict behaviour, mounted-file refresh timing
- [V] T020 A `Purger` (namespace, PVC, Secret, database removal) and its cluster verification
- [V] T021 `IngressStatus` from Traefik/ACME state, per-PVC disk usage, pod `ReadyProbe`, real sslip.io probe
- [V] T022 Installer copy of the relay Secret into module namespaces as `can-smtp` (email policy)
- [V] T023 Manual keyboard and screen-reader pass, screenshots
