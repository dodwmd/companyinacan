# API contract: accounting (E15c)

The machine-readable contract is the existing finance OpenAPI document, extended:
`specs/014-finance-basics/contracts/openapi.yaml` (`/v1/ledger/*`, `/v1/bank/*`). `TestFinanceOpenAPIMatchesRouter` fails when a route in
`core/finance/api.go` is missing from it or the reverse. The UI routes are in `specs/031-finance-ui/contracts/ui-routes.md`
(rows marked E15c), checked by `TestUIRoutesMatchContract`.

## Scopes

`finance.ledger.read`, `.write`, `.approve`, `.admin` (see `docs/accounting.md`). Every route below needs an unscoped, not own-only token (`403` otherwise,
whatever the permission, and the same `403` for an id that exists or not).

## Routes

| Route | Scope | Notes |
|---|---|---|
| `GET /v1/ledger/accounts` | read | `include_inactive=true` |
| `POST /v1/ledger/accounts` | admin | `{code, name, type, tax_code}` |
| `POST /v1/ledger/accounts/template` | admin | `{apply_mappings}`; safe to repeat |
| `PATCH /v1/ledger/accounts/{id}` | admin | `If-Match`; code, type and tax code only while unused |
| `GET`, `PUT /v1/ledger/mappings` | read, admin | `{mappings: {key: account_code}}` |
| `GET /v1/ledger/journals`, `GET .../{id}` | read | filters `status`, `source`, `from`, `to`, `cursor` |
| `POST /v1/ledger/journals` | write | `Idempotency-Key`; draft; `{entry_date, currency, memo, lines: [{account_code, debit_minor, credit_minor, memo}]}` |
| `DELETE /v1/ledger/journals/{id}` | write | drafts only |
| `POST /v1/ledger/journals/{id}/post` | approve | not the drafter; open period |
| `POST /v1/ledger/journals/{id}/reverse` | approve | `{date, reason}`; once per entry; manual and payroll journals only; not its drafter or poster |
| `GET /v1/ledger/reports/{trial-balance,balance-sheet}` | read | `as_of` |
| `GET /v1/ledger/reports/profit-loss` | read | `from`, `to` |
| `GET /v1/ledger/reports/general-ledger` | read | `account` (code), `from`, `to`, `cursor`, `limit` |
| `GET /v1/ledger/periods`, `POST .../close`, `POST .../reopen` | read, admin, admin | `{closed_through}`; reopen also `{reason}` |
| `GET`, `POST /v1/ledger/exports`, `GET .../{id}/download` | read, write, read | `{adapter}`; `501` for xero, quickbooks, wave |
| `POST /v1/ledger/payroll-import?adapter=csv` | write | body is the file |
| `GET`, `POST /v1/bank/accounts`, `GET .../{id}` | read, admin, read | create `{name, currency, number, ledger_account_code}`; `503` without the seal key |
| `POST /v1/bank/accounts/{id}/statements` | write | body is the file; query `format` and, for csv, `delimiter`, `date_col`, `amount_col` (or `debit_col`, `credit_col`), `reference_col`, `description_col`, `counterparty_col`, `id_col`, `date_layout`, `decimal`, `thousands`, `header`; `force_duplicates=true` imports equal rows that have no bank id; the answer lists skipped rows |
| `GET`, `POST /v1/bank/accounts/{id}/reconciliation` | read, approve | `{period: "YYYY-MM"}` |
| `GET /v1/bank/transactions` | read | `account_id`, `status`, `from`, `to`, `cursor`; audited |
| `GET /v1/bank/transactions/{id}/suggestions` | read | advice only; audited |
| `POST /v1/bank/transactions/{id}/confirm` | write (+ `finance.write`) | `{invoice_id, payment_id?, record_new?}`; `Idempotency-Key` |
| `POST /v1/bank/transactions/{id}/ignore` | write | `{reason}` |

## Errors

The finance conventions: `400` malformed, `401`, `403` scope or separation of duties (`separation_of_duties`), `404`, `409` conflict (`period_locked` for a closed ledger), `412`, `413` body over
`max_body_bytes`, `422` invalid fields (a rejected statement file is `422` with `file` and a message that never quotes the file or an account number), `429`, `501` adapter stub, `503` not configured or timed out.
