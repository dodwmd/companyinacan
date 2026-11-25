# Feature specification: Finance web UI (Epic E15b)

**Status**: built offline and tested; not exercised in a browser or on a cluster ([V] items in
`tasks.md`). **Depends on**: `specs/014-finance-basics` (the Store, scopes, party and project limits,
separation of duties, immutability, retention), `specs/026-people-ui` (UI conventions that passed QA),
`specs/016-module-wiring` (personal access tokens). **Principle**: constitution I (small), IV (secure by
default), V (financial and personal data stays in-house).

## Why

SPEC.md names clients for finance: a web UI and an Android/PWA client. Until now the module was a JSON API.
Bookkeepers, approvers and employees (timesheets, expenses) need pages, without anyone building a second
authorisation layer or a second invoice renderer.

## Hard rules (the QA lessons of earlier epics, applied)

1. **One authorisation path.** The UI calls the same `Store` methods as the JSON API. It never reads the
   database, never calls `authorizeApproval`, never inspects permissions to allow something beyond the
   per-route scope (`allowedBy`, the function the API's guard also uses). `TestUISourceHasNoSecondAuthorizationPath`
   fails if `ui*.go` does.
2. **Money from integer minor units with the currency's exponent.** Never a float. Formatting is tested for
   JPY (0), USD (2), KWD (3) and negatives; form input is parsed with the same exponent and anything that is
   not plain digits with one optional point is refused.
3. **Separation of duties is visible and enforced.** Approve is disabled (with the reason) for the creator,
   last editor or owner, and their approval queue omits the record; the Store refuses a forged post (403).
4. **Immutability.** An issued invoice has no edit form, no delete button, and its edit routes answer 409;
   the page offers send, payment, void, credit note, link and print only.
5. **Idempotent create forms.** A random key per render; a double submit creates one record. Edit forms carry
   the record version (missing: 400, stale: 409).
6. **Public signed links**: created from an issued invoice, shown once in the create response, never stored by
   the UI, never in a redirect, query string or log (the access log now redacts the path); revocable.
7. **CSV export** goes through the Store's exporters (scoped, formula-injection protected, capped, audited).
8. **PII only where needed.** Contact details are on party pages and lists (read scope; every read is audited by
   the Store). Reports and the dashboard use names or aggregates only; the dashboard names nobody.
9. **Erasure versus retention is shown as it is**: the erase page explains, the result page is the service's
   report (erased fields, deleted drafts, the retained list with dates and reasons), nothing softened.
10. **Scoped tokens**: a party- or project-limited token sees only its records (the same 404 otherwise); an
    own-only token reaches only its own timesheets and expenses (403 elsewhere).
11. **XSS-proof in every context**: `html/template`, no raw HTML, no inline script or style, CSP
    `default-src 'none'`, no `data:` URLs; contract and note text are plain text, no markdown.
12. **CSRF** on every POST plus `Sec-Fetch-Site`; sign-in uses a double-submit cookie; `no-store` on every page.
13. **Valid credentials are never throttled**; only failed sign-ins draw on the per-address budget.
14. **Bounded work** with DB deadlines: 50-row cursor pages, aging at most 200 customers per currency, statement
    100 documents; a deadline is a 503 page with `Retry-After`.
15. **Accessibility and phones**: labels, one `h1`, landmarks, skip link, captions, AA contrast in both
    schemes, reduced motion, 44 px targets, print styles; every string is in the message catalogue.
16. **Printable invoices reuse the service's HTML renderer** (`serveInvoiceHTML` shared with the API): no second
    escaping logic.

## Authentication

As in `specs/026-people-ui`: a token is presented once at `/login`, authenticated by the API's chain, and a
server-side session is kept (hashed id, `HttpOnly`, `SameSite=Strict`, `__Host-`/`Secure` unless
`insecure_cookies`; `force_secure` refuses the insecure option; at most 5 sessions per subject; idle 30 min,
absolute 8 h). The token is kept in memory to re-check it: every 60 s for sessions that hold `finance.approve`
or `finance.admin`, every 5 min otherwise. Any authenticated principal may sign in (the module has no person
directory); what it can do is its scopes.

## User stories and acceptance

- **US1 Sign in and out**; timeouts, fixation, throttling, anonymous redirect, revocation within a minute (approver).
- **US2 Dashboard and approver queue.** Counts and totals; the queue of what I could decide.
- **US3 Parties.** List, search, create, edit; erase flow for administrators with the retention explanation.
- **US4 Invoices.** Filtered list; draft with a lines editor; approve (SoD); send; payments; void; credit notes;
  public link create/revoke; printable view; CSV export.
- **US5 Recurring schedules, contracts, purchase orders.** Create, list, status changes; contracts from templates
  with variables, versions list, send, signature status, renewal dates.
- **US6 Timesheets and expenses.** Weekly grid, submit, approve/reject with period locks shown; expenses with
  receipt metadata; employees see only their own.
- **US7 Reports and settings.** Aging, statement; read-only series and tax rates.

## Out of scope (see tasks.md "Not done")

Browser/screen-reader verification, chart exposure and a route (no subdomain), SSO sign-in, contract text
editing and template editor, signature recording, tax-rate/token/audit/period-lock admin pages, recurring edit.
