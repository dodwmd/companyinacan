# Implementation Plan: Finance basics, first slice (Epic E15a)

**Branch**: `epic/E15a-finance` | **Date**: 2026-11-25 | **Spec**: [spec.md](spec.md)

## Summary

An in-house Go service (`cmd/can-finance`, library `core/finance`) with SQLite storage implementing the `invoices`,
`contracts` and `timesheets` capabilities plus expenses and purchase orders. It follows the sibling services (forms E05,
work E14a, people E13a): same store pattern (one write connection, a small read pool), same `Authenticator` seam, same
webhook signature and outbox, same module/chart/image shape. Choices are in [research.md](research.md), entities in
[data-model.md](data-model.md), wire contracts in [contracts/](contracts/).

## Technical Context

Go 1.27, standard library only for the service; `modernc.org/sqlite` and `go.yaml.in/yaml/v3` are already in go.mod. No new
dependency. Storage: SQLite with embedded migrations (WAL, `secure_delete`). Testing: `go test`; the API is driven in-process
with `httptest` and a fake clock; contract tests read OpenAPI, router source, schemas, fixtures, manifest and chart and run a
real instance against a loopback webhook receiver; no network, no container runtime. Target: linux static binary in distroless.
Performance: human-speed traffic; idle RSS at most 20 MB. Scale: about 8 k lines of Go, 2.5 k lines of tests, 1 migration,
6 event schemas, 3 capability schemas, 1 OpenAPI document.

## Constitution Check

| Principle | Status | Notes |
|---|---|---|
| I. Spec-first | PASS | Traces to SPEC.md 3.3, 3.7, 4.3, 5, 6.1. Additions recorded: capability name `timesheets` (the `time` name stays reserved); events `invoice.sent.v1`, `timesheet.approved.v1`, `expense.approved.v1` added to the catalogue (`invoice.issued.v1` and `time.approved.v1` in SPEC are not produced by this module) |
| II. Footprint | PASS | 16.1 MiB idle, 24.8 MiB after load (R8) against a 20 MB target and 48 MB limit; one write connection; every list, map and queue bounded |
| III. Test-first | PASS with a note | Money, rounding and FX tests were written and seen failing (build errors) before `money.go`. The store and API were written first and their tests followed in the same session; to show the tests bite, two mutations were applied and observed to fail (separation of duties disabled: 2 tests red; scope clause disabled: 3 tests red). Contract tests guard drift |
| IV. Stable module contracts | PASS | Manifest validates; three capabilities, six events, four permissions, four approval types and data classes declared; auth behind an `Authenticator` so can-core introspection plugs in without wire changes; adapters behind interfaces |
| V. Secure and private | PASS | Hashed tokens (constant time), scopes enforced in the store with 404 for out-of-scope, mass assignment refused, parameterised SQL, LIKE escaping, bounded bodies and reads, a throttle that never blocks valid credentials, audited sensitive reads and exports, append-only audit and immutable records enforced by triggers, no secrets or contact data in logs, audit or events, retention and erasure implemented, tool-not-advice disclaimer in the docs |
| VI. OSS hygiene | PASS | No new dependency; Apache-2.0; DCO sign-off; fake data only; `can licences --check` unchanged |
| VII. Simplicity and measured claims | PASS | stdlib router, SQLite outbox, id cursors, no ORM, no PDF engine, fakes instead of speculative adapters; footprint measured |

Post-design re-check: no violations. Complexity Tracking is empty.

## Project Structure

```text
cmd/can-finance/main.go
core/finance/
  money.go                                         # minor units, rounding, FX (pure)
  model.go scope.go                                # errors, validation, scope clauses
  parties.go invoices.go payments.go recurring.go reports.go export.go render.go   # receivables
  contracts.go timesheets.go expenses.go purchasing.go
  providers.go                                     # payment and e-signature adapter interfaces and fakes
  retention.go idem.go store.go tokens.go auth.go events.go
  api.go api_billing.go api_work.go httputil.go    # HTTP surface
  config.go app.go cmd.go
  migrations/001_finance.sql
schemas/capabilities/{invoices,contracts,timesheets}.v1.json  schemas/events/*.v1.json
modules/finance/  deploy/finance/  docs/finance.md  testdata/  test/contract/finance_test.go
specs/014-finance-basics/
```
