---

description: "Task list for Epic E01 — Project Foundation"
---

# Tasks: Project Foundation (Epic E01)

**Input**: Design documents from `/specs/001-project-foundation/`

**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/cli.md, quickstart.md

**Tests**: Requested (Constitution III, Test-First). In every story, test tasks are written and
observed failing **before** their implementation tasks.

**Organization**: Grouped by user story. Field constraints are cited from
[data-model.md](data-model.md) as `Entity.field` instead of being restated.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: parallelizable (different files, no dependency on an incomplete task)
- **[Story]**: US1–US4 map to the user stories in spec.md

## Phase 1: Setup

- [x] T001 Create `go.mod` (module `github.com/dodwmd/companyinacan`, Go 1.27), `.gitignore`, `.golangci.yml` (govet, staticcheck, errcheck, revive, gofmt, goimports) at the repo root
- [x] T002 [P] Create the directory skeleton from plan.md with a one-paragraph `README.md` in each of `core/`, `modules/`, `schemas/`, `deploy/`, `dev/`, `docs/` (keep `docs/epics/`), `examples/`, `templates/`, `scripts/`, `test/contract/`
- [x] T003 [P] Add `Makefile` with targets `help` (default), `build`, `test`, `lint`, `licences` and stub targets `dev` and `up` per contracts/cli.md (stubs print the epic that provides them and exit 3)
- [x] T004 Add dependencies with `go get`: cobra v1.10.2, `go.yaml.in/yaml/v3` v3.0.6, `santhosh-tekuri/jsonschema/v6` v6.0.3, `github/go-spdx/v2` v2.8.0; run `go mod tidy`

---

## Phase 2: Foundational (blocks all user stories)

- [x] T005 Define the shared `Problem` type (file, line, column, path, rule, severity, message) with text and JSON renderers in `core/validate/problem.go` (see data-model.md "Problem")
- [x] T006 [P] Write failing tests for `Problem` rendering (`file:line:col: path: message [rule]` and JSON) in `core/validate/problem_test.go`, then make them pass
- [x] T007 Implement the YAML loader that decodes to `yaml.Node` and maps JSON-pointer paths to line/column, with a parse-error `Problem` (rule `parse.yaml`) for empty/invalid YAML, in `core/config/loader.go`; tests first in `core/config/loader_test.go`
- [x] T008 Implement the exit-code constants (0, 1, 2, 3) and the root cobra command with `--output text|json` and `--no-color` in `core/cli/root.go` and `cmd/can/main.go`; tests first in `core/cli/root_test.go` using in-process execution with captured stdout/stderr/exit code

**Checkpoint**: `go build ./...` and `go test ./...` pass; user stories can start.

---

## Phase 3: User Story 1 — Validated company and module descriptions (Priority: P1) 🎯 MVP

**Goal**: `company.yaml` and `module.yaml` are validated against versioned schemas plus semantic
rules, with field-level messages and complete reporting.

**Independent Test**: `go test ./test/contract/...` — every valid fixture passes, every invalid
fixture produces exactly the problems in its `.expect.yaml`.

### Tests for US1 (write first, expect failure)

- [x] T009 [P] [US1] Create valid fixtures in `testdata/valid/` (`company-minimal.yaml`, `company-full.yaml`, `module-inhouse.yaml`, `module-oss-wrapper.yaml`, `module-or-licence-selected.yaml`)
- [x] T010 [P] [US1] Create invalid fixtures with sidecar `*.expect.yaml` in `testdata/invalid/` for: missing `ModuleManifest.provider.licence`, missing `ModuleManifest.resources.ram_mb`, `ModuleManifest.resources.ram_limit_mb` below `ram_mb`, unknown field, wrong `schema` version (`CompanyDescription.schema`, `ModuleManifest.schema`), malformed `ModuleManifest.subscribes` event name without `.vN`, malformed `ModuleManifest.permissions` name, `ModuleManifest.roles` referencing an undeclared permission, `ModuleManifest.data_classes.retention` missing, `data_classes.sensitivity: sensitive` on an `oss-wrapper` module, duplicate module subdomain, exclusive-capability conflict without `CompanyDescription.primary`, `primary` naming a disabled module, empty file, bad YAML, secret-looking value in `CompanyDescription.backup`
- [x] T011 [US1] Write the fixture-driven contract test runner in `test/contract/fixtures_test.go` (asserts the set of reported problems equals the sidecar expectation) and a test that validates `examples/` and `modules/_example/` (edge case: examples must not drift); confirm all fail before implementation

### Implementation for US1

- [x] T012 [P] [US1] Write `schemas/company.v1.schema.json` (draft 2020-12, `unevaluatedProperties: false`) implementing the constraints of `data-model.md` `CompanyDescription.schema`, `CompanyDescription.name`, `CompanyDescription.domain`, `CompanyDescription.provider`, `CompanyDescription.modules`, `ModuleEntry.enabled`, `ModuleEntry.settings`, `CompanyDescription.primary`, `CompanyDescription.site`, `CompanyDescription.backup`, `CompanyDescription.email`
- [x] T013 [P] [US1] Write `schemas/module.v1.schema.json` (draft 2020-12, `unevaluatedProperties: false`) implementing the constraints of `data-model.md` `ModuleManifest.schema`, `ModuleManifest.id`, `ModuleManifest.name`, `ModuleManifest.description`, `ModuleManifest.category`, `ModuleManifest.version`, `ModuleManifest.provider`, `ModuleManifest.runtime`, `ModuleManifest.requires`, `ModuleManifest.resources`, `ModuleManifest.provides`, `ModuleManifest.consumes`, `ModuleManifest.publishes`, `ModuleManifest.subscribes`, `ModuleManifest.permissions`, `ModuleManifest.roles`, `ModuleManifest.approvals`, `ModuleManifest.data_classes`, `ModuleManifest.settings_schema`, `ModuleManifest.hooks`, `ModuleManifest.healthcheck`
- [x] T014 [P] [US1] Write `schemas/names.yaml` (known capability names with `aggregate` flags, reserved platform events) and `schemas/embed.go` exposing the embedded schemas and names (research R6)
- [x] T015 [US1] Implement schema compilation and validation with error-to-`Problem` mapping (using the node line map from T007) in `core/schema/validate.go`; tests in `core/schema/validate_test.go`
- [x] T016 [P] [US1] Implement typed structs and loaders for company and module files in `core/config/company.go` and `core/config/module.go`
- [x] T017 [US1] Implement module-level semantic rules in `core/validate/module.go`: `ModuleManifest.resources.ram_limit_mb >= ram_mb` (default 1.5x when omitted), `ModuleManifest.roles` ⊆ `permissions`, event suffix `.vN`, `ModuleManifest.data_classes.sensitivity` sensitive ⇒ `provider.type` inhouse (Constitution V), unknown capability names as warnings; tests in `core/validate/module_test.go`
- [x] T018 [US1] Implement company-level rules in `core/validate/company.go`: unique module ids and subdomains, exclusive-capability conflicts vs `CompanyDescription.primary`, `primary` references enabled providers, secret-looking key warning for `CompanyDescription.backup` and `CompanyDescription.email`; tests in `core/validate/company_test.go`
- [x] T019 [US1] Create `examples/acme-co/company.yaml` (+ README) and `modules/_example/module.yaml` (+ README) with fake data and the reserved domain `acme.example`; run T011 and make the contract suite pass

**Checkpoint**: US1 is fully functional and testable on its own (MVP).

---

## Phase 4: User Story 2 — `can` CLI and `make` targets (Priority: P1)

**Goal**: `init`, `doctor`, `validate`, `apply` (stub), `version` behave as in contracts/cli.md.

**Independent Test**: Quickstart sections 2 and 4.

### Tests for US2 (write first)

- [x] T020 [P] [US2] CLI tests for `can init` in `core/cli/init_test.go`: writes a company that passes `schema/validate`, refuses overwrite (exit 1, file untouched), non-interactive without flags exits 2 and does not hang, writes only inside the target dir
- [x] T021 [P] [US2] CLI tests for `can doctor` in `core/doctor/doctor_test.go` and `core/cli/doctor_test.go` using an injectable PATH/probe: required tool missing ⇒ exit 1, optional missing ⇒ exit 0 with remedy, JSON output shape
- [x] T022 [P] [US2] CLI tests for `can validate`, `can version`, `can apply` in `core/cli/validate_test.go`, `version_test.go`, `apply_test.go` (multi-problem report in one run, `--modules-dir`, exit 3 with epic name for `apply`)

### Implementation for US2

- [x] T023 [US2] Implement `can init` in `core/cli/init.go` (interactive only on a TTY, flags `--name --domain --provider --force --non-interactive`; writes `company.yaml` and `.gitignore`)
- [x] T024 [P] [US2] Implement prerequisite probes in `core/doctor/doctor.go` (required `go`, `git`, `make`; optional `docker`/`podman`, `k3d`, `kubectl`, `helm`) and `can doctor` in `core/cli/doctor.go`
- [x] T025 [US2] Implement `can validate [dir] --modules-dir` in `core/cli/validate.go`, resolving manifests per research R15 and reporting module-missing problems
- [x] T026 [P] [US2] Implement `can version` (ldflags-injected version/commit/date) in `core/cli/version.go` and the `can apply` stub in `core/cli/apply.go`
- [x] T027 [US2] Verify `make test`, `make dev` (exit 3) and `make up` (exit 3) behave per contracts/cli.md; add a Makefile test in `test/contract/make_test.go` that runs the stub targets

**Checkpoint**: US1 + US2 work together; quickstart sections 1–4 pass.

---

## Phase 5: User Story 3 — Licence compliance (Priority: P2)

**Goal**: Fail on missing/unaccepted upstream licences; generate deterministic `THIRD_PARTY_LICENSES`.

**Independent Test**: `go test ./core/licence/...` and quickstart section 5.

### Tests for US3 (write first)

- [x] T028 [P] [US3] Table tests in `core/licence/licence_test.go` for: accepted id, missing licence, unknown id, `A OR B` with and without `ModuleManifest.provider.licence_selected`, `A AND B`, source-available with and without `licence_class`, in-house skip, empty modules dir
- [x] T029 [P] [US3] Golden-file test for the generated notice (byte-identical on two runs, sorted, LF, no dates) in `core/licence/notice_test.go` with `testdata/licence/*.golden`

### Implementation for US3

- [x] T030 [P] [US3] Create `schemas/accepted-licences.yaml` (SPDX ids with notes: Apache-2.0, MIT, BSD-2/3-Clause, ISC, MPL-2.0, GPL-2.0/3.0 (-only/-or-later), AGPL-3.0 (-only/-or-later), LGPL-2.1/3.0, Unlicense, 0BSD)
- [x] T031 [US3] Implement SPDX evaluation per research R7 in `core/licence/licence.go` using `go-spdx` and the accepted list, with rule ids `licence.missing`, `licence.not-accepted`, `licence.expression-ambiguous`
- [x] T032 [US3] Implement notice generation and `--check` mode in `core/licence/notice.go` and the `can licences` command (`--write`, `--check`, `--modules-dir`) in `core/cli/licences.go`; generate the initial `THIRD_PARTY_LICENSES` (header only plus any example third-party module) and wire `make licences`

**Checkpoint**: US1–US3 independently functional.

---

## Phase 6: User Story 4 — CI and contributor hygiene (Priority: P2)

**Goal**: Named CI checks and complete contributor documentation.

**Independent Test**: Quickstart sections 6 and 7.

- [x] T033 [P] [US4] Write `scripts/check-dco.sh <base> <head>` (walk `git rev-list --no-merges`, require a `Signed-off-by:` trailer matching the author name or email, print the `git commit -s` / `git rebase --signoff` remedy) and a shell test with a temporary repo in `scripts/check-dco_test.sh`
- [x] T034 [P] [US4] Resolve current commit SHAs of `actions/checkout` and `actions/setup-go` with `gh api` and record them (with version comments) for T035
- [x] T035 [US4] Write `.github/workflows/ci.yml` with `permissions: contents: read`, triggers on pull_request and push to `main`, and exactly the jobs `lint`, `unit`, `contract`, `licences`, `dco`, actions pinned by the SHAs from T034
- [x] T036 [P] [US4] Write `CONTRIBUTING.md` (Spec Kit workflow and epics, DCO sign-off, local checks, branch and PR rules, where specs live), `SECURITY.md` (private reporting via GitHub security advisories, response expectations), `CODE_OF_CONDUCT.md` (Contributor Covenant 2.1 text, fetched from the canonical source)
- [x] T037 [P] [US4] Write `.github/ISSUE_TEMPLATE/{bug.yml,feature.yml,epic.yml,config.yml}` and `.github/pull_request_template.md` with a Constitution I–VII compliance checklist and a DCO reminder
- [x] T043 [P] [US4] ~~Run `unit` and `contract` on a Linux and a macOS runner~~ Dropped: macOS is excluded for now (no build hosts); CI runs on Linux only (FR-016 amended)
- [x] T038 [US4] Update `README.md` (status, how to build and run `make test`, link to CONTRIBUTING and `docs/epics/`) and mark the contributions note accordingly

---

## Phase 7: Polish & Cross-Cutting

- [x] T039 [P] Time `make test` on a clean checkout and record the result in `specs/001-project-foundation/measurements.md` (SC-001, Constitution VII); fix anything over 2 minutes
- [x] T040 [P] Scan the diff for secrets, real names/emails, non-reserved domains (FR-015) and confirm examples use only fake data
- [x] T044 [P] Run `make test` with networking disabled (`unshare -rn` or equivalent) after dependencies are cached and record the result in `specs/001-project-foundation/measurements.md` (FR-016, spec edge case: doctor offline)
- [x] T045 Trial the CI attribution (SC-004) on a throwaway branch/PR: missing sign-off, failing test, bad fixture, licence violation each fail only their named job; needs maintainer approval to push, record the result in `measurements.md`
- [x] T041 Run the whole `quickstart.md` end to end and tick every scenario; update `docs/epics/README.md` status for E01
- [x] T042 Constitution compliance pass over the final tree (I–VII) and record it at the end of `plan.md`

---

## Dependencies & Execution Order

- Phase 1 → Phase 2 → user stories. US1 and US2 are both P1 but US2's `validate` and `init` depend
  on US1's schema package (T015–T016), so run US1 first; US3 depends on T016 (loaders) and on
  US1's `modules/_example`; US4 is independent of US1–US3 apart from CI invoking their tests.
- Within a story: tests (fail) → schemas/models → services → CLI wiring.

```text
Phase 1 ─► Phase 2 ─► US1 ─┬─► US2 ─┐
                           ├─► US3 ─┼─► Phase 7
                           └─► US4 ─┘   (US4 can start after Phase 2)
```

## Parallel Examples

- Phase 1: T002 and T003 in parallel after T001.
- US1: T009, T010 together; then T012, T013, T014, T016 together.
- US2: T020, T021, T022 together; T024 and T026 together.
- US3: T028, T029, T030 together.
- US4: T033, T034, T036, T037 together.

## Implementation Strategy

1. **MVP = Phase 1 + 2 + US1** (the contract every later epic needs): stop and validate with the
   contract suite.
2. Add US2 (usable CLI), then US3 and US4 in either order.
3. Finish with Phase 7 measurements and the constitution pass before opening the PR.

## Implementation notes (deviations from the plan)

- The shared `Problem` type lives in `core/problem` (not `core/validate`) to avoid an import cycle between `config` and `validate` (T005, T006).
- `go-spdx` was dropped: a ~150-line SPDX expression evaluator in `core/licence` covers AND/OR/WITH/parentheses with no extra dependency (Constitution VII). `golang.org/x/term` was added for correct terminal detection (`/dev/null` is a character device).
- Make cannot propagate a recipe's exit status (it reports 2), so `can dev` and `can up` are real stub commands exiting 3, and `make dev|up` call them (spec FR-009 updated).
- `company.yaml` secrets are prevented by schema (`secret_ref` pattern, unknown keys rejected) instead of a heuristic warning (T018).
- T045 (CI attribution trial) ran on four throwaway PRs; results in `measurements.md`. macOS was dropped from the CI matrix (T043) because there are no macOS build hosts.

**Totals**: 45 tasks — Setup 4, Foundational 4, US1 11, US2 8, US3 5, US4 7, Polish 6.
