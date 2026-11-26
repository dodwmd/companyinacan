# Data model: Finance web UI (E15b)

The UI adds **no table, no migration and no persisted data**. It reads and writes through the Store of
`specs/014-finance-basics`.

## In memory

| Thing | Where | Bound | Lifetime |
|---|---|---|---|
| Session `{subject, principal, token, csrf, created, last, checked}` keyed by SHA-256 of a random id | `core/uikit/session` | 5000 overall, 5 per subject (oldest dropped) | idle 30 min, absolute 8 h, logout, a failed re-check; lost on restart |
| Failed-login counter per client address | `rateLimiter` | 10 per minute, 10000 keys | one window |
| Request counter per subject | `rateLimiter` | 600 per minute | one window |
| Parsed page templates | `UI.tpl` | one per page (31) | parsed on first use |

## Read-only additions to the Store

| Method | Returns | Scope |
|---|---|---|
| `Dashboard(ctx, p, asOf)` | overdue count and balance per currency; issued and received this month per currency | party and project clauses on every query |
| `ListSeries(ctx)` | numbering series and the last number issued | none (no personal data) |
| `PartyNames(ctx, p, ids)` | `id -> name` for parties in scope | party clause; no contact data |

## Form fields (names are the contract of the templates)

| Form | Fields |
|---|---|
| Sign in | `token`, `csrf` |
| Party | `kind`, `name`, `contact_name`, `email`, `phone`, `address`, `tax_id`, `currency`, `notes`, `version` (edit) |
| Erase | `confirm` (`ERASE`) |
| Invoice and credit note | `party_id`, `currency`, `fx_rate`, `series`, `project`, `issue_date`, `due_date`, `due_days`, `notes`, `reason` (credit note), `idem` / `version`; repeated per line: `desc`, `qty`, `price`, `disc`, `tax`, `sku` |
| Payment | `amount`, `method`, `reference`, `received_on`, `idem` |
| Void, reject, terminate | `reason`, `version` |
| Public link | `ttl_hours` |
| Recurring | as the invoice plus `name`, `interval`, `start_date`, `end_date`, `due_days` |
| Contract | `party_id`, `title`, `template_id`, `var_<name>`, `start_date`, `end_date`, `renewal_date`, `reminder_days`; send: `mode`, `signers`, `version` |
| Timesheet | open: `week_start`; grid: repeated `project`, `task`, `desc`, `billable` per row and `h` (7 per row), `version` |
| Expense | `description`, `amount`, `currency`, `category`, `incurred_on`, `project`, `receipt_filename`, `receipt_type`, `receipt_size`, `receipt_sha256`, `idem` |
| Purchase order | `vendor_id`, `currency`, `project`, `notes`, line rows, `idem`; status: `status`, `version` |
