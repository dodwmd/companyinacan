# Contract: running the end-to-end suite

## Targets

| Target | What it runs |
|---|---|
| `make e2e-up` | `$(E2E_COMPOSE) up -d --build` with `E2E_COMPOSE = docker compose -p can-e2e -f docker-compose.yml -f dev/compose/e2e.yml` |
| `make e2e` | `go test -tags e2e -count=1 -timeout 15m -v ./test/e2e/...` with `E2E_COMPOSE` exported |
| `make e2e-down` | `$(E2E_COMPOSE) down -v` (only the `can-e2e` project) |
| `make docs-check` | `sh scripts/check-doc-links.sh` |

## Environment

| Variable | Default | Meaning |
|---|---|---|
| `E2E_PORT` | `8080` | host port of the edge (`127.0.0.1`) |
| `E2E_COMPOSE` | the command above | compose command prefix; run with the repository root as working directory |
| `E2E_ROOT` | found from the harness source path | repository root, for the public dev secrets in `dev/compose/secrets/` |
| `E2E_RESTART` | unset | `0` skips the restart persistence scenario |

## Behaviour

- Requests are sent to `http://<virtual host>:<port>/...` but dialled to `127.0.0.1:<port>`; a `Host` header override
  is available per request.
- A run rotates the admin TOTP secret once (`can-core admin reset-password --rotate-totp` through `compose exec`).
- Exit status is the `go test` status; `TestMain` exits 2 when the edge does not answer within 90 seconds.
- Output lines `ok   <what> (HTTP <n>)` are logged per passing assertion; a failure prints the step, the wanted and the
  actual status and the first 400 bytes of the body.

## Scenario catalogue

`TestSiteAndLeadCapture`, `TestAdminLoginTOTPAndPages`, `TestWorkTrackerAPIAndUI`, `TestPeopleOneOnOneAndTimeOff`,
`TestJobsRequisitionApplyAndBlindFeedback`, `TestFinanceInvoiceFlow`, `TestEquityGrantApprovalAndVesting`,
`TestCrossModuleTokensAreRefused`, `TestHostileHostReachesNothingPrivate`, `TestMetricsAndDebugAreNotExposed`,
`TestAnonymousCannotReachPrivateSurfaces`, `TestRestartKeepsData`.
