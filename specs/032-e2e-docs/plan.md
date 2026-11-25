# Implementation plan: End-to-end compose tests in CI and documentation refresh (E23)

Spec: [spec.md](spec.md). Research: [research.md](research.md). Data model: [data-model.md](data-model.md).
Contracts: [contracts/e2e-harness.md](contracts/e2e-harness.md), [contracts/ci-workflow.md](contracts/ci-workflow.md),
[contracts/doc-links.md](contracts/doc-links.md). Tasks: [tasks.md](tasks.md).

## Shape

- `test/e2e/harness/` (plain Go package, unit tested): `totp.go`, `client.go`.
- `test/e2e/*_test.go` (build tag `e2e`): one file per scenario group; `main_test.go` holds `TestMain`, the admin
  TOTP bootstrap and per-module API helpers.
- `dev/compose/`: `Dockerfile` (equity and introstub targets), `config/equity*.yaml`, `config/people.e2e.yaml`,
  `config/introstub.json`, `e2e.yml` overlay, `introstub/` (Go, with tests), `secrets/equity.env`, `init.sh`
  (equity keyring), `config/Caddyfile` (equity host, 421 catch-all), `smoke.sh` (equity and 421 checks).
- `.github/workflows/e2e.yml`; `Makefile` targets; `.golangci.yml` (`build-tags: [e2e]` so the tagged files are linted).
- `scripts/check-doc-links.sh`, `test/contract/doclinks_test.go`, `test/contract/e2e_workflow_test.go`.
- Documentation: `README.md`, `docs/{index,getting-started,architecture,security-model}.md`, `docs/local-compose.md`,
  `docs/modules-catalogue.md`, `docs/epics/README.md`, module READMEs, `CONTRIBUTING.md`, `SPEC.md` (contradictions only).

## Constitution check

| Principle | Check |
|---|---|
| I Spec-first, SPEC.md is the truth | SPEC.md is changed only where it contradicted built reality (can-core exists, `make dev` scope, `make dev-module` and the section 6.1 targets not implemented, 1:1 notes sealed, no offline PWA mode); each change is listed in the pull request. |
| II Footprint is a feature | No product binary grows: equity is one more compose service (measured 4 MB), introstub is test-only and 2 MB. The compose total was re-measured (about 94 MB) and reported next to the claim. |
| III Test first (non-negotiable) | The TOTP generator was written against RFC 6238 vectors before use; the doc-link checker has a contract test that proves it catches a broken link and ignores code blocks, and the workflow shape has a contract test; both were written together with the artefact rather than strictly before it (the script and workflow existed when the tests first ran; the first run failed on a path bug in the test). introstub has handler tests. Scenarios were run against the live stack as written, and failures (equity type case, leave date window, jobs `secret` field, unknown host) were fixed in the test or the stack, not skipped. |
| IV Stable module contracts | No module API changed. The e2e suite is a client of the public HTTP surface only. |
| V Secure and private by default | Fake secrets stay public and dev-only, labelled; the new edge catch-all closes an empty-200 on unknown hosts; introstub is not built into any release image and the base stack does not use it; the workflow is read-only and has no secrets. |
| VI Open-source hygiene | Actions pinned by SHA, `contents: read`, `persist-credentials: false`, contract-tested; commits signed off; no new dependency (standard library only). |
| VII Simplicity and measured claims | Go standard library instead of a test framework; numbers in the README have a source; what compose cannot prove is written down and asserted (issue #43), not papered over. |

## Design decisions in brief

1. **Go with a build tag, not shell.** Same language as the product, typed JSON handling, one process for TOTP and
   HTTP, runs with `go test`; shell stays for the older `smoke.sh`.
2. **Drive the edge, not the containers.** Host-routed requests to `127.0.0.1:<port>` exercise Caddy's routing and
   the new 421 rule. `docker compose exec` is used only to rotate and read the admin TOTP secret and for
   `down` / `up`.
3. **A fake introspection service rather than extending can-core** for people and equity identities; extending can-core
   to enable PATs in compose is a separate change (#43).
4. **Advisory CI**, because the build is slow and compose is a dev stack; the workflow still fails visibly.
5. **Docs link check is file-existence only**, offline, with fenced code and inline code skipped.

## Testing

`go test ./test/e2e/harness/... ./dev/...` (in `make unit`), `go test ./test/contract/...` (doc links, workflow
shape, build tags), `make e2e` against a running stack, the `e2e` workflow, `make lint` (with the `e2e` tag),
`sh scripts/lint-shell.sh`, `sh scripts/lint-dockerfiles.sh`, `sh scripts/scan-secrets.sh`.
