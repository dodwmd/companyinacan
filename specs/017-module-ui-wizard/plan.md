# Implementation Plan: Module management and onboarding wizard (Epic E12b)

**Branch**: `epic/E12b-module-ui-wizard` | **Date**: 2026-11-25 | **Spec**: [spec.md](spec.md)

## Summary

A new package `core/server/modmgr` holds the logic (catalogue, footprint, graph, settings forms,
`company.yaml` patches and stores, change preview and apply, wizard, status, purge) with no HTTP in it.
`core/server/adminui` renders it (new handlers, templates, messages); `core/server/adminapi` exposes two
read-only endpoints; `core/server/runtime` wires it and makes the periodic reconcile read the same store.
Design choices: [research.md](research.md); entities: [data-model.md](data-model.md); contracts:
[contracts/](contracts/).

## Technical Context

**Language/Version**: Go 1.27. **New dependencies**: none (`santhosh-tekuri/jsonschema/v6` and `yaml.v3` are already used).

**Storage**: SQLite migration `0003_module_management.sql` (4 tables); `company.yaml` itself lives in the
ConfigMap `can-system/company` (cluster) or `CAN_COMPANY_FILE` (development).

**Testing**: `go test -race`. `modmgr` (tests written first, seen red): catalogue, footprint, graph, settings
renderer, patches and diff, both stores, change flow, wizard, purge, status. `adminui`: route-gating
enumeration, CSRF on the new POSTs, flows, hostile data, wizard, golden HTML for 37 page states, accessibility
checklist for all of them. `adminapi`: gating and shape. `runtime`: store-first reconcile. Contract tests:
`TestRoutesMatchContract` (UI) and `test/contract` (OpenAPI).

**Constraints**: no cluster, docker or browser; no installs.

## Constitution Check

| Principle | Before research | After design |
|---|---|---|
| I. Spec-first | `SPEC.md` 6 says Phase 2 adds a browser wizard for templates, modules and DNS; this spec, contracts and `docs/admin-ui.md` land in the same PR. No contradiction of SPEC.md | Same. SPEC.md says the admin UI "sums declared RAM against the server and warns before over-commit": implemented with the baseline constants documented in research R3 |
| II. Footprint | Runs inside can-core; no new component. Catalogue read per request (bounded 200 manifests) | The wizard and catalogue add templates and a few KiB of code; assets stay under the budget (tested). No new resident memory beyond a mutex and a goroutine per apply. can-core idle RAM is measured by the E03 gate, not claimed here |
| III. Test-first | Tests for `modmgr` were written and run red (undefined symbols) before the implementation; route-contract and message-catalogue tests went red before the contract and messages were written | UI handler tests were written after the handlers for speed (honest note): the gating, CSRF and hostile-data tests exist and pass, but they were not red first |
| IV. Stable module contracts | Reads manifests and `settings.schema.json` only; no capability or event schema changes. Adds `settings_schema` to the Go manifest struct (already in the JSON Schema) | Same |
| V. Secure and private | RBAC on every route, CSRF, audit before and after, fail closed, bounded, escaped, secrets only into a Secret and never echoed or audited, purge behind a distinct permission, typed confirmation and password | Tested (see spec FR list). Residual risks in Known gaps |
| VI. Open-source hygiene | Apache-2.0, no new third-party code | Same |
| VII. Simplicity and measured claims | One service package, no JavaScript, polling by meta refresh. Footprint numbers are declared figures plus documented constants, labelled estimates | Same; nothing claimed about real servers |

**Result**: pass, no violations to justify.

## Project structure

```text
core/server/modmgr/        catalogue.go graph.go settings.go company.go service.go wizard.go status.go runner.go export.go disk_*.go
core/server/adminui/       modmgmt.go wizard.go messages_modmgmt.go templates/{catalogue,cmodule,change,run,status,company,purge,setup,partials}.html
core/server/adminapi/      modmgmt.go (GET /api/v1/catalogue, GET /api/v1/status)
core/server/runtime/       module-management wiring, store-first reconcile loop, CAN_PUBLIC_IPS
core/server/store/         migrations/0003_module_management.sql, DeleteDisabledModule
core/server/rbac/          PermSetup, PermPurge
core/validate, reconcile/  CompanyBytes, LoadDesiredBytes (in-memory variants)
specs/010-admin-ui/contracts/ui-routes.md   extended with the new routes
specs/005-can-core-mvp/contracts/admin-api.yaml  extended with catalogue and status
docs/admin-ui.md           pages, flows, permissions, configuration
```
