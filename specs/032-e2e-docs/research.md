# Research and decisions: E23

## Can the compose stack issue and verify personal access tokens? No

can-core builds module management (catalogue, changes, wizard, PAT issue) only when an installer exists
(`core/server/runtime/runtime.go`: `if inst != nil`). Compose runs `CAN_INSTALLER=none`, so `/catalogue` answers
503, `/tokens` says no enabled module offers a permission, and the fake `TokenReview` (`tokenreview.Fake`) only
accepts tokens added from Go code. A company file in the data volume did not change this (tried: `CAN_COMPANY_FILE`
plus a minimal `company.yaml`, still 503). Decision: assert the limitation in `TestAdminLoginTOTPAndPages`, use
module-minted service tokens (work, jobs, finance have a tokens API) for scenarios, and file the gap (issue #43).

## People and equity need more than one identity

Both accept only their static service token without an introspection endpoint. The service-account token has no
person record (people) and never carries the board role (equity), so leave approval by a second person, leave
masking for a colleague and board approval of a grant cannot be shown with it. Both services already accept an
introspection URL (`POST {"token"}` returns `{active, subject, roles, person_id}`), which is what can-core's
`/v1/tokens/introspect` is shaped like. Decision: a 90-line stub (`dev/compose/introstub`) with a fixed table of five
fake tokens, wired in only by the overlay `dev/compose/e2e.yml` (config files `people.e2e.yaml`, `equity.e2e.yaml`).
`allow_insecure_http: true` is needed because the stub is plain HTTP on the compose network. People records link to
the stub's subjects through `sso_subject`, created idempotently by the suite.

## Findings from running the suite against the live stack

| Finding | Action |
|---|---|
| `equity` was not in the compose stack although its documentation lists it | added (image target, config, token, keyring in `init.sh`, edge host) |
| An unknown `Host` header got an **empty 200** from Caddy, not the public site as `docs/local-compose.md` said | catch-all `http://:8080` block answering 421; documentation corrected |
| An authentication code is single use: two admin logins in one 30 s step fail the second with 401 | `adminLogin` waits for the next step and retries once |
| `podman-compose up -d --build <service>` did not recreate a running container after the image changed | documented: add `--force-recreate` |
| podman-compose reruns the one-shot `init` job once per dependent service | already documented; `init.sh` is idempotent |
| Jobs token creation returns `secret`, work and finance return `token` | helper per module in the scenarios |
| Equity grant types are upper case (`NSO`); activation cannot predate approval | scenario uses upper case and today's date |
| Leave requests must fall within 90 days back and two years ahead; a clash with an earlier run is 409 or 422 | scenario picks a free weekday |
| The compose healthchecks, `service_completed_successfully` and `service_healthy` all work on the real Docker Compose (v5.1.3 on Docker Engine 29.4.2) | recorded in `docs/local-compose.md` |

## Why Go and not shell

`dev/compose/smoke.sh` already exists as a shell and curl smoke test (36 checks). It cannot compute a TOTP code
without an extra tool (`oathtool` is not installed on the author's machine nor guaranteed on runners), parses JSON
with `sed`, and has no per-scenario structure. A Go test needs only the toolchain every contributor has. The shell test
stays as a fast smoke check and was extended with equity and the 421 rule.

## Why advisory

Building ten Go binaries in the compose images takes minutes, compose is a dev stack, and a failing nightly should
alert without blocking merges (the same stance as `dev-cluster` and `footprint`). Required check names in `ci.yml`
and `scripts/gh-protect-main.sh` are untouched; a contract test keeps `e2e` out of them.

## Documentation numbers

Footprint figures in the README quote their source: `docs/measurements/phase0-ci.md` (k3s node, forms, can-static),
`docs/module-wiring.md` (can-core), module READMEs (idle RSS of each service, a different measure from the cgroup
working set in CI, noted in the table), and a new `docker stats` sum of the compose stack after a full e2e run
(about 94 MB; core 35, edge 13, work 9, people 9, jobs 8, finance 6, equity 4, forms 4, site 3, introstub 2).
Nothing about a 2 GB server was measured and the README says so.
