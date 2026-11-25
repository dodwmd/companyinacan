---

description: "Task list for Epic E15a: Finance basics, first slice"
---

# Tasks: Finance basics, first slice (Epic E15a)

**Input**: `/specs/014-finance-basics/` (spec, plan, research, data-model, contracts)

**Tests**: Required (Constitution III). See plan.md for the honest account of what was written first.

## Phase 1: Setup and pure rules

- [x] T001 Create `core/finance/`, `cmd/can-finance/`, `modules/finance/`, `deploy/finance/`, this directory; no new dependency
- [x] T002 Tests (seen red) then `money.go`: minor units, rounding, FX, currency table (`money_test.go`)
- [x] T003 `migrations/001_finance.sql`: tables, constraints, immutability triggers, retention gate

## Phase 2: Store and rules

- [x] T004 Parties, tax rates, products, erasure with legal retention (`parties.go`)
- [x] T005 Invoices: pricing, drafts, approval with gap-free numbers, send, void, credit notes (`invoices.go`)
- [x] T006 Payments, import, provider webhook, adapters and fakes (`payments.go`, `providers.go`)
- [x] T007 Aging, recurring, HTML and signed links, CSV exports (`reports.go`, `recurring.go`, `render.go`, `export.go`)
- [x] T008 Contracts: templates, versions, signing, verify, reminders (`contracts.go`)
- [x] T009 Timesheets, locks, billing; expenses; purchase orders (`timesheets.go`, `expenses.go`, `purchasing.go`)
- [x] T010 Retention purge (`retention.go`); tokens, auth, idempotency, events outbox

## Phase 3: API, wiring, contracts

- [x] T011 HTTP surface, guard, throttle, strict JSON, ETag (`api*.go`, `httputil.go`), app and entry point
- [x] T012 Tests: lifecycle, numbering, FX, payments, void and credit notes, validation (`invoices_test.go`)
- [x] T013 Tests: scoping and IDOR, throttle, audit, own-only, public link, CSV injection, erasure and WAL, purge (`security_test.go`)
- [x] T014 Tests: timesheets, billing, locks, expenses, contracts, providers, aging, recurring, purchase orders (`workflow_test.go`)
- [x] T015 Capability schemas, event schemas, fixtures, manifest, chart values, image fragment, OpenAPI, names, event catalogue
- [x] T016 Contract tests (`test/contract/finance_test.go`), including a live instance against a signed webhook receiver
- [x] T017 `docs/finance.md`; footprint measured (research R8); mutation checks that the tests bite

## QA round 1 (PR #21)

- [x] Q1 purchase-order and credit-note counters reserved (invoices cannot use `PO` or `CN`)
- [x] Q2 credit notes capped at the unpaid balance
- [x] Q3 timesheet updated_by and submitted_by join the separation-of-duties check
- [x] Q4 erasure deletes draft contracts and versions; purge reports erased parties it could not delete
- [x] Q5 generated invoices carry the schedule creator as creator
- [x] Q6-Q14 subject normalisation, void maker-checker, throttle comment, provider webhook acks, public view fields, lock reopening, `FuzzCalcLine` and a sum-of-lines invariant test, documented idempotency TTL, backdating and no year rollover

## Known gaps (not hidden)

- [ ] G01 Image build and cluster checks: the Dockerfile is a fragment with digest placeholders; no Helm templates; module status stays `experimental`
- [ ] G02 Web UI (admin UI, E12) and customer portal; PDF and structured e-invoices (Factur-X, Peppol)
- [ ] G03 Real adapters: Stripe (payments) and Documenso (e-signature); only fakes exist, so no real money or signature moves
- [ ] G04 Bills and payables, three-way matching, dunning reminders, bank feeds, accounting and payroll adapters, subscriptions with usage billing
- [ ] G05 can-core integration: token introspection (the `Authenticator` seam is ready), secret injection, routing only `/public/invoices/`, the `approvals` engine and `directory` reporting-line approvers (the `Approver` seam is ready), notifications for reminders
- [ ] G06 `can finance` CLI and CI footprint job for this module
- [ ] G07 Receipt file storage (needs `files`); metadata only today
- [ ] G08 Tax-inclusive pricing, rounding at document level, withholding and reverse-charge flows; only tax-exclusive, per-line rounding exists
- [ ] G09 Per-line product price conversion between currencies and automatic FX rate lookup; rates are supplied by the caller
- [ ] G10 Test-first was strict only for the pure money rules; the rest followed the code (plan.md Constitution III)
