# Feature Specification: Invoices by email, PDF and payment reminders (Epic E15e)

**Branch**: `epic/E15e-invoice-email` | **Date**: 2026-11-26 | **Status**: built, tested offline and against the compose SMTP sink; experimental

SPEC.md references: invoices with dunning reminders and PDF output ("Money, time and paper"),
`email.send` (specs/039-email-send), signed public links (specs/045-public-access), external
recipients for finance (specs/049-contract-esign).

## What existed before

`SendInvoice` only flipped the status. Invoices were HTML only. Overdue invoices were visible in
the aging report; nothing reached the customer.

## User scenarios

1. **Email an invoice.** On an issued invoice or credit note a person presses "Email to customer".
   The party's email address receives a plain-text message with a link to the document and to its
   PDF. An approved document becomes `sent` once the mail service has accepted the message.
2. **Email a statement.** On a customer's page: the open invoices, their balances and links, and the
   total per currency.
3. **Reminders.** An administrator turns on a schedule such as 7, 14 and 30 days overdue. Customers
   are reminded once per step until they pay.
4. **A dispute.** The invoice is marked disputed (or reminders stopped, for the invoice or the whole
   customer): nothing more is sent about it.
5. **Did it go?** The invoice and customer pages show each message and its delivery status.

## Decisions

- **The billing contact is the party's `email` field.** There is no separate billing address.
- **Link only, no attachment.** `email.send` carries plain text only (specs/039: HTML and
  attachments out of scope), and this feature does not widen it. The message says nothing is
  attached and links to the PDF. Payment instructions are on the linked document, not in the mail.
- **Allow-list.** Customers are outside the company, so they need
  `email.modules.finance.recipients` or `external_recipients: true`, off by default. It is the same
  switch as for contract signers: turning it on for one turns it on for the other.
- **PDF written in Go, in `core/finance/pdf.go`.** No library, no embedded font, no new module
  dependency.
- **The schedule is stored in finance's database** and set through the API or web UI, not through
  `company.yaml`: finance's chart does not render module settings into `finance.yaml`.
- **Dispute is a reminder hold**, not an accounting state: it changes nothing about the invoice.

## Functional requirements

- **FR-001** `POST /v1/invoices/{id}/email` (finance.write, party and project scope as for reading)
  MUST send to the party's address only, and only for an issued document that is not void. It MUST
  refuse an erased party, a party without an address, a repeat within a minute and more than 10
  sends in 24 hours.
- **FR-002** `POST /v1/parties/{id}/statement/email` MUST list at most 50 open invoices and the total
  of all of them, and MUST refuse a project-scoped token.
- **FR-003** Mailed links are the existing signed public links, valid 60 days, revocable with the
  existing "revoke all links". Without `public_base_url`, or without can-core, emailing is off.
- **FR-004** `GET /v1/invoices/{id}/pdf` and `GET /public/invoices/{token}/pdf` MUST show what the
  HTML view shows and nothing more; the public one fails and is throttled exactly as the HTML link.
- **FR-005** Reminders MUST be off by default. A schedule has 1 to 5 increasing steps (days overdue,
  at most 365) and `max_overdue_days` (default 90, at most 730): older debts are not chased. Only an
  unscoped administrator may change it.
- **FR-006** A reminder MUST go only for an invoice that is `sent` or `partially_paid`, past due, with
  a balance, not held, its party not stopped, not erased and with an address. It is checked when
  claimed and again immediately before it is handed over.
- **FR-007** Each step MUST be sent at most once per invoice, across restarts and parallel runs. Only
  the latest step reached is sent (no backlog), two reminders for one invoice are at least 3 days
  apart, and a sweep claims at most 20.
- **FR-008** The sweep runs hourly and on `POST /v1/dunning/run`. When the mail service cannot take a
  message it stops, retries with the same idempotency key, and gives up after 72 hours.
- **FR-009** Delivery status per message MUST be visible: `pending`, `retry`, `queued`, `sent`
  (accepted by the relay), `failed`, `not_allowed`, `cancelled`, `unknown`.
- **FR-010** Claiming a message, every status change, holds and schedule changes MUST be audited in
  the same transaction. No address, name, subject, body or link is stored in the delivery records,
  audited, logged or put in an event. No new event type is added.
- **FR-011** Erasing a party MUST cancel what has not been handed over and stop all further mail.
  Delivery records hold no personal data and go when their invoice is purged (statement records after
  the retention period).

## Data

Migration `008_document_mails.sql`: `document_mails`, `invoice_reminder_holds`,
`party_reminder_stops`, `dunning_policy`. A row of `document_mails` is written before the call to
`email.send`, with a random key used as `source_id`.

## The PDF: what it can and cannot do

PDF 1.4, A4, the standard fonts Courier and Courier-Bold with WinAnsiEncoding. Every PDF reader
supplies these fonts; nothing is embedded.

- **Text is limited to Windows-1252** (Western European letters, the euro sign, typographic quotes and
  dashes). Anything else (Greek, Cyrillic, Chinese, Japanese, Arabic, Hebrew, emoji, many Central
  European letters) is written as `?`; the page then says so and points to the online view, which
  shows all text. Companies who need other scripts should send the HTML link or print it to PDF.
- No right-to-left layout, no logo or images, no hyphenation, one fixed layout in English.
- Not PDF/A, not tagged for accessibility (the HTML view is the accessible one), not signed, and not a
  structured e-invoice (Factur-X, Peppol).
- Checked by tests of the file structure and by one visual reading of a sample; not run through a PDF
  validator or tried in a range of readers.

**Licence.** All code is this project's (Apache-2.0). Character encoding uses
`golang.org/x/text/encoding/charmap` (BSD-3-Clause) from `golang.org/x/text`, already a dependency.
No font or font metrics file is shipped, so `THIRD_PARTY_LICENSES` does not change.

## Limits and threats

- A message already accepted by can-core **cannot be recalled**. If the relay is down it may leave up
  to 72 hours later, after the customer paid, a dispute began or the party was erased; can-core holds
  the address and text until then (docs/email.md, "Data kept").
- A payment finance does not know about yet (an unrecorded or unmatched bank payment) does not stop
  a reminder.
- Anyone holding a mailed link can read the invoice until it expires or is revoked.
- Restoring a backup from before a reminder was sent loses its record; if the step is still due it is
  sent again (can-core's idempotency keys only protect if can-core was not restored with it).
- Reminders and invoices share finance's mail quota (default 60 an hour, 500 a day) with signing mail.
- "Sent" means the SMTP relay accepted the message, not that it arrived. Bounces are not read.
- With `external_recipients`, whoever holds `finance.write` can mail a link to any address by putting
  it on a party, within the quota. The text is fixed apart from the seller name.

## Out of scope

Attachments; a separate billing contact; HTML mail; per-customer schedules; late fees; payment links
in mail; bills (payable); translations; real-relay and cluster verification.

## Tests

`core/finance/mailing_test.go` (send, refusals, statement, schedule, holds, restart, retry, lost reply,
crash after claim, erasure, purge, UI), `core/finance/pdf_test.go` (structure, encoding, layout),
`test/e2e/invoice_email_test.go` (compose stack: finance, can-core, SMTP sink, public link and PDF,
statement, one reminder).
