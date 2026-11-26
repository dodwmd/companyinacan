# Implementation plan: Accounting interfaces (E15c)

Spec: [spec.md](spec.md). Research: [research.md](research.md). Data model: [data-model.md](data-model.md).
Contracts: [contracts/](contracts/). Tasks: [tasks.md](tasks.md).

## Shape

All in `core/finance` (one binary, one database, one RAM budget):

| File | Content |
|---|---|
| `migrations/003_accounting.sql` | tables and triggers (new numbering after `002_core_approvals.sql`; applied to existing databases by the existing migrator) |
| `ledger.go` | accounts, template, mappings, period close, journal create/post/reverse, the posting core (`postEntryTx`) |
| `ledger_post.go` | the automatic postings (`postInvoiceTx`, `postPaymentTx`, `postExpenseTx`, ...), called from `ApproveInvoice`, `VoidInvoice`, `recordPaymentTx` and the expense decisions |
| `ledger_reports.go` | trial balance, general ledger, profit and loss, balance sheet |
| `bank.go`, `bankparse.go`, `seal.go` | bank accounts, import, suggestions, confirm and ignore, reconciliation; the parsers; the sealer |
| `adapters.go` | `AccountingExporter`, `PayrollAdapter`, the CSV adapters, the stubs, export batches |
| `api_ledger.go`, `api.go` | handlers and the routes (registered in `api.go` so the OpenAPI route test covers them) |
| `ui_pages_ledger.go`, `ui_messages_ledger.go`, `ui/templates/{ledger_*,bank_reconciliation}.html` | five pages and two forms; no SQL, no permission checks beyond the route table |
| `ledger_template.json` | the default chart as data |

Touched elsewhere: `auth.go` (permissions and `Can`), `ui.go` (`allowedBy`, caps, nav, route table), `invoices.go`
(`ApprovalRequest.Perm`, hooks), `payments.go`, `expenses.go`, `retention.go`, `app.go`/`store.go` (seal key),
`core/server/modsecrets` (generated `CAN_FINANCE_SEAL_KEY`), `schemas/events`, `modules/finance`, `test/contract`.

## Constitution check

| Principle | Check |
|---|---|
| I Spec-first | SPEC.md section 5 says integrate, do not build full accounting engines: this is an interface layer (journal, import, export) beside the operator's system, with its limits written down (`docs/accounting.md`). The OpenAPI document and the UI route contract are extended and tested against the routers. |
| II Footprint | No dependency added (`encoding/csv`, `encoding/xml`, `x/crypto` already in `go.mod`). Re-measured: idle 18.5 MB, 31.7 MiB at the peak of a heavy load including a 5000-row import and 800 renders; budget 28/64 kept (31.7 under 28 x 1.2) (`research.md` R9). Templates still parse on first use. |
| III Test first | `ledger_property_test.go` was written before any ledger code; the one red run recorded stopped at a duplicate test helper name, so the missing symbols were never shown failing. API, bank, parser and UI tests were written after their code, and so were the QA regression tests (`qa_ledger_test.go`), which were not run against the unfixed code. Five fuzz targets, each run for 20 s without a finding before the QA round. |
| IV Stable module contracts | New events have schemas, fixtures (valid and invalid), the manifest lists them and the live-event test validates what a running service sends. New permissions are in the module's own manifest, where the registry gives ownership to the first declarer. Existing routes and events are unchanged. |
| V Secure and private by default | Deny by default (unscoped tokens only), 404 for unknown ids, audit before return for bank reads (fail closed), same-transaction audit for writes, sealed numbers with AAD, immutability triggers, period locks, SoD through `authorizeApproval`, idempotency keys, CSV formula defusing, XXE and bomb refusal, linear parsing, deadlines on every read. |
| VI Open-source hygiene | Apache-2.0 like the rest; no new third-party code and no new dependency (`go.mod` and `THIRD_PARTY_LICENSES` unchanged). |
| VII Simplicity and measured claims | One currency per entry and no conversion (stated); six mapping keys; no engine for tax or payroll; stubs are labelled stubs; the RAM numbers are measured and quoted with their method. |

## Design decisions in brief

1. **One currency per entry.** Balanced means `sum(debit) = sum(credit)` in minor units, checkable in SQL, and reports per currency need no FX policy.
2. **Draft then post.** Entries are inserted as drafts and flipped by an `UPDATE`; the trigger on that update checks line count, balance and the period, and an insert already posted is refused. A bug cannot skip the check.
3. **Automatic posting fails the event.** A document without its entry is worse than a refused approval, so a closed period or a deactivated mapped account stops the approval with a clear error.
4. **Idempotent by construction.** A unique index on `(source, source_ref)` makes a business event post once; reversal has its own unique index.
5. **Accounting permissions are separate and unscoped-only.** `allowedBy` is the single decision for API and UI, and `authorizeApproval` re-checks it for approvals.
6. **Payments are the only door for money received.** Confirming a bank transaction calls `recordPaymentTx`, so there is no second payment path to keep consistent.
7. **Exports store their bytes.** A batch can be downloaded again identically; the cursor and the batch commit together.
8. **Retention.** Journal entries stay (no personal data, and deleting old ones would change later balances); decided bank transactions follow the invoices' retention; open work is never purged.

## Testing

`go test -race ./core/finance/... ./test/...` and `make test`: property tests, API tests (scopes, SoD, period, mapping, export, payroll),
bank tests (sealing, import, dedup, matching, reconciliation, fail-closed audit, parsers, linearity, retention), UI tests (golden HTML,
accessibility checklist, forms, scope refusal), contract tests (OpenAPI against the router, event schemas and fixtures, live events, manifest).
