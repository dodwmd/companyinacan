# Events published by the accounting interfaces (E15c)

Same envelope, signing and delivery as `specs/014-finance-basics/contracts/events.md`. Schemas: `schemas/events/<type>.json` (copies in
`modules/finance/events/`); fixtures: `testdata/events/<type>/` (valid and invalid); the contract test also validates what a running service sends.
Payloads carry ids, numbers, counts and minor-unit totals only: no account name, memo, party, counterparty, bank account number or transaction text.

| Event | `data` |
|---|---|
| `journal.posted.v1` | `entry_id`, `number` (`JE-00007`), `source` (manual, payroll, invoice, credit_note, invoice_void, payment, expense, expense_reimbursed, reversal), `currency`, `total_minor` (the entry's debit total), `entry_date`, `posted_at` |
| `bank.statement.imported.v1` | `statement_id`, `bank_account_id`, `format` (csv, ofx, camt053), `transaction_count`, `new_count`, `duplicate_count`, `imported_at`; not published for a file that was already imported |
