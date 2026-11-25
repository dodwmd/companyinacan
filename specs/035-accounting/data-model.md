# Data model: Accounting interfaces (E15c)

Migration `core/finance/migrations/003_accounting.sql` (after `001_finance.sql` and `002_core_approvals.sql`; the existing migrator applies it to
existing databases in a transaction and records version 3). Dates are `YYYY-MM-DD` text, timestamps unix seconds, amounts integer minor units.

| Table | Purpose | Guards (triggers unless noted) |
|---|---|---|
| `ledger_accounts` | chart: `code` unique, `name`, `type` (asset, liability, equity, income, expense), `tax_code`, `active`, `used`, `version` | `used` is set by the first line; `code`, `type`, `tax_code` cannot change and the row cannot be deleted once used |
| `ledger_mappings` | `key` (ar, cash, income, tax_payable, expense, expense_payable, opening_equity) to an account | store validates existence, active, type; the account cannot be deactivated while mapped (store) |
| `ledger_closes` | append-only close and reopen history; the latest row is in force | no update, no delete before retention |
| `journal_entries` | `status` draft or posted, `source`, `source_ref`, `entry_date`, `currency`, `memo`, `number`/`number_text` (`JE-00001`, series `JE`), `reversal_of`, `reversed_by`, who and when | insert only as draft; draft-to-posted update checks 2+ lines, balance, open period; posted rows are immutable except `reversed_by` set once; posted rows cannot be deleted before retention; unique `(source, source_ref)` (one posting per event) and unique `reversal_of` |
| `journal_lines` | `entry_id`, `position`, `account_id`, `debit_minor`, `credit_minor`, `memo` | `CHECK` exactly one of debit and credit is positive; no insert, update or delete once the entry is posted |
| `bank_accounts` | `name`, `currency`, `ledger_account_id`, `sealed_number`, `last4`, `active` | store: sealed with AAD `finance.bank_accounts.sealed_number:<id>`; never returned |
| `bank_statements` | one imported file: `format`, `sha256` (unique per account), `size_bytes`, counts, who and when | the same file is a no-op |
| `bank_transactions` | `txn_key` (unique per account), `booked_on`, signed `amount_minor` (money in positive), `currency`, `reference`, `counterparty`, `description`, `status` (unmatched, matched, ignored), `matched_payment_id`, who and when, `ignore_reason` | what the bank reported cannot change; a matched or ignored row is final; no delete before retention |
| `bank_reconciliations` | a signed-off month per bank account | unique `(bank_account_id, period)` |
| `export_cursors`, `export_batches` | last exported posting number per adapter; stored batch (`content`, `sha256`, range, count, who, when) | batches append-only |
| `payroll_imports` | SHA-256 of an imported payroll file and the drafts it made | one import per file |

**Journal sources**: `opening` (`opening:<currency>`, the cut-over), `manual`, `payroll`, `invoice`, `credit_note`, `invoice_void`, `payment`, `expense`, `expense_reimbursed`, `reversal`. `source_ref` is
`invoice:<id>`, `payment:<id>`, `expense:<id>` (empty for manual and reversal; `<sha16>:<date>:<currency>` for payroll).

**Postings**

| Event | Debit | Credit | Date |
|---|---|---|---|
| invoice approved | `ar` total | `income` (sum of line totals less tax), `tax_payable` or the liability account carrying the line's `tax_code` | issue date |
| credit note approved | `income`, tax accounts | `ar` | issue date |
| invoice voided | the mirror of its entry | | today (not before the entry) |
| payment recorded | the bank account's ledger account (else `cash`) | `ar` | `received_on` |
| expense approved | `expense` | `expense_payable` | `incurred_on` |
| expense reimbursed | `expense_payable` | `cash` | today (not before the expense) |
| cut-over (mappings first complete), one entry per currency | `ar`: open invoice balances | `expense_payable`: approved unreimbursed claims; `opening_equity`: the difference (debited when negative) | that day |

`finance.ledger.*` permissions and roles are in `modules/finance/module.yaml`; data classes `finance-ledger` and `finance-bank` likewise. The
existing `audit_log`, `idempotency`, `outbox`, `series` and `retention_gate` tables are reused (series `JE` is reserved so an invoice cannot use it).
