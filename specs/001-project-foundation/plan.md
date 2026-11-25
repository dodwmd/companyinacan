# Implementation Plan: Project Foundation (Epic E01)

**Branch**: `001-project-foundation` | **Date**: 2026-11-24 | **Spec**: [spec.md](spec.md)

**Input**: Feature specification from `/specs/001-project-foundation/spec.md`

## Summary

Deliver the buildable, tested skeleton: a single Go module with the `can` CLI (`init`, `doctor`,
`validate`, `apply` stub, `version`, `licences`), a thin Makefile, versioned JSON Schemas for
`company.yaml` and `module.yaml` with additional Go semantic checks and a fixture-driven contract
test suite, a licence checker that generates a deterministic `THIRD_PARTY_LICENSES`, GitHub
Actions CI with five separately named jobs including a DCO check, and the open-source hygiene
files. Approach and dependency choices are in [research.md](research.md); entities in
[data-model.md](data-model.md); CLI/CI contract in [contracts/cli.md](contracts/cli.md).

## Technical Context

**Language/Version**: Go 1.27 (module `github.com/dodwmd/companyinacan`)

**Primary Dependencies**: `spf13/cobra` v1.10.2; `go.yaml.in/yaml/v3` v3.0.6;
`santhosh-tekuri/jsonschema/v6` v6.0.3; `golang.org/x/term`; dev-only
`golangci-lint` v2.15.0 (run via `go run`, not a module dependency)

**Storage**: Files only (YAML, JSON Schema, generated Markdown); no database

**Testing**: `go test` (table-driven, fixtures under `testdata/`, in-process CLI tests, golden
file for `THIRD_PARTY_LICENSES`); no network or container runtime

**Target Platform**: Linux developer machines and GitHub-hosted Linux runners (macOS is not supported for now: no build hosts)

**Project Type**: CLI plus libraries (single Go module)

**Performance Goals**: `make test` completes in < 2 minutes on a laptop (SC-001);
`can validate` on the example repo in < 1 s

**Constraints**: No network access at runtime for any command; deterministic generated output;
unknown fields rejected; every validation problem reported in one run

**Scale/Scope**: Two schemas (~40 and ~60 fields), ~6 commands, roughly 2–3 k lines of Go plus
fixtures; one example company repo and one example module manifest

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-checked after Phase 1 design.*

| Principle | Status | Notes |
|---|---|---|
| I. Spec-first | PASS | Behaviour traces to `SPEC.md` §3.3, §4.1, §6.4, §12; one spec clause added (FR-003: sensitive classes require an in-house module) to encode Constitution V, noted in research R5 |
| II. Footprint | PASS (n/a at runtime) | Schema carries `ram_mb`/`ram_limit_mb`; the CLI has no resident footprint. Footprint CI harness belongs to E00 |
| III. Test-first | PASS | Tasks order tests before implementation; contract tests for both schemas; no k3d needed in this epic |
| IV. Module contracts | PASS | Manifest encodes capabilities, events, permissions, approvals, data classes, resources, licence |
| V. Secure/private | PASS | No secrets accepted in `company.yaml` (only `secret_ref`); examples use fake data and reserved domains; sensitive classes restricted to in-house modules |
| VI. OSS hygiene | PASS | Licence check, `THIRD_PARTY_LICENSES`, DCO, hygiene files are this epic's scope |
| VII. Simplicity/measured | PASS | Cobra + two small libs; no framework beyond need; timing claims (SC-001) measured in CI logs |

Post-design re-check: no violations; Complexity Tracking is empty.

## Project Structure

### Documentation (this feature)

```text
specs/001-project-foundation/
├── plan.md
├── research.md
├── data-model.md
├── quickstart.md
├── contracts/cli.md
├── checklists/requirements.md
└── tasks.md            # created by /speckit-tasks
```

### Source Code (repository root)

```text
go.mod  go.sum  Makefile  .golangci.yml  .gitignore
THIRD_PARTY_LICENSES                    # generated
CONTRIBUTING.md  SECURITY.md  CODE_OF_CONDUCT.md
.github/
├── workflows/ci.yml
├── ISSUE_TEMPLATE/{bug.yml,feature.yml,epic.yml,config.yml}
└── pull_request_template.md
cmd/can/main.go
core/
├── README.md
├── cli/            # cobra commands: root, init, validate, doctor, apply, version, licences
├── config/         # typed structs + loaders (yaml.Node aware) for company and module
├── schema/         # embedded schemas, compile + validate, error -> Problem mapping
├── problem/        # shared Problem type (avoids a config<->validate import cycle)
├── validate/       # semantic rules (R5), company-level cross-module checks
├── licence/        # accepted list, SPDX evaluation, notice generation, --check
└── doctor/         # prerequisite probes
schemas/
├── README.md  company.v1.schema.json  module.v1.schema.json
├── accepted-licences.yaml  names.yaml  embed.go
modules/            # README.md and _example/{module.yaml,README.md}
examples/acme-co/   # company.yaml, README.md
deploy/ dev/ docs/ templates/            # README.md placeholders (docs/epics exists)
scripts/check-dco.sh
test/contract/      # schema + example contract tests
testdata/{valid,invalid}/                # fixtures + *.expect.yaml
```

**Structure Decision**: Single Go module, library packages under `core/`, thin `cmd/can`. The
`schemas/` directory is a Go package (`embed.go`) so the same files are both the published
contract and the embedded validation data.

## Complexity Tracking

No constitution violations to justify.

## Constitution Compliance Pass (end of implementation, 2026-11-25)

| Principle | Result | Evidence |
|---|---|---|
| I. Spec-first | PASS | Spec amended where implementation learned something (FR-003 sensitive classes, FR-006 `licences`, FR-009 make exit status); tasks record deviations |
| II. Footprint | PASS / n/a | No resident component; manifest fields `ram_mb`/`ram_limit_mb` validated (limit >= request) |
| III. Test-first | PASS | Tests written and observed failing before each implementation step (compile failures / red runs); contract suite covers 6 valid and 23 invalid fixtures plus the shipped examples; found two real defects before release (hard-coded `/bin/bash`, `/dev/null` treated as a terminal) and a lint-masking Makefile bug |
| IV. Module contracts | PASS | `module.v1` encodes capabilities, events, permissions, approvals, data classes, resources, licence; unknown fields rejected |
| V. Secure/private | PASS | No secret values accepted (`secret_ref` pattern); sensitive classes restricted to in-house modules; fake data only (scan in T040) |
| VI. OSS hygiene | PASS | Licence check + generated `THIRD_PARTY_LICENSES`, DCO script with self-test, CONTRIBUTING/SECURITY/CODE_OF_CONDUCT/templates |
| VII. Simplicity/measured | PASS | One extra small dependency dropped (go-spdx); `make test` measured in `measurements.md` |
