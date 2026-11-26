# Events published by the finance module

Envelope (same as every module): `{id, type, time, source: "finance", data}`; `id` is `evt_` plus 24 hex characters, stable across
redeliveries; delivery is at least once, signed with `X-Can-Signature: sha256=HMAC(secret, timestamp + "." + body)`; consumers
de-duplicate on `id`. Schemas: `schemas/events/<type>.json` (copies in `modules/finance/events/`); fixtures: `testdata/events/<type>/`.
Payloads carry identifiers, numbers and amounts only, never names, addresses, emails or descriptions. `person` is the opaque person id
used as the token subject.

| Event | `data` |
|---|---|
| `invoice.sent.v1` | `invoice_id`, `number`, `kind` (invoice or credit_note), `party_id`, `currency`, `total_minor`, `due_date`, `sent_at` |
| `invoice.paid.v1` | `invoice_id`, `number`, `party_id`, `currency`, `total_minor`, `paid_minor`, `paid_at` |
| `contract.signed.v1` | `contract_id`, `party_id`, `version`, `sha256`, `signed_at` |
| `contract.expiring.v1` | `contract_id`, `party_id`, `renewal_date`, `days_left` |
| `timesheet.approved.v1` | `timesheet_id`, `person`, `week_start`, `total_minutes`, `billable_minutes`, `approved_at` |
| `expense.approved.v1` | `expense_id`, `person`, `amount_minor`, `currency`, `category`, `approved_at` |
| `invoice.overdue.v1` | `invoice_id`, `party_id`, `due_date`, `days_overdue`; or `count` alone for a backlog |
| `contract.declined.v1` | `contract_id`, `party_id`, `version` |
| `expense.submitted.v1`, `expense.decided.v1` | `expense_id`, `owner` |
| `timesheet.submitted.v1`, `timesheet.decided.v1` | `timesheet_id`, `owner` |

SPEC.md 4.3 lists `invoice.issued.v1` and `time.approved.v1`; this module publishes `invoice.sent.v1` and `timesheet.approved.v1`
instead (an invoice is "issued" at approval, but the event consumers care about is that it left the building), and the catalogue in
`core/server/events` lists both families.

The capability contracts are `schemas/capabilities/{invoices,contracts,timesheets}.v1.json`; the full REST surface is `openapi.yaml`.
