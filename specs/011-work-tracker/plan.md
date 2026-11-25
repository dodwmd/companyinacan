# Implementation Plan: Work Tracker, first slice (Epic E14a)

**Branch**: `epic/E14a-work-tracker` | **Date**: 2026-11-25 | **Spec**: [spec.md](spec.md)

**Input**: Feature specification from `/specs/011-work-tracker/spec.md`

## Summary

An in-house Go service (`cmd/can-work`, library `core/work`) with SQLite storage that implements
the `work` and `projects` capabilities: projects, issues, workflows, sprints, boards, comments,
links, watchers, activity, search, bulk operations, scoped API tokens, signed webhook events and
audit, plus the `can work` CLI. It mirrors the forms gateway (E05): same store pattern, same
`Authenticator` seam, same webhook signature, same module/chart/image shape. Choices are in
[research.md](research.md), entities in [data-model.md](data-model.md), wire contracts in
[contracts/](contracts/).

## Technical Context

**Language/Version**: Go 1.27 (existing module)

**Primary Dependencies**: standard library only for the service (`net/http`, `database/sql`,
`log/slog`, `crypto/*`); `modernc.org/sqlite` and `go.yaml.in/yaml/v3` are already in go.mod; the
CLI uses the existing `spf13/cobra`. No new dependency.

**Storage**: SQLite file with embedded migrations (one connection, WAL)

**Testing**: `go test`; the API is driven in-process with `httptest` and a fake clock; a real
loopback webhook receiver; contract tests read the OpenAPI document, router source, schemas,
fixtures, manifest and chart; no network, no container runtime

**Target Platform**: linux/amd64 and arm64 static binary in distroless

**Project Type**: single service plus library plus CLI client

**Performance Goals**: human-speed traffic; searches and boards over thousands of issues stay in
milliseconds (indexed); idle RSS at most 20 MB

**Constraints**: bounded memory everywhere (rate limiter key sets, list pages, board columns,
bulk sizes, request bodies, query length); no cgo; secrets only from environment

**Scale/Scope**: about 5 k lines of Go, 2.4 k lines of tests, 1 migration, 4 event schemas, 2
capability schemas, 1 OpenAPI document

## Constitution Check

| Principle | Status | Notes |
|---|---|---|
| I. Spec-first | PASS | Traces to SPEC.md 3.3 (work tracking and its API), 4.3, 6.1. Additions recorded here and in research.md: `projects` capability name (one SPEC row added), `$N` batch parent references, `work.issue.*` event names carry the module prefix |
| II. Footprint | PASS | Measured 13.6 MiB idle and 20.4 MiB after load (R7) against a 20 MB target and a 48 MB limit; single SQLite connection; every list, queue and map bounded |
| III. Test-first | PASS with a note | Tests for rank, query language, workflow and SLA were written and run before their implementation. For the store, API, tokens and CLI the tests were written in the same working session as the code, in small increments, not strictly before each function; they cover every acceptance scenario and security property in the spec. The contract tests (OpenAPI vs router vs schemas) guard drift |
| IV. Stable module contracts | PASS | Manifest validates under module.v1; capabilities `work`, `projects`, four events, three permissions and roles declared; `work.v1` exposes a deliberately small stable surface while the module's full API is OpenAPI; auth behind an `Authenticator` so can-core introspection plugs in without wire changes |
| V. Secure and private | PASS | Tokens hashed (SHA-256 of 256-bit secrets), constant-time compare, scopes and per-project scoping enforced in the store (404 not 403), mass assignment refused, parameterised search, LIKE wildcards escaped, bounded bodies and queries, rate limits, auth-failure throttling, audit of admin actions, no secrets in logs or audit, redirects not followed, events carry no bodies, retention declared and enforced |
| VI. OSS hygiene | PASS | No new dependency; Apache-2.0; DCO sign-off; `can licences --check` unchanged |
| VII. Simplicity and measured claims | PASS | stdlib router and middleware, SQLite outbox instead of a broker, offset cursors, no ORM; footprint measured (R7); module status is `experimental` because the image and cluster checks have not happened |

Post-design re-check: no violations. Complexity Tracking is empty.

## Project Structure

```text
cmd/can-work/main.go                 # thin entry point
core/work/
  model.go workflow.go rank.go query.go          # pure rules: types, validation, workflow, fractional rank, search language
  store.go issues.go extras.go sprints.go tokens.go retention.go   # SQLite store; all rules live here
  events.go                                      # event envelope, transactional outbox, signed dispatcher
  auth.go api.go httputil.go                     # Authenticator, HTTP surface, limits
  config.go app.go cmd.go client.go              # wiring, entry point, API client
  migrations/001_work.sql
core/cli/work.go                     # `can work`
schemas/capabilities/{work,projects}.v1.json  schemas/events/work.*.v1.json
modules/work/{module.yaml,README.md,settings.schema.json,chart/,events/}
deploy/work/{Dockerfile,README.md}
docs/work-tracker.md
testdata/{capabilities,events}/...
test/contract/work_test.go
specs/011-work-tracker/
```

## Design notes

- All business rules sit in the store, below the HTTP layer, and every store method takes the
  caller's `Principal`, so project scoping cannot be forgotten by a handler (IDOR defence in one
  place). A key outside the scope is `ErrNotFound`.
- One SQLite connection serialises writes: key numbering and rank neighbours cannot race, and
  every multi-step operation runs in one transaction with only the `tx` in use (no deadlock).
- Events are written to an outbox table in the same transaction as the change, then delivered by
  one goroutine. No webhook configured means no outbox rows.
- The HTTP layer decodes strictly (unknown fields are errors), authenticates, rate limits, checks
  the scope and caps the body before any handler runs.
- Time, clock-dependent expiry and the authenticator are injected, so tests are deterministic.
