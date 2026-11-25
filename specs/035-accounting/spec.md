# Feature Specification: Accounting interfaces for the in-house finance module (Epic E15c)

**Feature Branch**: `035-accounting` (git branch `epic/E15c-accounting`)

**Created**: 2026-11-25

**Status**: Implemented offline (ledger, bank import, adapters, API, minimal UI, tests, docs); never run against a real
bank or accounting service, never on a cluster; image not rebuilt

**Input**: Epic E15 (`docs/epics/README.md`), SPEC.md 3.3 (finance), 4.3 (events), 5 (wrap versus build: "integrate, do not
build full accounting engines"), 6.1 (footprint). Builds on `specs/014-finance-basics` (parties, invoices, payments,
immutability triggers, gap-free numbering, separation of duties, retention) and `specs/031-finance-ui` (UI conventions).
**A tool, not accounting, tax or legal advice.**

## Scope of this slice

In: a chart of accounts (types asset, liability, equity, income, expense; codes; tax-code mapping; active flag; immutable
once used) with a minimal default template shipped as data; a double-entry journal posted by the module's own events inside
the same transaction (invoice approved, credit note approved, invoice voided, payment recorded, expense approved and
reimbursed), with configurable, validated account mappings; manual journals approved by a second person; reversals; trial
balance, general ledger, profit and loss and balance sheet as of a date; period close and reopen; bank accounts with sealed
numbers; statement import (CSV with explicit configuration, OFX/QFX 1.x, CAMT.053); a suggestion-only matching engine whose
confirmation records a payment through the existing payment path; an unmatched queue; reconciliation status and sign-off per
month; the `AccountingExporter` interface with a generic CSV journal export (cursor, idempotent batches, audited) and
documented stubs for `xero`, `quickbooks` and `wave`; the `PayrollAdapter` interface with an inbound CSV payroll journal
import; API (`/v1/ledger/*`, `/v1/bank/*`), minimal UI pages, events `journal.posted.v1` and `bank.statement.imported.v1`;
a migration for existing finance databases; manifest permissions `finance.ledger.*`; docs with disclaimers and limits.

Out (recorded in tasks.md as gaps): real Xero, QuickBooks, Wave or bank connections; payroll calculation; bank feeds and MT940;
accounts payable and bills (purchase orders do not post); multi-currency revaluation and conversion; tax calculation and
filing; year-end closing entries; budgets, cash flow, multi-entity ledgers; back-filling documents approved before the
mappings were set (a cut-over opening entry carries what they still owe); a UI for chart editing, manual journals, closing and uploads (API only); archiving old journal entries;
cluster and image checks.

## User Scenarios & Testing

### User Story 1 - Every event leaves a balanced, permanent trail (Priority: P1)

An accountant maps six accounts once. From then on approving an invoice, recording a payment or approving an expense posts a
journal entry in the same transaction; the entry equals the document to the minor unit; it can never be edited, only
reversed; a closed period takes nothing new.

**Independent Test**: `ledger_property_test.go` (every posted entry balances per currency over random entries; a reversal nets
every account to zero; the triggers refuse an unbalanced, edited or deleted entry; invoice rounding never leaks a minor unit),
`ledger_api_test.go` (posting flows, period close and audited reopen, mapping validation).

### User Story 2 - A manual journal needs a second person (Priority: P1)

A bookkeeper drafts an adjustment; a different person with the approve permission posts it; the drafter cannot, even holding
both permissions. A mistake is corrected by a reversal.

**Independent Test**: `TestManualJournalNeedsAnotherApprover`, `TestPayrollImportMakesDraftsOnly`.

### User Story 3 - Bank statements reconcile without surprises (Priority: P1)

An accountant uploads a statement; duplicates are recognised; the queue suggests invoices; confirming records the payment and
posts it to the bank's ledger account; ignoring needs a reason; a month with nothing unmatched is signed off.

**Independent Test**: `bank_test.go` (import, dedup, suggestions are advice, confirm and ignore, finality, reconciliation,
retention), `ui_ledger_test.go` (the forms).

### User Story 4 - Hostile files and limited tokens are harmless (Priority: P1)

A statement with a DOCTYPE, an entity bomb, a formula cell, a billion rows or a 2 GiB body is refused cheaply; a token limited
to some customers or projects, or an employee's own-only token, sees nothing of the journal or the bank.

**Independent Test**: `TestOFXParser`, `TestCAMT053ParserRefusesXXEAndBillionLaughs`, `TestParsersAreLinear`, the fuzz
targets, `TestLedgerRoutesRefuseScopedAndOwnOnlyTokens`, `TestBankReadsFailClosedWhenTheAuditCannotBeWritten`.

### User Story 5 - Hand the journal to the accounting system (Priority: P2)

An accountant exports new entries as CSV; running it again exports nothing; a retry with the same key returns the same batch;
the file downloads byte for byte; the named products answer "not implemented".

**Independent Test**: `TestJournalExportIsIdempotentAndSafe`.

## Requirements

- **FR-001** Amounts are int64 minor units; no float anywhere (statement parsers included).
- **FR-002** Every posted entry has at least two lines, each a debit or a credit, and balances in its one currency; checked in the
  store and by a trigger at the draft-to-posted update; entries cannot be inserted already posted.
- **FR-003** Posted entries and their lines are immutable; the only later change is the one-time link to the reversing entry;
  deletion is refused before retention. Corrections are reversals (once per entry; a reversal is not reversible).
- **FR-004** Accounts used by a posting keep code, type and tax code; accounts cannot be deleted; a mapped account cannot be deactivated.
- **FR-005** Automatic postings happen in the event's transaction and fail the event if they cannot be made; one posting per event (unique index).
- **FR-006** Numbering of entries is gap-free (`JE-00001`), taken when the entry posts.
- **FR-007** A closed period takes no entry; the close only moves forward; reopening needs `finance.ledger.admin` and a reason and is audited; the history is append-only.
- **FR-008** Manual and payroll journals are posted by a person other than the drafter, through `authorizeApproval`.
- **FR-009** `finance.ledger.*` routes and pages accept only unscoped, not own-only tokens; one function decides for API and UI.
- **FR-010** Bank account numbers are sealed with the row as additional data, shown only as `****last4`, absent from audit and events.
- **FR-011** Statement files: size, row, field and depth caps; strict parsers; no entity expansion; one linear pass; explicit CSV configuration; dedup by (account, bank id or content hash).
- **FR-012** Matching suggests; confirming records a payment through `recordPaymentTx`; nothing posts without a confirmation; decided transactions are final.
- **FR-013** Reads of bank data are audited before they are returned and fail closed; writes audit in the same transaction.
- **FR-014** Exports are batches with a cursor, stored content, a hash, an audit entry and idempotency; stubs do nothing.
- **FR-015** Accounting records survive party erasure; decided bank transactions are purged with their retention period; unmatched ones never.
- **FR-016** Events carry ids, numbers, counts and minor-unit totals only.
