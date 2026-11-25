# Feature Specification: Finance basics, first slice (Epic E15a)

**Feature Branch**: `014-finance-basics` (git branch `epic/E15a-finance`)

**Created**: 2026-11-25

**Status**: Implemented offline (service, API, contracts, tests, docs); web UI, image build, real payment and
e-signature adapters and cluster checks pending

**Input**: Epic E15, first slice: in-house `invoices`, `contracts`, timesheets, expenses and vendors/purchase orders.
SPEC.md sections 3.3 (invoices, timesheets, contracts, expenses, vendors), 3.7 (approvals, retention, erasure),
4.3 (capabilities, events), 5 (licence check, wrap versus build), 6.1 (footprint). Tool, not accounting, tax or legal advice.

## Scope of this slice

In: `cmd/can-finance` and `core/finance`; parties (customers and vendors), a product catalogue and tax rates as data;
invoices and credit notes (draft, approved, sent, partially paid, paid, void) with gap-free numbering per series and
immutability after approval; integer minor-unit money, multi-currency with a stored FX rate, rounding rules; payments
(manual, import, provider webhook); aging; recurring invoices; HTML rendering and a signed expiring public link;
payment-provider and e-signature adapter interfaces with fakes; contracts (templates with variables, immutable versions
with SHA-256, signing, renewal reminders); timesheets (weekly, workflow, locked periods, billing to a draft invoice);
expenses with receipt metadata; minimal purchase orders; scoped API tokens, per-party and per-project scoping,
separation of duties, optimistic concurrency, idempotency keys; six events; capability schemas `invoices.v1`,
`contracts.v1`, `timesheets.v1`; retention and erasure that respects legal retention; CSV exports with formula-injection
protection; module manifest, chart values, image fragment, OpenAPI, docs.

Out (recorded in tasks.md as gaps): web UI; PDF output and structured e-invoicing (Factur-X, Peppol); real Stripe and
Documenso adapters; bills (payables) and three-way matching; dunning emails; bank feeds; accounting and payroll adapters;
subscriptions with usage billing; the `can finance` CLI; can-core integration (token introspection, secret injection,
routing of the public link, the `approvals` engine); file storage for receipts; Helm templates; cluster and image checks.

## User Scenarios & Testing

### User Story 1 - Issue an invoice that can never be quietly changed (Priority: P1)

A bookkeeper drafts an invoice, a second person approves it, it is sent and paid. After approval nothing but its status
and payments change; a mistake is fixed with a void or a credit note, and the numbers stay gap-free.

**Independent Test**: `core/finance/invoices_test.go`: totals and rounding, separation of duties, numbering (concurrent
approvals, a failed approval), immutability at the API and through direct SQL.

**Acceptance Scenarios**:

1. **Given** a draft, **When** its creator approves it, **Then** `403 separation_of_duties`; **When** another holder of
   `finance.approve` does, **Then** it gets `INV-00001` and the buyer's name, address and tax id are frozen onto it.
2. **Given** an approved invoice, **When** anyone PATCHes or DELETEs it, **Then** `409`; direct SQL updates of its
   amounts, party, number or lines are refused by triggers.
3. **Given** 20 drafts approved concurrently, **Then** the numbers are 20 distinct consecutive values; a failed approval
   consumes none; a deleted draft leaves no gap; a voided invoice keeps its number.
4. **Given** a payment above the balance, **Then** `422`; a retry with the same idempotency key returns the first
   payment; the final payment makes the invoice `paid` and emits `invoice.paid.v1` once.

### User Story 2 - Money is exact and multi-currency is honest (Priority: P1)

All amounts are integers of the currency's minor unit; each document stores its FX rate; rounding is documented and
tested; tax rates are data.

**Acceptance Scenarios**: line rounding is half away from zero and totals are sums of rounded lines; a EUR invoice
needs a rate and stores base totals using both currencies' exponents (JPY has none, KWD three); changing a tax rate
never alters an issued invoice and re-prices drafts at approval; floats in a request are rejected by the schema.

### User Story 3 - Customers and auditors see only their slice (Priority: P1)

Tokens are limited to parties and projects, employees get own-only tokens, and no request can read or change outside
its scope.

**Acceptance Scenarios**: a party-scoped token lists and fetches only its party's invoices, contracts and payments;
filters (`party_id`, `project`, `status`) cannot widen the scope; every by-id route answers 404 for a record outside the
scope, indistinguishable from a missing one; an own-only token sees only its own timesheets and expenses and none of the
business routes; a token creator cannot grant more than it holds; a flood of bad tokens or links is throttled while
valid credentials are never throttled.

### User Story 4 - Time becomes money (Priority: P2)

A person fills in their week, a manager approves it (never their own), and approved billable hours become a draft
invoice once, with locked periods preventing late changes.

**Acceptance Scenarios**: submit and approve/reject workflow; entries validated (Monday weeks, days inside the week, at
most 24 h a day, project and task keys); `timesheet.approved.v1`; billing groups minutes per task into milli-hours, marks
entries billed, cannot bill twice, releases them when the draft is deleted or the invoice voided; a period lock freezes
sheets and expenses up to a date.

### User Story 5 - Contracts have integrity (Priority: P2)

Contracts are rendered from templates into immutable, hashed versions, sent for e-signature, and tracked to renewal.

**Acceptance Scenarios**: missing or extra variables are refused and values are not re-expanded; a signature with the
wrong document hash is refused; webhooks need a valid, fresh signature; `contract.signed.v1` is emitted once; signed text
cannot change; `verify` detects tampering; renewal reminders fire once per renewal date and re-arm when it changes;
start and end dates activate and expire contracts.

### User Story 6 - Expenses and purchase orders with approval (Priority: P3)

Expenses carry receipt metadata (no file bytes), are approved by someone else, then reimbursed; purchase orders to a
vendor are approved by someone else and numbered gap-free.

### User Story 7 - Personal data and legal retention (Priority: P1)

Erasing a customer removes what the law lets us remove and says what it keeps.

**Acceptance Scenarios**: erasure deletes drafts and schedules, pseudonymises the party, keeps issued invoices with their
snapshot, returns a report with counts and the date retention ends, and truncates the WAL (no trace of the email in the
database files); after the retention period the purge deletes the invoice and then the erased party, never an unpaid
invoice; contact reads, exports and public views are audited; the audit log is append-only.

## Requirements

- **FR-001** Money is `int64` minor units everywhere; no floats in API, storage or code (tests, schemas).
- **FR-002** Rounding is half away from zero per line; totals are sums of lines; documented in `docs/finance.md`.
- **FR-003** Numbers are assigned on approval inside the approving transaction, gap-free per series.
- **FR-004** Issued invoices, lines, payments, contract versions and audit entries are immutable at the API and the database.
- **FR-005** A record's creator, last editor or owner cannot approve it; one function enforces it for every approval kind.
- **FR-006** Scope (party, project, owner) is applied to the whole query by AND-joined, parenthesised clauses.
- **FR-007** Valid credentials are never throttled; failures are counted per client address.
- **FR-008** Reads run under a deadline; sensitive reads and exports are audited and fail closed.
- **FR-009** Creates take idempotency keys; edits take `If-Match` versions.
- **FR-010** Events carry identifiers, numbers and amounts only; schemas and fixtures are published and tested against a live instance.
- **FR-011** Retention and erasure follow `docs/finance.md`; legal retention beats erasure for issued invoices.
- **FR-012** CSV text cells are defused against formula injection.
- **FR-013** No real network call exists anywhere in the module; adapters have fakes.

## Success Criteria

- `make test` and `go test -race ./core/finance` are green; contract tests tie OpenAPI, router, schemas, fixtures,
  manifest and chart together.
- Idle RAM at most 20 MB (measured 16.1 MiB; research R8).
