# Contract: `.github/workflows/e2e.yml`

- Name `e2e`, one job `e2e` (not equal to any `ci.yml` job name; absent from `scripts/gh-protect-main.sh`).
- Triggers: `pull_request` (path filter: compose files, `dev/**`, `core/**`, `cmd/**`, `go.mod`, `go.sum`, `test/e2e/**`,
  `Makefile`, the workflow), `schedule` (03:17 UTC nightly), `workflow_dispatch`. Never `push` or `pull_request_target`.
- `permissions: contents: read` at the top level, no job-level write, no secrets other than the automatic token.
- Actions pinned by 40-hex SHA with a version comment; `actions/checkout` with `persist-credentials: false`.
- `continue-on-error: true`, `timeout-minutes: 40`, a concurrency group that cancels superseded runs.
- Steps: tool versions (`docker version`, `docker compose version`), `make e2e-up`, `docker compose ps -a`, `make e2e`,
  on failure collect `ps` and each service's log into `e2e-logs/` and upload it as an artifact (7 days), a job summary that
  says what is and is not covered, `make e2e-down` always.
- Enforced by `test/contract/e2e_workflow_test.go` and the shared hygiene tests in `test/contract/workflows_test.go`.
