# Contract: finance UI routes

Served by `can-finance` on the same listener as the JSON API when `ui.enabled: true` (in the dev
compose stack: `http://finance.localhost:8080/`). `/v1/*`, `GET /public/invoices/{token}` and
`GET /healthz` belong to the API (`specs/014-finance-basics/contracts/openapi.yaml`); everything else
is the UI. Source: `core/finance/ui*.go`. Tests: `TestUIRoutesMatchContract` (this table against
`finance.UIRoutes()`, scope column included), `TestUISessionRoutesRequireASession` (every "yes" route
answers `303 /login` without a session), `TestUIContractRoutesEnforceTheirScope` (a token without the
scope gets 403 on every route, an own-only token on every route not marked `+own`),
`TestUIContractNamesRealStoreMethods` (every method named below exists).

**Authorisation.** The UI has no authorisation logic of its own. The **Scope** column is the same scope
the API's `guard` demands for the same operation (`read`, `write`, `approve`, `admin`; `session` means
any signed-in token) and `+own` marks routes an own-only (employee) token may use; both are checked by
one function (`allowedBy`) shared with the API's guard. The accounting scopes (`ledger.read`, `ledger.write`, E15c) are only ever granted to an unscoped token that is not own-only: `allowedBy` refuses a party- or project-limited token on them even if it holds the permission. The **Store methods** are the calls the route
makes; they scope every query to the token's parties and projects (a record outside the scope is the
same `404` page as one that does not exist) and decide approvals in one place (`authorizeApproval`:
scope, the configured `Approver`, and separation of duties). A page may hide or disable a button; the
Store makes the decision again when the form is posted.

**Unsafe requests** are `application/x-www-form-urlencoded` posts carrying the hidden field `csrf` (the
session's token; on the login form, the double-submit token from the login cookie) and, when the browser
sends `Sec-Fetch-Site`, only `same-origin` or `none` is accepted. Failures are `403`. Create forms carry
a random `idem` key made when the form was rendered, so a double submit creates one record. Every state-change form
carries the record `version`; a post without one is `400`, a stale one `409`. Unauthenticated requests to a session route get
`303 /login`. Unknown paths get the 404 page. Every response carries `Cache-Control: no-store` (static
files excepted) and no `ETag` or `Last-Modified`.

**Statuses of failures:** 400 a form without its version, 422 invalid input (the page lists every problem), 404 not found or out of
scope, 403 missing scope or separation of duties, 409 conflict, locked period or stale form, 503
(`Retry-After`) a database deadline or the approvals engine did not answer.

| Route | Session | Scope | Store methods | Purpose |
|---|---|---|---|---|
| `GET /login` | no | - | - | Sign-in form; sets the login CSRF cookie |
| `POST /login` | no | - | the shared Authenticator | Field `token`, `csrf`. 303 `/` on success; 401 generic, 403 stale form, 429 after repeated failures (valid tokens are never throttled) |
| `GET /login/ticket` | no | - | - | Single sign-on landing (`specs/036-module-ui-sso`): query `t` (a launch ticket). Shows a Continue form only; it never consumes the ticket, so a link scanner or prefetch cannot spend it. Sets the login CSRF cookie. 400 for a malformed ticket, 404 when not installed under can-core |
| `POST /login/ticket` | no | - | - | Fields `ticket`, `csrf` (double-submit). Redeems the ticket at can-core (this module is the audience), then 303 to a clean URL (`/` or the checked landing path) with a new session. 401 for every refusal (one message), 403 stale form or cross-site, 429 after repeated failures (a valid ticket is never throttled), 503 when can-core cannot answer. When `ui.token_login` is off, `POST /login` answers 403 and `GET /login` shows no token form |
| `GET /static/{file}` | no | - | - | Embedded stylesheet and icon |
| `GET /manifest.webmanifest` | no | - | - | Web app manifest |
| `POST /logout` | yes | session+own | - | Ends the caller's own server-side session, 303 `/login` |
| `GET /{$}` | yes | session+own | `Dashboard`, `ListInvoices`, `ListTimesheets`, `ListExpenses`, `ListPurchaseOrders` | Dashboard: my approvals waiting, overdue invoices, this month's totals (aggregates only); an own-only token gets links to its sheets and claims |
| `GET /parties` | yes | read | `ListParties` | Search (`q`, at most 100 characters) and kind filter; cursor paged; audited read |
| `GET /parties/new` | yes | write | - | Create form |
| `POST /parties` | yes | write | `CreateParty` | Create, 303 to the party |
| `GET /parties/{id}` | yes | read | `GetParty`, `ListInvoices` | Party with contact details (audited read) and recent invoices |
| `GET /parties/{id}/edit` | yes | write | `GetParty` | Edit form (not for an erased party) |
| `POST /parties/{id}` | yes | write | `UpdateParty` | Update with `version` |
| `GET /parties/{id}/erase` | yes | admin | `GetParty` | Explanation of erasure and of legal retention; confirmation form |
| `POST /parties/{id}/erase` | yes | admin | `ErasePartyData` | Fields `confirm` = `ERASE` and `token` (the sign-in token again, compared with the session's); shows the service's report: erased fields, deleted drafts, what is retained and until when |
| `GET /invoices` | yes | read | `ListInvoices` | Filters `status`, `kind`, `party`, `project`, `from`, `to`, `overdue=1`; 50 per page, cursor paged |
| `GET /invoices/new` | yes | write | `ListParties`, `ListTaxRates` | Create form with repeated line rows |
| `POST /invoices` | yes | write | `CreateInvoice` | Create a draft, idempotent on `idem` |
| `GET /invoices/{id}` | yes | read | `GetInvoice`, `ListPayments` | Invoice; buttons shown from the status and the token's scopes; approve is disabled for the creator or last editor |
| `GET /invoices/{id}/edit` | yes | write | `GetInvoice`, `ListTaxRates` | Edit form of a draft; 409 for an issued invoice (immutable) |
| `POST /invoices/{id}` | yes | write | `UpdateInvoice` | Update a draft with `version`; 409 for an issued invoice |
| `POST /invoices/{id}/delete` | yes | write | `DeleteDraft` | Delete a draft |
| `POST /invoices/{id}/request-approval` | yes | write | `RequestInvoiceApproval` | Open the can-core approval request (only when can-core is wired) |
| `POST /invoices/{id}/approve` | yes | approve | `ApproveInvoice` | Approve: number, freeze, snapshot of the buyer; separation of duties in the Store |
| `POST /invoices/{id}/send` | yes | write | `SendInvoice` | Mark as sent |
| `POST /invoices/{id}/void` | yes | approve | `VoidInvoice` | Void with `reason` |
| `GET /invoices/{id}/credit-note` | yes | write | `GetInvoice`, `ListTaxRates` | Credit note form |
| `POST /invoices/{id}/credit-note` | yes | write | `GetInvoice`, `CreateCreditNote` | Draft a credit note, idempotent on `idem`; the cap is enforced at approval |
| `POST /invoices/{id}/payments` | yes | write | `GetInvoice`, `RecordPayment` | Record a payment (`amount` in major units, parsed with the currency exponent), idempotent on `idem` |
| `POST /invoices/{id}/link` | yes | write | `IssueLink` | Create a signed public link; the response shows it once |
| `POST /invoices/{id}/link/revoke` | yes | write | `RevokeLinks` | Revoke every link of the invoice |
| `GET /invoices/{id}/html` | yes | read | `GetInvoice` | The printable invoice, rendered by the service's own renderer |
| `GET /exports/invoices.csv` | yes | read | `ExportInvoicesCSV` | CSV, formula-safe, audited |
| `GET /exports/payments.csv` | yes | read | `ExportPaymentsCSV` | CSV, formula-safe, audited |
| `GET /exports/expenses.csv` | yes | read | `ExportExpensesCSV` | CSV, formula-safe, audited |
| `GET /recurring` | yes | read | `ListRecurring`, `PartyNames` | Schedules |
| `GET /recurring/new` | yes | write | `ListParties`, `ListTaxRates` | Create form |
| `POST /recurring` | yes | write | `CreateRecurring` | Create |
| `POST /recurring/{id}/status` | yes | write | `SetRecurringStatus` | Pause, resume, end |
| `GET /contracts` | yes | read | `ListContracts`, `PartyNames` | Contracts with status and renewal dates |
| `GET /contracts/new` | yes | write | `ListTemplates`, `ListParties` | Choose a template, then fill its variables |
| `POST /contracts` | yes | write | `CreateContract` | Create from a template with variables (`var_<name>`) |
| `GET /contracts/{id}` | yes | read | `GetContract`, `GetVersion`, `PartyNames` | Contract, status, signature, hashes, versions list (read-only) |
| `GET /contracts/{id}/versions/{n}` | yes | read | `GetContract`, `GetVersion` | One version, read-only |
| `POST /contracts/{id}/send` | yes | write | `SendContract` | Send for signature (manual or through the configured provider) |
| `POST /contracts/{id}/terminate` | yes | write | `TerminateContract` | Terminate a signed or active contract |
| `POST /contracts/{id}/dates` | yes | write | `UpdateContractDates` | Start, end, renewal, reminder |
| `GET /timesheets` | yes | read+own | `ListTimesheets` | My timesheets (an own-only token sees only its own) |
| `POST /timesheets` | yes | write+own | `CreateTimesheet` | Open a week (a Monday) |
| `GET /timesheets/{id}` | yes | read+own | `GetTimesheet`, `PeriodLock` | Weekly grid form, or read-only when submitted, approved or locked |
| `POST /timesheets/{id}/entries` | yes | write+own | `GetTimesheet`, `SetEntries` | Save the grid with `version` |
| `POST /timesheets/{id}/submit` | yes | write+own | `SubmitTimesheet` | Submit |
| `POST /timesheets/{id}/approve` | yes | approve | `ApproveTimesheet` | Approve; separation of duties in the Store |
| `POST /timesheets/{id}/reject` | yes | approve | `RejectTimesheet` | Reject with `reason` |
| `GET /expenses` | yes | read+own | `ListExpenses` | My expenses |
| `GET /expenses/new` | yes | write+own | - | Create form with receipt metadata fields (no file upload) |
| `POST /expenses` | yes | write+own | `CreateExpense` | Create, idempotent on `idem` |
| `GET /expenses/{id}` | yes | read+own | `GetExpense` | Expense; approve is disabled for the owner |
| `POST /expenses/{id}/submit` | yes | write+own | `SubmitExpense` | Submit |
| `POST /expenses/{id}/approve` | yes | approve | `ApproveExpense` | Approve |
| `POST /expenses/{id}/reject` | yes | approve | `RejectExpense` | Reject with `reason` |
| `POST /expenses/{id}/reimburse` | yes | write | `ReimburseExpense` | Record the reimbursement (not by the owner) |
| `GET /purchase-orders` | yes | read | `ListPurchaseOrders`, `PartyNames` | Purchase orders |
| `GET /purchase-orders/new` | yes | write | `ListParties`, `ListTaxRates` | Create form |
| `POST /purchase-orders` | yes | write | `CreatePurchaseOrder` | Create, idempotent on `idem` |
| `GET /purchase-orders/{id}` | yes | read | `GetPurchaseOrder`, `PartyNames` | One order |
| `POST /purchase-orders/{id}/approve` | yes | approve | `ApprovePurchaseOrder` | Approve |
| `POST /purchase-orders/{id}/status` | yes | write | `SetPurchaseOrderStatus` | Receive or cancel |
| `GET /approvals` | yes | approve | `ListInvoices`, `ListTimesheets`, `ListExpenses`, `ListPurchaseOrders` | The approver queue: what the caller could decide (own work left out) |
| `GET /reports/aging` | yes | read | `Aging`, `PartyNames` | Receivables aging, per currency, at most 200 customers per currency |
| `GET /ledger/accounts` | yes | ledger.read | `ListAccounts` | E15c: the chart of accounts, read-only (`?all=1` adds inactive accounts) |
| `GET /ledger/journals` | yes | ledger.read | `ListJournals` | E15c: journal entries, newest first, filter `status` and `source`; read-only |
| `GET /ledger/journals/{id}` | yes | ledger.read | `GetJournal` | E15c: one entry with its lines and the link to its reversal; read-only |
| `GET /ledger/trial-balance` | yes | ledger.read | `TrialBalanceAsOf` | E15c: trial balance as of `as_of`, per currency, never converted |
| `GET /bank/reconciliation` | yes | ledger.read | `ListBankAccounts`, `Reconciliation`, `ListBankTransactions`, `SuggestMatchesFor` | E15c: the months of one bank account and its oldest 25 unmatched transactions with suggestions; the read is audited before the page is rendered |
| `POST /bank/transactions/{id}/confirm` | yes | ledger.write | `ConfirmMatch` | E15c: fields `invoice_id`, `account`, `idem`, optional `payment_id` (link a payment already recorded) and `record_new`; records the payment through the payment path. Money out is refused. No record `version` (a transaction is decided once: a second post is `409`) |
| `POST /bank/transactions/{id}/ignore` | yes | ledger.write | `IgnoreTransaction` | E15c: fields `reason`, `account`; final, like confirm |
| `GET /reports/statement` | yes | read | `ListParties`, `GetParty`, `ListInvoices`, `ListPayments` | One customer's documents and payments (newest 100) |
| `GET /settings` | yes | read | `ListSeries`, `ListTaxRates`, `PeriodLock` | Read-only: series (unscoped tokens only: the counters are company-wide), tax rates, company settings |
