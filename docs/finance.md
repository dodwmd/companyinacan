# Finance

`finance` is the in-house module for the money side of a small company: customers and vendors,
invoices and credit notes, payments, receivables aging, recurring invoices, contracts with
e-signature, timesheets, expenses and minimal purchase orders. It is a service (`cmd/can-finance`,
library `core/finance`) with a REST API and SQLite storage, and it backs the `invoices`,
`contracts` and `timesheets` capabilities.

> **This is a tool, not accounting, tax or legal advice.** Tax rates, numbering rules, retention
> periods and what an invoice must contain differ by country. The module gives you the mechanics
> (data-driven tax, gap-free numbers, immutability, retention settings); whether they satisfy your
> jurisdiction is for you and your accountant to decide. It is not a general ledger, does not do
> payroll, and does not file returns.

Status: **experimental**. The service is built and tested offline and its idle memory is measured;
the image has not been built or run on a cluster, the web UI ([`finance-ui.md`](finance-ui.md)) is
built offline and not browser-verified, and the payment and
e-signature adapters are fakes (the real ones are separate, later work). See
`specs/014-finance-basics/tasks.md` for the known gaps.

## Build versus wrap

SPEC.md section 5 says to wrap an OSS invoicer when one passes the licence and footprint check, and
otherwise to build lean. Invoice Ninja and Akaunting do not pass the OSI rule (source-available
terms), and the module has to share one party model with contracts, timesheets and expenses, hold
personal data under per-record scoping, enforce separation of duties and audit reads. So the first
slice is in-house and small (measured about 16 MiB idle). The `invoices` capability is a contract,
not an implementation: an adapter in front of a wrapped invoicer can replace the module later as
long as it keeps the same immutability rules.

## Money rules

* Every amount is an integer count of the currency's **minor unit** (cents, pence, yen, and
  thousandths for KWD and BHD). There are no floats anywhere, in the API, the database or the code.
  The exponent comes from a built-in ISO 4217 table.
* Quantities are milli-units (`1500` is 1.5), discounts are basis points (`1000` is 10%), tax rates
  are parts per million (`200000` is 20%), FX rates are decimal strings with at most 8 decimals.
* **Rounding**: every division rounds half away from zero (0.5 becomes 1, -0.5 becomes -1). A line
  is `gross = round(quantity x unit price)`, `discount = round(gross x discount)`,
  `tax = round((gross - discount) x rate)`. Each line is rounded on its own and the document totals
  are the **sums of the rounded lines**, so a printed invoice always adds up. Prices are
  tax-exclusive. Minutes on a timesheet are summed per task first and converted to milli-hours once.
* **Multi-currency**: a document has one currency. The company has a base currency
  (`base_currency`, default USD). A document in another currency must carry its **FX rate**
  (base units per one unit of the document currency, for example `1.0850`), which is stored on the
  document and never looked up again; `base_total_minor` is converted with the exponents of both
  currencies. Payments are recorded in the invoice's currency. Reports never add currencies together.
* **Tax is data**: `POST /v1/tax-rates` (admin) defines codes such as `VAT20`. Nothing in the code
  knows any jurisdiction. A line copies the rate when it is saved; a draft is re-priced with the
  current rates when it is approved; an issued invoice never changes when a rate does.

## Invoices

`draft` -> `approved` -> `sent` -> `partially_paid` -> `paid`, or `void`. Credit notes follow the
same states from `draft`.

* **Numbers are assigned on approval**, per series (`INV-00001`, credit notes `CN-00001`, purchase
  orders `PO-00001`; set `series` on create for others). The counter moves in the same transaction
  as the approval, so a rolled-back approval gives its number back, deleting a draft leaves no
  gap, and concurrent approvals get distinct consecutive numbers. A voided invoice keeps its number.
* **Series are reserved per document class**: invoices use `INV` or a series you name; `CN` (credit
  notes) and `PO` (purchase orders) each have their own counter and an invoice cannot use them, so
  one class can never burn or collide with another's numbers. Numbers never reset: there is no
  year rollover (`INV-00001` continues into the next year), and an invoice may be **backdated**
  (`issue_date` is yours to set; the number follows approval order, not the date). If your
  jurisdiction wants per-year series, name one yourself (`INV` then `INA`).
* **Immutability**: after approval the content cannot change. The API refuses edits (409), and the
  database refuses them too (triggers on invoices, lines and payments), so a bug or a manual SQL
  session cannot rewrite an issued invoice. The correction path is **void** (only when nothing was
  paid or credited) or a **credit note** against the invoice. The buyer's name, address and tax id
  are frozen onto the invoice at approval.
* **Separation of duties**: the person who created or last edited an invoice cannot approve it
  (`403 separation_of_duties`). Subjects are compared case-folded and Unicode-normalised, and a token
  cannot be minted for a second spelling of an existing subject. Voiding an invoice applies the
  same rule to its creator. Timesheets also remember who last edited and who submitted them, so an
  admin who edits or submits someone's week cannot approve it. Invoices generated by a recurring
  schedule are created in the name of the person who set the schedule up, so that person cannot
  approve them. It is enforced in one function used by invoices, timesheets,
  expenses and purchase orders, and an optional `Approver` interface can narrow it further (for
  example "only the owner's manager approves a timesheet") but never widen it.
* **Payments** are append-only. Record them by hand (`POST /v1/invoices/{id}/payments`), import a
  bank or provider export (`POST /v1/payments/import`, up to 200 rows, all or none, de-duplicated on
  `external_id`, so re-importing a file is safe), or let a payment provider's signed webhook record
  them. A payment cannot exceed the balance. A verified provider notification that cannot be
  applied (unknown or void invoice, overpayment, wrong currency, no currency is refused outright) is
  acknowledged with `200 {"status":"unmatched"}` so the provider stops retrying, and is recorded in
  the audit log as `payment.unmatched` for a person to reconcile. A **credit note** can credit at
  most the unpaid balance (`total - paid - credited`); money already received is not credited by
  this module (refunds are outside it), so a balance never goes negative. When the balance reaches zero the invoice is `paid` and
  `invoice.paid.v1` is emitted.
* **Aging**: `GET /v1/reports/aging?as_of=` groups unpaid balances into current, 1-30, 31-60, 61-90
  and over 90 days past due, per currency and per customer.
* **Recurring**: a schedule (weekly, monthly, quarterly, yearly) drafts an invoice per period.
  Drafts are never approved or sent automatically. Each (schedule, period) is unique in the
  database, so overlapping or repeated runs cannot duplicate. Month steps clamp to the end of a
  short month and do not remember the original day: start on days 1 to 28 for stable dates.
* **Rendering**: `GET /v1/invoices/{id}/html` is a print-friendly page (no JavaScript, no images,
  strict CSP). Every value is escaped, so a description such as `<script>` shows as text. There is no
  PDF generator: use the browser's "print to PDF".
* **Public link**: `POST /v1/invoices/{id}/link` returns a URL with an HMAC-signed, expiring token
  (default 14 days, at most 90). It shows the invoice and the payment instructions text, needs no
  login and exposes nothing else. It is customer-facing, so it shows only: the seller block from
  config, the buyer snapshot, number, dates, currency and rate, lines, totals, paid and credited
  amounts, balance, the invoice `notes`, the payment instructions, and a bare VOID or DRAFT marker.
  Internal fields (void reason, creator, approver, project, history) are never rendered; do not put
  anything internal in `notes`. Every failure (forged, expired, revoked, unknown) is the same 404.
  `POST .../link/revoke` invalidates every link issued for the invoice. Bad links count against the
  caller's address and are throttled; valid links are never throttled. Views are audited (one entry
  per invoice per ten minutes). In a cluster, `modules.finance.expose.public: true` routes
  `https://finance.<domain>/public/invoices/` and nothing else and sets `public_base_url` to it
  ([`module-routing.md`](module-routing.md)); elsewhere route **only** that path to the internet.
* **Payment provider adapter**: `PaymentProvider` (Stripe-like: create a checkout, verify a signed
  webhook). The shipped `fake` provider never opens a socket (checkout URLs use the reserved
  `.invalid` TLD) and signs webhooks like Stripe (`t=...,v1=<HMAC>`), so a real Stripe adapter only
  adds the HTTP calls. Enable the fake for development with `payment_provider: fake`.

## Contracts

Templates hold text with `{{variables}}`. A contract is rendered from a template (or a plain body)
into **version 1**; every placeholder needs a value and every value must be used, and a value that
itself contains `{{x}}` is not expanded again. Each version is immutable and stored with its
**SHA-256**. States: `draft` -> `sent` -> `signed` -> `active` -> `expired` or `terminated`.

* `send` asks the **e-signature adapter** (`ESigner`) to collect signatures on the current text,
  identified by its hash, or records a manual send (`{"manual": true}`) for wet-ink flows. Signer
  addresses go to the service and are not stored here. The shipped `fake` signer never touches a
  network. **Documenso path**: an adapter creates a document from the body through Documenso's API,
  adds the recipients, registers a webhook to `POST /v1/webhooks/esign`, and maps Documenso's
  "completed" event to a `SignNotice` carrying the SHA-256 of the signed PDF's source text. The
  interface is the seam; the adapter is a later, separate module.
* A signature is accepted only when its document hash equals the contract's current text. A text
  that changed after sending (a new version) puts the contract back to draft. After signing the text
  never changes: terminate and write a new contract. `GET /v1/contracts/{id}/verify` recomputes the
  hash of every stored version.
* Renewal: set `renewal_date` and `reminder_days`. An hourly sweep (and `POST /v1/contracts/sweep`)
  activates signed contracts when their start date arrives, expires them after their end date, and
  emits one `contract.expiring.v1` per renewal date inside the lead time (changing the date re-arms it).

## Timesheets

A person's week (a Monday) holds entries: day, project key, optional task key, minutes, billable
flag. Project and task keys are **opaque references to the `work` tracker**; this service never
calls it. Workflow: `draft` -> `submitted` -> `approved` or `rejected` (back to editable). Nobody
approves their own sheet. `PUT /v1/period-lock` (admin, audited) **locks** weeks and expense dates
up to a date: nothing in them can be created, edited, submitted or decided. The most recent lock
is the one in force, so an admin can reopen a period by setting an earlier date (audited).

`POST /v1/timesheets/billing-draft` turns approved, billable, not yet billed time of one project
and date range into a **draft invoice**: one line per task, quantity in hours to the milli-hour at
the rate you give. The entries are marked billed and cannot be billed twice; deleting the draft or
voiding the invoice releases them.

## Expenses and purchase orders

An expense has an amount, currency, category, date, optional project and **receipt metadata**
(file name, type, size, SHA-256; the module stores no file bytes: keep the file in `files`).
Workflow: `draft` -> `submitted` -> `approved` or `rejected`, then `reimbursed`. Nobody approves or
reimburses their own claim. A **purchase order** to a vendor is drafted, approved by someone other
than its creator (which numbers it `PO-00001`), then marked received or cancelled. Bills,
three-way matching and vendor portals are later work.

## Access

Every request needs `Authorization: Bearer <token>`.

| Scope | Allows |
|---|---|
| `finance.read` | read everything in scope |
| `finance.write` | create and edit, send, record payments (implies read) |
| `finance.approve` | approve invoices, timesheets, expenses, purchase orders; void (implies read, **not** write) |
| `finance.admin` | tax rates, tokens, audit log, erasure, period locks, recurring runs (implies all) |

A token can be limited to **parties** (customers and vendors by id) and **projects**. Scope applies
to the whole query: a record outside it is a `404`, exactly like one that does not exist, and no
filter can widen it. A party-scoped token cannot create parties. An **`own_only`** token (for an
employee) sees and edits only the timesheets and expenses it owns and has no access to anything
else. The can-core role `finance:employee` grants nothing: a can-core token or admin sign-in is never
own-only, so give employees an `own_only` token from finance itself. A token creator can only hand out scopes, parties and projects it holds itself. The bootstrap
token `CAN_FINANCE_ADMIN_TOKEN` has every scope; mint real tokens with it. The subject
`bootstrap-admin` is reserved. Use opaque person ids (not email addresses) as token subjects, since
they appear in timesheet and expense events.

Failed authentications are counted per client address and throttled past a limit; that throttle
never affects a valid token. A valid token has its own budget (`rate_limit_per_minute`, 600 by
default; `0` disables), so a runaway client of one token cannot starve others. Request bodies are capped, JSON is
decoded strictly (unknown fields are an error, so a client cannot set `status` or `number`), lists
are cursor-paged and reads run under a 5-second deadline.

Records carry a `version`, also the `ETag`; send `If-Match: "<version>"` to avoid overwriting
someone else's edit (`412`). Creates (invoices, payments, expenses, purchase orders, billing drafts,
imports) accept an `Idempotency-Key` header: a retry returns the original result (`200` with
`Idempotent-Replayed: true`), and the same key with a different body is `422`. Keys are remembered
for **7 days** per caller; after that the same key creates again.

## Personal data, retention and erasure

Party contact data is personal data. Reads of a party or the party list, every export, issuing a
public link, and every public view are **audited** (append-only; the database refuses updates and
refuses deletes except by the retention purge, and never of entries under a year old). The audit log
holds no contact data and no secrets. Events carry identifiers, numbers and amounts only, never
names or addresses.

**Legal retention overrides erasure** for issued invoices. `POST /v1/parties/{id}/erase` (admin):

* deletes the party's drafts (invoices, **contracts that were never sent, with their versions**,
  purchase orders), recurring schedules and the cached idempotent responses that name the party. A
  contract that was sent and then given a new version keeps the versions it was sent with;
* pseudonymises the party record: the name becomes `Erased party #<id>`; contact name, email,
  phone, address, tax id and notes are emptied; the party can no longer be used;
* **keeps** issued invoices and credit notes, their payments and the frozen snapshot of name,
  address and tax id they were issued with, and sent or signed contracts and approved purchase
  orders, until the retention period ends;
* returns a report listing what was erased, what was kept (unpaid invoices separately as
  `finance-invoice-unpaid` with an empty `until`, since their retention clock starts when they are paid
  or voided), how many records and until when
  (`retained[].until`), so the data subject can be told;
* truncates the SQLite write-ahead log so the erased values do not linger in it
  (`secure_delete` already zeroes freed pages).

Retention is `retention_years` (default 7, counted from the invoice issue date, expense date, week
start, contract end or purchase-order date). The hourly purge deletes expired records with their
lines, payments and versions, never an invoice that is still unpaid, and finally deletes erased
parties that no record refers to any more. When a purge cannot delete an erased party yet because an
unpaid invoice or a record still inside its retention refers to it, the purge report says so
(`erased_parties_kept`) and the party is retried every hour. Set it to what your jurisdiction requires, or `0` to keep
everything. `audit_retention_days` must be at least 365.

## Importing customers and suppliers

`can import parties parties.csv` previews and `--apply` writes the whole file or nothing
(`CAN_FINANCE_URL`, `CAN_FINANCE_TOKEN`). Each row needs `external_id` and `name`; running the same file
again changes nothing. It needs `finance.write` and a token that is not limited to parties, projects or own
records. There is no web page for this yet. See [specs/047-csv-import](../specs/047-csv-import/spec.md).

## CSV exports

`GET /v1/exports/invoices.csv`, `payments.csv`, `expenses.csv` (scoped, audited). Text cells that
begin with `=`, `+`, `-`, `@`, a tab or a carriage return are prefixed with an apostrophe so a
spreadsheet cannot run them as formulas; amounts are written as plain decimals from integers.

## Running it

`can-finance serve` reads `finance.yaml` (optional, strict) and secrets from the environment:
`CAN_FINANCE_ADMIN_TOKEN` and `CAN_FINANCE_LINK_SECRET` (32+ characters, at least 10 distinct),
`CAN_FINANCE_WEBHOOK_SECRET` when `webhook.url` is set, `CAN_FINANCE_PROVIDER_SECRET` with the fake
providers. Settings are described in `modules/finance/settings.schema.json`. Events are queued in
a SQLite outbox and POSTed signed (`X-Can-Signature`, the same scheme as the forms gateway),
at least once, with backoff, dead-lettering after seven days (`GET /v1/events/dead`, replayable).

| Event | When |
|---|---|
| `invoice.sent.v1` | an approved invoice or credit note is marked sent |
| `invoice.paid.v1` | payments bring the balance to zero |
| `contract.signed.v1` | a sent contract is signed and the hash matches |
| `contract.expiring.v1` | an active contract enters its renewal reminder window |
| `timesheet.approved.v1` | a timesheet is approved |
| `expense.approved.v1` | an expense is approved |
| `journal.posted.v1` | a journal entry is posted (E15c: [`accounting.md`](accounting.md)) |
| `bank.statement.imported.v1` | a bank statement file is imported (ids and counts only) |

The OpenAPI document is `specs/014-finance-basics/contracts/openapi.yaml`; the capability schemas
are `schemas/capabilities/{invoices,contracts,timesheets}.v1.json`.

## Accounting interfaces (E15c)

A chart of accounts, a double-entry journal posted from the events above, period close, bank statement import with reconciliation and
file adapters (CSV journal export, CSV payroll import; xero, quickbooks and wave are documented stubs) live in the same binary and database. They have their
own permissions (`finance.ledger.*`, unscoped tokens only), a sealed bank account number (`CAN_FINANCE_SEAL_KEY`, 64 hex characters, optional until you add a
bank account) and their own limits and disclaimers: see [`accounting.md`](accounting.md).

## Web UI

The module can serve a server-rendered web UI next to the API (dashboard, approvals, parties, invoices, contracts, timesheets, expenses, purchase orders, reports): see [`finance-ui.md`](finance-ui.md). It is off unless `ui.enabled` is set.
