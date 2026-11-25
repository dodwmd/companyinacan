# Contract: `can` command line and `make` targets

Global: `--output text|json` (default `text`), `--no-color`, `-h/--help`. Diagnostics go to
stderr, results to stdout. Exit codes: `0` success, `1` failure (validation/doctor/licence),
`2` usage error, `3` not implemented.

## `can version`
Prints `can <version> (<commit>, <date>) go<version> <os>/<arch>`. JSON: `{"version","commit","date","go","os","arch"}`.

## `can init [dir]`
Flags: `--name`, `--domain`, `--provider` (default `local`), `--force`, `--non-interactive`.
- Interactive only if stdin is a terminal and `--non-interactive` is unset; otherwise the three
  values must be given or the command exits `2` naming the missing flags.
- Writes `dir/company.yaml` (valid against `company.v1`) and `dir/.gitignore` (`.can/`).
- Exits `1` if `company.yaml` exists and `--force` is not set; writes nothing in that case.
- Never touches the network or files outside `dir`.

## `can validate [dir]`
Flags: `--modules-dir <path>`. Validates `dir/company.yaml` and the manifest of every enabled
module (see research R15). Reports all problems; exit `1` if any `error`, `0` otherwise
(warnings do not fail). Text line: `file:line:col: /path: message [rule]`.

## `can doctor`
Reports each prerequisite as `ok`, `missing` (with remedy) or `optional-missing`. Exit `1` only
when a **required** tool is missing (`go`, `git`, `make`). JSON: array of
`{"tool","required","status","version","remedy"}`.

## `can apply`
Stub: prints `can apply is not implemented yet (planned in epic E03: can-core MVP)` and exits `3`.
No side effects.

## `can dev`
Implemented in epic E02; see `specs/004-local-dev-cluster/contracts/cli.md`.

## `can up`
Stub: prints `can up is not implemented yet (planned in epic E06: server providers and make up)` to stderr, exit `3`, no side effects.

## `can licences` (also `make licences`)
Flags: `--modules-dir`, `--write` (regenerate `THIRD_PARTY_LICENSES`), `--check` (CI mode:
fail if the committed file differs). Violations are named per module with rule ids
`licence.missing`, `licence.not-accepted`, `licence.expression-ambiguous`. Exit `1` on any.

## Make targets
| Target | Behaviour |
|---|---|
| `make` / `make help` | list targets |
| `make build` | build `bin/can` |
| `make unit` | `go test -race ./cmd/... ./core/...` (the race detector is on everywhere; about 40 s cold) |
| `make test` | `lint` + unit + contract + `licences --check`; no network required except an optional first-time lint download |
| `make lint` | golangci-lint (pinned), falls back to `go vet` with a notice offline |
| `make licences` | regenerate `THIRD_PARTY_LICENSES` |
| `make dev`, `make down`, `make doctor` | run `can dev up`, `can dev down`, `can dev doctor` (epic E02) |
| `make up` | runs `can up`: not implemented (epic E06); make reports failure (status 2), `can up` itself exits 3 |

## CI contract (`.github/workflows/ci.yml`)
Jobs with exactly these names: `lint`, `unit`, `contract`, `licences`, `dco`. Trigger: pull
requests and pushes to `main`. `permissions: contents: read`. `dco` runs
`scripts/check-dco.sh "$BASE" "$HEAD"`; failure message includes the `git commit -s` /
`git rebase --signoff` remedy.
