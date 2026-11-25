# Finance web UI

People who run the books use the `finance` module from a browser or a phone: dashboard and approver
queue, customers and vendors, invoices and credit notes, payments, recurring schedules, contracts,
timesheets, expenses, purchase orders, aging and statement reports. It is server-rendered Go templates with an embedded
stylesheet: no JavaScript, no framework, no build step. It is a client of the same `Store` as the
JSON API (see [`finance.md`](finance.md)), so what a token may see or do is decided by the Store (token scopes, party
and project limits, separation of duties), not by the pages. Source: `core/finance/ui*.go`.
Specification: `specs/031-finance-ui/`. Route table:
[`specs/031-finance-ui/contracts/ui-routes.md`](../specs/031-finance-ui/contracts/ui-routes.md).

This is a tool, not accounting, tax or legal advice. The UI is **experimental**: it is unit- and contract-tested
offline and has not been checked in a real browser, with a screen reader, or on a cluster.

## Turning it on

Off by default. In `finance.yaml`:

```yaml
ui:
  enabled: true
  # force_secure: true       # production: Secure, __Host- cookies even if a proxy terminates TLS
  # insecure_cookies: true   # plain-HTTP development only; refused with force_secure or an https public_base_url
  # notifications_url: https://admin.example.com/notifications
  # idle_minutes: 30
  # absolute_hours: 8
```

The UI is served on the module's listener at `/`; `/v1/*`, `GET /public/invoices/{token}` and
`GET /healthz` stay the API. Unlike the work UI (which decides per request and defaults `force_secure` from its public URL), cookies here are `Secure` with a `__Host-` name **unless** `insecure_cookies` is set, so a forgotten setting fails safe; `force_secure` states the intent and makes a contradicting `insecure_cookies` a configuration error, as does an https `public_base_url`. Behind
a proxy also set the top-level `trust_proxy: true` so failed sign-ins are throttled per client address. In the dev
compose stack it is at `http://finance.localhost:8080/` (`dev/compose/config/finance.yaml` turns it on). The Helm chart
does not expose the `ui` block or a route yet: the module has no subdomain, so can-core's installer cannot route
it (follow-up).

## Signing in

1. Use a finance token: a personal access token from can-core, or a module token made through the tokens API
   (`POST /v1/tokens`), or the bootstrap admin token for a first look.
2. Open the finance site and paste the token at **Sign in**. The server checks it with the same authenticator chain the
   API uses, once.
3. You get a session: a random id in an `HttpOnly`, `SameSite=Strict` cookie, scoped to the finance origin. The server
   stores only its SHA-256. The token stays in server memory (never on disk, never sent back) so that revocation is
   noticed: a session re-checks its token every minute when it holds `finance.approve` or `finance.admin`, every five
   minutes otherwise, and a token that no longer authenticates ends the session. Idle timeout 30 minutes, absolute 8
   hours, at most 5 sessions per subject. A restart signs everyone out. Failed sign-ins are throttled per address;
   a valid token is never throttled.

### Single sign-on from the admin UI

With the module installed under can-core and its address configured
([`sso.md`](sso.md#opening-module-web-pages-from-the-admin-ui-e11c)), **Open** on the admin UI's **Apps** page
signs a person in without a token (`/login/ticket`, valid only in the browser that clicked **Open**; check the account, then **Continue**), with the finance permissions they hold.
The session is re-checked with can-core on the same intervals as above. `ui.token_login: false` (default
`true`) removes the paste-a-token form. Not verified in a browser or on a cluster.

## What the pages are

| Area | What you can do | Scope |
|---|---|---|
| Dashboard | Waiting for your approval, overdue invoices, this month's issued and received totals per currency (aggregates only, no names or documents) | read; approver count needs approve |
| Approvals | Draft invoices, submitted timesheets and expenses, draft purchase orders that you could decide. Anything you created, edited or own is left out | approve |
| Customers and vendors | List, search, create, edit; contact details are shown to `finance.read` (every list and page read is audited); **erase** for administrators | read / write / admin |
| Invoices | Filtered list (50 per page); create a draft with a repeated lines editor; edit or delete a draft; approve; mark sent; record payments; void; credit notes; public link; printable view; CSV export | read / write / approve |
| Recurring | Schedules that draft invoices; pause, resume, end | read / write |
| Contracts | Create from a template with variables, versions (read-only), send for signature (manual or the configured provider), signature status and hashes, renewal dates and reminders, terminate | read / write |
| Timesheets | Open a week, weekly grid form, submit; approvers decide from the queue or the sheet | read+own / write+own / approve |
| Expenses | Create with receipt **metadata** (name, type, size, SHA-256; no file upload), submit, approve or reject, reimburse | read+own / write / approve |
| Purchase orders | Create, approve, receive, cancel | read / write / approve |
| Reports | Receivables aging (per currency, as of a date) and a per-customer statement | read |
| Settings | Read-only: number series (shown to unscoped tokens only), tax rates, company settings, period lock | read |

`+own` means an own-only (employee) token may use the page and sees only its own timesheets and expenses; it cannot reach
anything else (every other route answers 403).

## Accounting pages (E15c)

For a token with a `finance.ledger.*` permission that is not limited to parties or projects: **Accounting** (chart of accounts, journal and
entry detail, trial balance; all read-only) and **Bank** (reconciliation: the months of a bank account and the oldest 25 unmatched transactions with
suggested invoices, a *Confirm match* form that records the payment, and an *Ignore* form with a reason). The forms need `finance.ledger.write` and `finance.write`.
Everything else (chart edits, mappings, manual journals, period close, exports, statement upload) is API only. Details and limits: [`accounting.md`](accounting.md).

## Money

Every amount is an integer count of the currency's minor unit; the page prints it with the currency's exponent
(`JPY` 0 decimals, `USD` 2, `KWD` 3, negatives with a leading minus) and the forms parse what you type back with the
same exponent. A form accepts plain digits with an optional single decimal point (`1234.50`); `1e3`, `1,5`, `NaN` and
too many decimals are refused with a message, never rounded. No float is involved anywhere. Currencies are never added
together: totals and the aging report are per currency.

## Separation of duties

The approve button of an invoice, timesheet, expense or purchase order is **disabled with the reason** for the person who
created, last edited or owns it, and the record is left out of that person's approval queue. That is only presentation:
the Store's single `authorizeApproval` decides again when the form is posted, so a forged request is refused
(403 "someone else must do this"). The same rule covers voiding an invoice. Recurring schedules draft invoices in the name of
the person who set them up, so they cannot approve those drafts either.

## Issued invoices are immutable

After approval an invoice has no edit form and no delete button; the edit and update routes answer 409. The page offers
only what the model allows: send, record a payment, void (not once payments or credit notes exist), draft a credit note,
create or revoke a public link, print. A credit note is capped at the invoice's unpaid balance when it is approved.
Numbers are gap-free per series; voiding keeps the number used.

## Public invoice links

**Create** a signed, expiring link from an issued invoice (a draft has no link). The link is shown **once**, in the response
to the create form; it is not stored, not put in a redirect or a query string, and not shown again. The invoice page only
offers **Revoke all links**, which invalidates every link of the invoice. The access log redacts any path containing `/public/invoices/` and the UI log never records query strings or bodies. Set `public_base_url` or the page shows only the path.

## Exports

`/exports/invoices.csv`, `/exports/payments.csv` and `/exports/expenses.csv` use the Store's exporters: scoped to the
token, formula-injection protected (a cell that starts with `= + - @`, tab or CR is prefixed with an apostrophe), capped,
and every download is written to the audit log. A malformed date is a 422 page, not a download.

## Erasure and legal retention

An administrator opens **Erase personal data** on a party, reads what will happen, types `ERASE` and enters the sign-in token again (step-up). A person who signed in from the admin UI (no token) is not asked
for one: erasure works only within five minutes of opening finance from the **Apps** page, and can-core is asked
at that moment whether they still hold `finance.admin`; otherwise it is refused and they open finance from Apps
again. This is as strong as the admin UI session they launch from: it does not ask for a password again. The result page is the
service's own report: the fields that were erased, the number of drafts deleted, and the **retained** list: issued invoices
and credit notes, payments, sent or signed contracts and approved purchase orders are legal records, kept for
`retention_years` after their own date with the name, address and tax id they were issued with; unpaid invoices are never
purged. The page says so and shows the date each class is kept until. Nothing is hidden: if the log truncation after the
erasure failed, the page says so and the report is still shown.

## Safety properties

- **One authorisation path.** The UI never reads the database and never decides an approval; a test fails if `ui*.go` does.
  The per-route scope check (`allowedBy`) is the function the API's guard uses. A record outside a token's party or project
  scope is the same 404 page as one that does not exist.
- **CSRF** token on every POST (constant-time compare) plus a `Sec-Fetch-Site` check; sign-in uses a double-submit cookie.
- **Idempotent create forms.** Every create form carries a random key made when the page was rendered; a double submit
  creates one record (invoices, credit notes, payments, expenses, purchase orders). Every state-change form carries the record version: a post without it is a 400, a stale one a 409 (as in the other UIs).
- **XSS.** `html/template` everywhere, no raw HTML, no inline script or style, CSP `default-src 'none'; script-src 'none'`,
  no `data:` URLs. Hostile names, notes and contract variables are tested in text, attribute and query contexts. Contract and
  note text is shown as plain text, `white-space: pre-wrap`; there is no markdown.
- **No caching** of pages: `Cache-Control: no-store`, no `ETag` or `Last-Modified`.
- **Bounded work.** Lists show 50 rows with a cursor; the aging report shows at most 200 customers per currency; the
  statement the newest 100 documents; every read has the Store's 5 second deadline and a deadline is a 503 page with
  `Retry-After`, not a hang.
- **Accessibility.** Labels on every field, one `h1`, landmarks, skip link, captions and header scopes on tables, WCAG AA
  colours in both schemes, `prefers-reduced-motion`, mobile-first CSS, 44 px targets, print styles, web app manifest.
  Every string is in a message catalogue (`ui_messages.go`).

## Memory

Templates are parsed on first use. Measured on the static linux/amd64 build (method in `specs/031-finance-ui/research.md`
R2): 17.2 MiB RSS idle with the UI on (the same as off), 27.9 MiB after 200 invoice creates, 400 list and aging reads,
700 page renders and 100 form posts; loaded RSS (27.9 to 29.9 MiB across runs) exceeded the old 20 MiB target, so the budget is now `ram_mb: 28`, `ram_limit_mb: 64` (module.yaml, chart values and `test/contract/finance_test.go` agree).

## Not done

See `specs/031-finance-ui/tasks.md`: no browser, screen-reader or phone pass; no cluster run; no chart exposure or
route; no direct OpenID Connect sign-in at the module (single sign-on is by launch ticket from the admin UI); no record-signature, contract-text editing or template editor pages; no recurring-schedule edit;
no tax-rate, token, audit-log or period-lock administration pages (the API has them); sessions do not survive a restart.
