# Data model: Finance basics (Epic E15a)

One SQLite file (`/data/finance.db`), migration `core/finance/migrations/001_finance.sql`. Timestamps are unix seconds (UTC), dates are
`YYYY-MM-DD` text, every amount is an integer count of the currency's minor unit.

| Table | Purpose and key rules |
|---|---|
| `parties` | Customers, vendors or both. Contact data (personal): name, contact name, email, phone, address, tax id, notes. `erased_at` marks a pseudonymised party. `version` is the ETag. |
| `tax_rates` | `code`, `name`, `rate_ppm` (0 to 1,000,000), `active`. Data, not code. |
| `products` | Catalogue: `sku`, price in minor units, currency, default `tax_code`. |
| `series` | `code`, `next_number`. Incremented inside the approving transaction: gap-free. Series used: `INV`, `CN`, `PO`, or the one named on create. |
| `invoices` | Invoices and credit notes. `status` draft, approved, sent, partially_paid, paid, void. `number` is NULL exactly while draft (CHECK) and unique per series. Currency, `base_currency`, `fx_rate_e8` (stored per document), totals, `base_total_minor`, `paid_minor`, `credited_minor`, `party_snapshot` (name, address, tax id frozen at approval), `original_id` (credit notes), `project` (opaque key from `work`), `link_epoch` (public link revocation), `recurring_id` + `recurring_period` (unique), creator, last editor, approver. Triggers refuse changes to content columns once not draft, and deletion. |
| `invoice_lines` | Position, description, SKU, `qty_milli`, unit price, `discount_bp`, tax code and the `tax_rate_ppm` copied when saved, gross, discount, tax and total in minor units. Triggers freeze them after approval. |
| `payments` | Append-only (triggers). `source` manual, import or provider; `external_id` unique per source, which de-duplicates imports and webhooks. |
| `recurring` | Schedule: party, lines as JSON inputs, interval, `next_date`, `end_date`, status. |
| `contract_templates` | Body with `{{variables}}` and the variable list. |
| `contracts` | Party, status draft, sent, signed, active, expired, terminated; current version; start, end and renewal dates; `reminded_for` (the renewal date already reminded); e-signature provider and external id; `signed_sha256`. |
| `contract_versions` | Immutable (triggers): rendered body, variables, `sha256`. |
| `timesheets` | Owner (opaque person id) and the Monday `week_start`, unique together; status draft, submitted, approved, rejected; decider and reason. |
| `time_entries` | Day, project and task keys (opaque), minutes 1 to 1440, `billable`, `invoice_id` once billed. |
| `period_locks` | Lock-through dates; the latest wins. |
| `expenses` | Owner, amount, currency, category, date, project, receipt metadata (name, type, size, SHA-256; no bytes), status draft to reimbursed. |
| `purchase_orders` | Vendor, priced lines as JSON, totals, status draft, approved, received, cancelled; numbered `PO-` at approval. |
| `idempotency` | `(subject, key)` with the request hash and the stored response; purged after 7 days. |
| `api_tokens` | SHA-256 of the secret, scopes, `parties`, `projects`, `own_only`, expiry, revocation. |
| `audit_log` | Append-only (triggers): admin actions, approvals, exports, personal-data reads. Never secrets or contact data. |
| `outbox` | Events queued in the same transaction as the change; delivered signed, dead-lettered after 7 days. |
| `retention_gate` | One row opened by the purge for its own transaction (and naming the audit cut-off). |

## State machines

Invoice: draft -> approved (number, snapshot) -> sent -> partially_paid -> paid; approved or sent -> void (no payments or credit notes).
A credit note approval adds to `credited_minor` of its invoice and settles it (`paid`) when the balance reaches zero.
Contract: draft -> sent -> signed -> active -> expired | terminated (a new version while sent returns it to draft).
Timesheet: draft -> submitted -> approved | rejected -> (edit) draft. Expense: draft -> submitted -> approved | rejected; approved -> reimbursed.
Purchase order: draft -> approved -> received; draft or approved -> cancelled.

## Retention and erasure by class

| Class | Retention | On erasure of the party |
|---|---|---|
| finance-party-contact | legal | pseudonymised immediately; row deleted when no record refers to it and retention has ended |
| finance-invoice (invoices, credit notes, payments, snapshot) | 7y from issue date | kept until then |
| finance-contract | 7y from end | sent or signed ones kept; drafts deleted with the party's drafts |
| finance-timesheet, finance-expense | 7y from week or expense date | not party data |
| finance-purchase-order | 7y from creation | approved ones kept |
| finance-audit | 7y (at least 365 days) | entries hold ids only |
