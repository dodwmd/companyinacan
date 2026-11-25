# Accounting interfaces (E15c)

The in-house `finance` module can keep a chart of accounts and a double-entry journal posted from its own
events, import bank statements and help you reconcile them, close periods, and hand a journal to your
accounting system as a file. Spec: `specs/035-accounting/`. API: `specs/014-finance-basics/contracts/openapi.yaml`
(`/v1/ledger/*`, `/v1/bank/*`). UI: the Accounting and Bank pages, read-only apart from the reconciliation forms.

**This is a tool, not accounting, tax or legal advice.** It is bookkeeping support that sits beside the
accounting system you already use (SPEC.md: integrate, do not build full accounting engines). It does not file
anything, calculate any tax, depreciate, revalue currencies, close a year, or replace your accountant's books.
Check every result with a qualified person before you rely on it. Status: experimental, built offline and tested
with fakes; never run against a real bank or accounting service and never on a cluster.

## What it does

| Area | What you get | What you do not get |
|---|---|---|
| Chart of accounts | Accounts with a code, name, type (asset, liability, equity, income, expense), an optional tax code mapping and an active flag. A minimal default chart shipped as data (`ledger_template.json`) that you can apply and edit. An account that carries a posting keeps its code, type and tax code. | Jurisdiction rules, statutory account numbering, tax treatment. The template is a starting point, not a recommendation. |
| Automatic posting | Inside the same transaction as the event: invoice approved (receivable debit, income credit, tax payable credit per tax code), credit note approved (the mirror), payment recorded (bank or cash debit, receivable credit), invoice voided (reversal), expense approved (expense debit, payable credit), expense reimbursed. Totals come from the document's own rounded lines, so an entry always equals its document. | Purchase orders are not bills and do not post. There is no accounts-payable ledger, no bank charges, no inventory, no fixed assets. |
| Account mapping | Six keys (`ar`, `cash`, `income`, `tax_payable`, `expense`, `expense_payable`) each pointing at an account. Each is validated when set (exists, active, the right type). Automatic posting is on only when all six are set, and an account that is mapped cannot be deactivated. | Per-product or per-category income accounts. Tax codes route to a liability account carrying that `tax_code`, else to `tax_payable`. |
| Manual journals | Balanced, one currency, drafted by a person and posted by another (see Separation of duties). | Recurring journals, allocations. |
| Reports | Trial balance, general ledger (paged, with running balances), profit and loss, balance sheet, as of a date, per currency, in integer minor units. Bounded queries (5 second deadline, at most 2000 rows, reported as truncated). | Comparatives, budgets, cash flow, consolidation. |
| Period close | Close the ledger through a date. Nothing dated on or before it can be posted (the database refuses too). Reopening needs `finance.ledger.admin` and a reason, and is audited; the close history is append-only. | Year-end closing entries. The balance sheet shows earnings to date instead. |
| Bank | Bank accounts with a sealed number, statement import (CSV, OFX 1.x, CAMT.053), suggestions, confirm (records a payment) and ignore (with a reason), reconciliation status per month with a sign-off. | Bank feeds, payment initiation, MT940, foreign-currency accounts converting to your base currency, outgoing payments or bill matching. Matching is for money received against invoices. |
| Adapters | `AccountingExporter` interface with a generic CSV journal export (cursor, idempotent batches, audited). `xero`, `quickbooks` and `wave` are documented stubs. `PayrollAdapter` interface with a CSV payroll journal import. | A working Xero, QuickBooks or Wave connection. Payroll calculation. |

## Money and currencies

Every amount is an integer count of the currency's minor unit (`amount_minor`); there is no floating point
anywhere in this feature, including the statement parsers, which read decimal text straight into integers and
refuse more decimals than the currency has (they never round). A journal entry is in one currency: it balances
when debits equal credits in minor units. Reports are per currency and **never converted**: there is no
revaluation, no realised or unrealised exchange difference, and the FX rate stored on an invoice is not used
by the ledger. If you trade in more than one currency, the multi-currency treatment is your accountant's job.

## Corrections: reversal, never editing

A posted entry is immutable (database triggers, not just code). To correct one, post its reversal
(`POST /v1/ledger/journals/{id}/reverse`): the mirror entry, dated in an open period, linked both ways, allowed
once per entry, and itself not reversible (post a new entry instead). Voiding an invoice posts the reversal
for you. A payment is append-only; correct a payment with a credit note or a documented adjustment journal.

## Separation of duties

One rule, `authorizeApproval`, decides every approval in the module. A manual journal is drafted with
`finance.ledger.write` and posted with `finance.ledger.approve`; the poster cannot be the drafter, however many
permissions they hold. The same applies to a payroll journal (the importer is the drafter) and to a reversal
(the reverser is not the creator of the entry). Signing off a bank reconciliation month needs `finance.ledger.approve`.

## Permissions and scopes

| Permission | Allows |
|---|---|
| `finance.ledger.read` | Chart, mappings, journal, reports, periods, export list and downloads, bank accounts, transactions, suggestions, reconciliation |
| `finance.ledger.write` | Draft a manual journal, delete a draft, export, import a payroll file, import a statement, confirm or ignore a bank transaction (confirming also needs `finance.write`) |
| `finance.ledger.approve` | Post a manual journal, reverse an entry, sign off a reconciliation month |
| `finance.ledger.admin` | Add and edit accounts, apply the template, set mappings, add bank accounts, close and reopen periods (implies read and write, not approve) |

`finance.admin` implies all of them. **Only an unscoped, not own-only token can use any of these**: a token
limited to some parties or projects, an employee's own-only token, or a token without the permission gets 403
on every accounting route, because the journal, the reports and the bank data span every customer and project
(one function, `allowedBy`, decides this for the API and the UI). Suggested roles in the manifest: `accountant`,
`controller`, `ledger-auditor`. An unknown id is a 404 for a caller who may ask.

## Bank accounts, statements and matching

**Account numbers are sealed.** XChaCha20-Poly1305 with the row id as additional data (a value copied to another
row does not open), key from `CAN_FINANCE_SEAL_KEY` (64 hex characters; can-core generates it once and never
rotates it, because a new key could not open what the old one sealed). Without the key, adding a bank account
answers 503. The API and the UI show only `****` and the last four characters; the audit log and events never
hold the number. A statement that names its account (OFX `ACCTID`, CAMT IBAN) must match the bank account it is
imported into; the error says nothing about either number.

**Statement files are untrusted.** Limits: 2 MiB and 5000 transactions per file (and the API's
`max_body_bytes`, 1 MiB by default: raise it to import larger files), 1000 bytes per field, nesting 16 (OFX) or 32
(CAMT). Everything is one linear pass.

- **CSV** (RFC 4180, `encoding/csv`): nothing is guessed. You give the delimiter (`comma`, `semicolon`, `tab`,
  `pipe`), the column numbers (`date_col`, `amount_col` or `debit_col` and `credit_col`, optional `reference_col`,
  `description_col`, `counterparty_col`, `id_col`), `date_layout` (a Go layout, default `2006-01-02`),
  `decimal` (`dot` or `comma`), `thousands` and `header`. A bare quote, a short row, a bad date or a bad amount
  rejects the whole file. Text cells are stored as text; every CSV and HTML output escapes them (a cell
  starting with `=`, `+`, `-` or `@` is defused in exports).
- **OFX / QFX 1.x** (SGML subset): bank statements, tags with or without closing tags. Any `<!` (document type or
  entity declaration) rejects the file; only the five predefined entities are read and are never expanded again.
- **CAMT.053** (`encoding/xml`, strict, UTF-8 only): a document type declaration is refused before parsing, so there
  is no entity to expand (no XXE, no billion laughs).
- **MT940** is not supported.

**Deduplication.** A transaction is unique per bank account by the bank's own id (`FITID`, `AcctSvcrRef`, the CSV
`id_col`; CAMT `NtryRef` is not used because banks restart it in every message) or, when the file has none, by a hash of its content plus its occurrence number in the file. The same
file twice is recognised by its SHA-256 and changes nothing; an overlapping file adds only the new transactions.

**Matching only suggests.** For an incoming transaction the engine proposes open invoices in the same currency
whose balance equals the amount, or whose number appears in the reference (a partial payment). Nothing is recorded
until a person confirms. Confirming records a payment through the same code path as any payment (so the currency,
the no-overpayment rule, the invoice status, its event and its journal entry all apply) and the payment's debit goes to
the ledger account of the bank account the money arrived in. Money out cannot be matched to an invoice; ignore it
with a reason. A decided transaction cannot be changed. A reconciliation month can be signed off only when nothing
in it is unmatched; a later import that brings new unmatched rows shows the month open again.

**Reads of bank data are audited before they are returned**, and fail closed: if the audit entry cannot be
written, the request fails and nothing is shown.

## Adapters

`AccountingExporter` (`core/finance/adapters.go`): `Name`, `Implemented`, `Export(entries, w)`. The `csv` adapter
writes one row per journal line: `entry_number, entry_date, source, source_ref, memo, currency, line, account_code,
account_name, debit, credit, line_memo` (amounts as plain decimals, text defused against formula injection).

Export is a batch with a **cursor**: `POST /v1/ledger/exports {"adapter":"csv"}` takes, in posting order, the posted entries after the
adapter's last exported posting number (at most 500), stores the file, moves the cursor and writes the audit entry in one
transaction. Running it again exports nothing new; with an `Idempotency-Key` a retry returns the same batch;
`GET /v1/ledger/exports/{id}/download` returns the same bytes (audited). `xero`, `quickbooks` and `wave` answer `501 not_implemented`,
change nothing and make no network call. They are registered so that the interface is real; a working adapter is a separate piece of work
that would belong in its own module with its own credentials, not in this one.

`PayrollAdapter`: inbound only. `POST /v1/ledger/payroll-import?adapter=csv` with a file whose header is exactly
`date,account_code,debit,credit,currency,memo` creates one draft journal per date and currency, each of which must
balance. The drafts post only when someone else approves them. The same file imports once.
**No payroll is calculated here.** The file is whatever your payroll provider produced; this module checks that it balances and nothing
else. Verify payroll, tax and super with your payroll provider and your accountant.

## Events

`journal.posted.v1` (`entry_id`, `number`, `source`, `currency`, `total_minor`, `entry_date`, `posted_at`) and
`bank.statement.imported.v1` (`statement_id`, `bank_account_id`, `format`, the three counts, `imported_at`).
Ids, numbers, counts and minor-unit totals only: no account names, memo, party, counterparty or bank data.

## Retention and erasure

Accounting records are legal records: **erasing a customer does not touch them**, and the retention purge does not delete journal
entries (they hold no personal data, and removing old ones would change every later balance; archiving is a
separate decision for you and your accountant). Data classes in the manifest: `finance-ledger` (7 years, standard) and
`finance-bank` (7 years, sensitive). Decided bank transactions (matched or ignored) are purged with the invoices once their booking date is past
`retention_years`; unmatched transactions are open work and are never purged. Export batches and the close history are kept. The audit log follows
`audit_retention_days`. This is not legal advice: set `retention_years` for your jurisdiction.

## Operating it

1. Add `CAN_FINANCE_SEAL_KEY` (can-core generates it for new installs; for an existing install add it to the module Secret once and keep it).
2. As `finance.ledger.admin`: `POST /v1/ledger/accounts/template` with `{"apply_mappings": true}`, review the chart and the mapping, adjust.
3. From that moment finance events post automatically. Approvals that would post into a closed period fail with `period_locked`:
   open the period (audited) or date the document in an open one. Documents approved before the mappings were set are not back-filled.
4. Add a bank account, import statements, work the queue (UI: Bank), sign off months, close periods, export the journal.

Limits worth knowing: one currency per entry and no conversion; single ledger (no entities or tracking categories); the journal
list is paged 50 at a time; the general ledger recomputes the opening balance per page.
