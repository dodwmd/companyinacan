# Implementation Plan: Equity and cap table, first slice (E17a)

**Branch**: `epic/E17a-equity` | **Date**: 2026-11-25 | **Spec**: [spec.md](spec.md)

> Tool, not legal, tax or securities advice. 409A, ISO/NSO limits, tax treatment, 83(b) and ESOP scheme
> rules are not implemented (data fields and documented limits only).

## Summary

One in-house Go service, `cmd/can-equity` over library `core/equity`, built like `can-people` and
`can-finance`: SQLite store with embedded migrations, an `Authenticator` seam, a REST JSON API on a
ClusterIP-only listener, an outbox for events, a manifest, a values-only chart and a Dockerfile fragment.
Authorisation is one pure function (`Decide`/`FieldAccess`); the vesting calculator and the round model are
pure functions; separation of duties is one function (`sodOK`) repeated as triggers.

## Technical Context

**Language/Version**: Go 1.27. **Dependencies**: stdlib, `modernc.org/sqlite`, `golang.org/x/crypto`,
`golang.org/x/text` (case folding, NFKC) and `go.yaml.in/yaml/v3`, all already in `go.mod`; no new ones.
**Storage**: SQLite (WAL, `secure_delete`, one connection). **Testing**: `go test` with `-race`, fuzz targets
for vesting, ownership allocation and the round model, contract tests in `test/contract`. **Target**: Linux
container, distroless static. **Constraints**: idle RAM 16 MB target, limit 64 MB; no cgo; fail closed.

## Constitution Check (before research)

Checked against `.specify/memory/constitution.md`.

| Principle | Assessment |
|---|---|
| I Spec-first | Refines SPEC.md 3.7, 4.3 and *Stock and options*. Divergence: the catalogue names `equity.proposed/approved/granted.v1`; this slice publishes `equity.grant.approved.v1`, `equity.grant.vested.v1` and `equity.exercise.recorded.v1` (the SPEC names stay in the catalogue; "granted" after a signed agreement is the activation step, recorded as data, not yet an event). |
| II Footprint | 16 MB / 64 MB declared in manifest and chart; measured locally; no sidecar, no extra always-on dependency. |
| III Test-first | Vesting tests were written before the calculator and observed failing (undefined symbols) before it existed; the service, API and contract tests followed each slice. Schemas have valid and invalid fixtures; routes are checked against OpenAPI. k3d integration tests are pending. |
| IV Stable contracts | Capability `equity.v1` and three event schemas with fixtures; modules consume ids, never internals. |
| V Secure and private | In-house only; equity is a sensitive class: per-field access, audited reads (fail closed), sealed columns, retention, legal hold and erasure tombstones delivered with the first module that stores it; ClusterIP only; restricted security context; disclaimer present. |
| VI Open-source hygiene | Apache-2.0; no new dependencies; fake data only. |
| VII Simplicity and measured claims | Boring stack; RAM measured, not asserted; the round model is a documented simple formula flagged illustrative; complexity tracked below. |

## Project Structure

```text
cmd/can-equity/main.go            entry point
core/equity/
  policy.go                       the single authorisation layer (pure) and sodOK
  vesting.go                      pure vesting calculator (cliff, periodic, tranches, acceleration, rounding)
  whatif.go                       pure round model and recorded convertibles
  captable.go                     pool status, cap table as of a date, ownership allocation
  model.go crypto.go              quantities, money, canonical subjects; keyring and AEAD
  service.go structure.go         Service, migrate, idempotency; entities, classes, plans, stakeholders
  grants.go exercises.go ledger.go valuations.go   domain operations
  export.go retention.go          CSV export; erasure, retention, legal hold, key rotation
  audit.go events.go              audit hook + hash chain; outbox + signed dispatcher + sweep
  api.go auth.go config.go cmd.go REST surface, authenticators, config, CLI
  migrations/001_equity.sql 002_immutability.sql
modules/equity/                   module.yaml, settings schema, chart values, events copies, README
deploy/equity/                    Dockerfile fragment, README
schemas/capabilities/equity.v1.json
schemas/events/equity.*.v1.json   + testdata/ fixtures
specs/018-equity/                 this directory
docs/equity.md
test/contract/equity_test.go
```

## Design decisions

1. **One policy function** and **one SoD function.** Facts (is this the caller's own record) are resolved
   from the store; the policy only decides. Managers have no more rights than employees.
2. **404 before 403** for records, so existence is not confirmed.
3. **Audit before data**, per record, atomically (`RecordBatch`); a failing auditor means no data.
4. **Database-level guarantees** in `002_immutability.sql`: append-only ledger, audit, milestones,
   acceleration events and valuations; frozen grant terms; lifecycle transitions; SoD belt on approval and
   exercise chains.
5. **Quantities as integers of 1/10000 share**, math/big for products and divisions; caps keep every
   intermediate in range; cost rounds half away from zero; ownership by largest remainder.
6. **Cap table as of a date** derives from dated ledger rows and grant approval, termination and terminal
   dates, not from a mutable balance table.
7. **Erasure pseudonymises**; the ledger refers to opaque ids so it survives; tombstones use the stakeholder
   id as subject (matches the E08 ledger pattern).
8. **Approval engine as an interface** (`ApprovalEngine`), optional; default is the board role in-service.
9. **Idempotency keys store only resource ids**; replays re-read under current rights; erasure deletes them.
10. **One SQLite connection** serialises writes (pool and balance checks cannot race) and keeps RAM low.

## Complexity Tracking

| Addition | Why | Simpler alternative rejected because |
|---|---|---|
| Triggers duplicating service rules | The ledger and approval chain are the legal record; a service bug must not break them | Service-only checks leave a single point of failure |
| Hash-chained audit table in the module | The hook must work before can-core's audit accepts module events | A plain log is easier to edit silently; the hook lets can-core replace it |
| Round model with post-money SAFE iteration | SAFEs are the common instrument | Ignoring post-money SAFEs would give wrong dilution; the model stays labelled illustrative |
| Grant terms stored unsealed | Pool and dilution queries and trigger immutability need them in the clear | Sealing them needs a derived plaintext index; recorded as a gap |

## Constitution Check (after design)

No principle is violated; no new dependency. Remaining risks: Principle III integration tests on k3d, the
measured footprint, and the gaps in `tasks.md`.
