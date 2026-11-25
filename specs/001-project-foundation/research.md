# Research: Project Foundation (E01)

All unknowns from the plan's Technical Context are resolved below. Versions were checked against
the Go module proxy on 2026-11-24.

## R1. Language and toolchain
- **Decision**: Go, one module at the repo root (`github.com/dodwmd/companyinacan`), `go`
  directive set to the installed stable toolchain (1.27). CI uses `go-version-file: go.mod`.
- **Rationale**: Constitution technology constraint; later epics need `client-go` and the Helm SDK.
- **Alternatives**: none (fixed by the constitution).

## R2. CLI framework
- **Decision**: `github.com/spf13/cobra` v1.10.2.
- **Rationale**: De-facto standard, shell completion, consistent help; the later `can` commands
  (providers, backup, restore) fit its command tree.
- **Alternatives**: `urfave/cli` (fine, but no advantage here), stdlib `flag` (too much glue for
  ~15 eventual subcommands).

## R3. YAML parsing
- **Decision**: `go.yaml.in/yaml/v3` v3.0.6 (the maintained continuation of `gopkg.in/yaml.v3`,
  which has had no release since 2022). Decode to `yaml.Node` first so validation errors can carry
  line and column; decode the node to `any` for schema validation and to typed structs for
  semantic checks.
- **Rationale**: Node API gives line numbers for FR-003 messages and for the parse-error edge case.
- **Alternatives**: `goccy/go-yaml` v1.19.2 (good errors, but a larger API surface and a second
  YAML dialect to track); `gopkg.in/yaml.v3` (unmaintained).

## R4. JSON Schema validation
- **Decision**: `github.com/santhosh-tekuri/jsonschema/v6` v6.0.3, draft 2020-12, schemas embedded
  with `go:embed`. Every object uses `unevaluatedProperties: false` / `additionalProperties:
  false` so unknown fields fail (spec US1 scenario 4).
- **Rationale**: Complete draft 2020-12 support, no network by default when schemas are
  pre-loaded, structured `ValidationError` output we can map to file + field path + reason.
- **Alternatives**: `xeipuuv/gojsonschema` (older drafts, unmaintained); `kaptinlin/jsonschema`
  (less proven).

## R5. Cross-field and semantic validation
- **Decision**: Plain Go checks after schema validation, each returning a typed `Problem`
  (file, path, rule id, message): `ram_limit_mb >= ram_mb`; unique module ids and subdomains;
  `subscribes`/`publishes` event names end in `.vN`; every `data_classes` entry has `retention`;
  a data class with `sensitivity: sensitive` is only allowed when `provider.type: inhouse`
  (Constitution V); every `requires.capabilities`/`consumes` entry is a known capability name or
  declared by a provided module (checked at company level only when manifests are loaded);
  exclusive-capability conflicts between enabled modules unless `primary:` designates one
  (non-`aggregate` capabilities); `primary:` values must name enabled modules providing that
  capability. All problems are collected, not first-fail (FR-003).
- **Rationale**: These rules are not expressible (or readable) in JSON Schema.
- **Alternatives**: CEL or `if/then` in schema (opaque messages).

## R6. Capability and event name registry
- **Decision**: A data file `schemas/names.yaml` lists known capability names (and which are
  `aggregate`) and the reserved platform events; validation of names is by pattern
  (`^[a-z][a-z0-9]*(\.[a-z0-9]+)*$` for capabilities, `...\.v[0-9]+$` for events) and unknown
  capability names are a **warning** until a capability schema exists in `schemas/capabilities/`
  (later epics).
- **Rationale**: Keeps E01 independent of later capability definitions while still catching typos
  in the known set.

## R7. Licence checking
- **Decision**: A small SPDX-expression evaluator in `core/licence` (AND, OR, WITH, parentheses;
  `WITH` exceptions do not change acceptance; ids compared case-insensitively) plus
  `schemas/accepted-licences.yaml` (reviewable data, FR-012). Rules: a single id must be accepted;
  `A AND B` requires both; `A OR B` passes if every alternative is accepted, or if
  `provider.licence_selected` names an accepted alternative that appears in the expression;
  some-but-not-all accepted without a selection is `licence.expression-ambiguous`; none accepted
  is `licence.not-accepted`; `licence_class: source-available` passes and is listed separately;
  `provider.type: inhouse` is skipped (Apache-2.0 by repository licence); a missing licence on a
  non-inhouse module fails. Directories starting with `_` (examples) are not listed.
- **Output**: `THIRD_PARTY_LICENSES` sorted by module id, no timestamps, LF endings (SC-006);
  `--check` fails with the regeneration command when the committed file differs.
- **Why not `github/go-spdx`**: v2.8.0 exists, but the project only needs boolean acceptance over a
  tiny grammar; owning ~150 lines avoids a dependency and its release cadence (Constitution VII).

## R8. `can init` interaction
- **Decision**: Interactive only when stdin is a terminal (`os.Stdin.Stat()` char-device check)
  and `--non-interactive` is not set; otherwise `--name`, `--domain`, `--provider` are required
  and missing ones produce a usage error (exit 2) without hanging. Writes `company.yaml` plus a
  `.gitignore` containing `.can/`; refuses to overwrite without `--force`. No network.
- **Alternatives**: a TUI library (extra dependency, not needed).

## R9. `can doctor`
- **Decision**: `exec.LookPath` and `--version` probing only. Required now: `go`, `git`, `make`.
  Optional with remedy text: `docker` or `podman`, `k3d`, `kubectl`, `helm` (needed from E02),
  `restic`/`sqlite3` are server-side and not checked locally. Exit non-zero only if a required
  tool is missing. `--json` for machines.

## R10. Exit codes and output
- **Decision**: 0 success; 1 failure (validation problems, doctor missing required tool); 2 usage
  error (cobra); 3 not implemented (stubs). `--output json|text` (default text) on `validate`,
  `doctor`, `version`, licence check. Problems are printed as `file:line:col: path: message`.

## R11. Test strategy
- **Decision**: Table-driven Go tests. `testdata/valid/*.yaml` must validate; `testdata/invalid/
  <name>.yaml` each have a sidecar `<name>.expect.yaml` listing the expected problems
  (`rule`, `path`, optional line) so a new failure mode is added by dropping two files. Contract
  tests also validate `examples/` and `modules/_example/` so examples cannot drift (Edge case).
  CLI tests run the cobra command tree in-process with captured stdout/stderr/exit codes. A golden
  file test covers `THIRD_PARTY_LICENSES`. No network, no containers.

## R12. Lint
- **Decision**: `golangci-lint` v2.15.0, run via `go run github.com/golangci/golangci-lint/v2/
  cmd/golangci-lint@v2.15.0 run` (no local install needed) with a small `.golangci.yml`
  (govet, staticcheck, errcheck, revive basics, gofmt, goimports). `make lint` falls back to
  `go vet` with a notice when the module proxy is unreachable.

## R13. CI and DCO
- **Decision**: GitHub Actions workflow `.github/workflows/ci.yml` with separate jobs `lint`,
  `unit`, `contract`, `licences`, `dco`; `permissions: contents: read`; actions pinned by commit
  SHA (`actions/checkout`, `actions/setup-go`), SHAs resolved with `gh api` at implementation
  time. DCO is `scripts/check-dco.sh <base> <head>` walking `git rev-list --no-merges`, requiring a
  `Signed-off-by: Name <email>` trailer matching the commit author's name or email; no third-party
  app. Unit and contract jobs use `go test` with `-run`/package selection (`./core/...` vs a
  `contract` build tag-free package `./test/contract/...`).
- **Alternatives**: the DCO GitHub App (needs repository-wide install), `tim-actions/dco` (third
  party action).

## R14. Repository layout
- **Decision**: As in `SPEC.md` §8, with Go code under `cmd/can` and `core/*` (`cli`, `config`,
  `schema`, `validate`, `licence`, `doctor`) and a README in `core/`, `modules/`, `schemas/`,
  `deploy/`, `dev/`, `docs/`, `examples/`, `templates/`. The two exceptions to "one place per
  concern" are `scripts/` (CI helpers) and `test/contract/` (cross-package contract tests).

## R15. Module manifests location for `can validate`
- **Decision**: `can validate [dir]` loads `dir/company.yaml`; each enabled module's manifest is
  resolved from `--modules-dir` (default `$CAN_MODULES_DIR`, else `./modules`, else the
  `modules/` directory next to the executable's source checkout). Missing manifest is a problem.
