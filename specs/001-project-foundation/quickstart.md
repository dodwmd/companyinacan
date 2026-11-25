# Quickstart / Validation Guide: Project Foundation (E01)

Prerequisites: Go (see `go.mod`), `git`, `make`. No network is needed after dependencies are
downloaded once; no container runtime is needed.

## 1. Full local check (SC-001)
```sh
make test        # lint + unit + contract + licence check, expect exit 0 in < 2 min
make build       # produces bin/can
```

## 2. Create and validate a company (US2, SC-003)
```sh
d=$(mktemp -d); bin/can init "$d" --name "Acme Co" --domain acme.example --provider local --non-interactive
bin/can validate "$d" --modules-dir modules   # expect: no errors, exit 0
bin/can init "$d" --name x --domain x.example --non-interactive; echo $?   # expect: 1, file untouched
bin/can init "$(mktemp -d)" </dev/null; echo $?                            # expect: 2, no hang
```

## 3. Schema contract (US1, SC-002)
```sh
go test ./test/contract/...      # every valid fixture passes, every invalid fixture fails exactly as its .expect.yaml says
bin/can validate examples/acme-co --modules-dir modules
```
Add a new failure mode by dropping `testdata/invalid/<name>.yaml` and `<name>.expect.yaml`.

## 4. Doctor, version, stubs (US2)
```sh
bin/can version; bin/can doctor; bin/can doctor --output json
bin/can apply; echo $?    # expect: "not implemented ... epic E03", exit 3
make dev; echo $?         # expect: exit 3
```

## 5. Licence check (US3, SC-006)
```sh
bin/can licences --check                # passes on the committed example modules
bin/can licences --write && git diff --exit-code THIRD_PARTY_LICENSES   # idempotent
go test ./core/licence/...              # fixtures: accepted, missing, unknown, source-available with/without mark, OR/AND expressions
```

## 6. CI and DCO (US4, SC-004)
Open a PR from a branch with signed-off commits: five named checks green. Then push a commit
without `Signed-off-by` and confirm only `dco` fails; break a fixture and confirm only
`contract` fails.

## 7. Hygiene review (SC-005)
Check the repository root for `CONTRIBUTING.md`, `SECURITY.md`, `CODE_OF_CONDUCT.md`,
`.github/ISSUE_TEMPLATE/*`, `.github/pull_request_template.md`, and a README in each top-level
directory listed in `SPEC.md` §8.
