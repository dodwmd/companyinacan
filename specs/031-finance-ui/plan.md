# Implementation plan: Finance web UI (E15b)

Spec: [spec.md](spec.md). Research: [research.md](research.md). Data model: [data-model.md](data-model.md).
Contract: [contracts/ui-routes.md](contracts/ui-routes.md). Tasks: [tasks.md](tasks.md).

## Shape

- Package `core/finance`, files `ui.go` (handler, routes, authed wrapper, error mapping, login/logout, static),
  `ui_session.go` (session store), `ui_form.go` (money, quantity, percentage and duration parsing; line rows;
  idempotency and version fields), `ui_pages_core.go` (home, approvals, parties, reports, settings),
  `ui_pages_invoices.go` (invoices, payments, links, exports), `ui_pages_work.go` (recurring, contracts,
  timesheets, expenses, purchase orders), `ui_messages.go` (catalogue), `ui/templates/*.html`,
  `ui/static/*` (embedded). Mounted by `NewApp` next to the API when `ui.enabled`; `/v1/`, `/public/invoices/{token}`
  and `GET /healthz` still go to the API.
- Small, read-only Store additions so the UI never touches SQL: `Dashboard` (aggregates), `ListSeries`,
  `PartyNames` (names only, no contact data, scoped, not an audited contact read).
- Shared with the API rather than copied: `allowedBy` (the guard's scope rule), `serveInvoiceHTML` (printable
  invoice and headers), `csvWriter` (CSV headers), `encodeCursor`/`decodeCursor`, `clientIP`, the rate limiter.
- Fix found on the way: the access log wrote `/public/invoices/{token}` paths, i.e. working links; it now logs
  `/public/invoices/{token}` (`logPath`).

## Constitution check

| Principle | Check |
|---|---|
| I Small and honest | No JavaScript, framework or dependency; templates and CSS embedded; templates parse on first use so idle RAM equals the API-only build (17.2 MiB idle, 27.9 MiB after load, budget 20/48 unchanged, `research.md` R2). Off by default. |
| II Open and replaceable | Apache-2.0; plain `html/template`; the UI is a client of the documented Store and replaceable by another client. |
| III Test first | Tests were written before the handlers and seen to fail (`undefined: ParseMinor`, then each page); golden HTML for 49 pages, a11y checklist, contrast, catalogue, route-contract, scope and source-guard tests. |
| IV Secure by default | Deny by default through scopes and the Store; CSRF, CSP, `no-store`, `__Host-` cookies, failed-login throttle, hashed session ids, revocation re-check, SoD enforced in the Store, tokens never logged. |
| V Sensitive data in-house | No data leaves the module; the UI adds no store; contact reads and exports are audited by the Store; sessions never persist the token. |
| VI Honest status | Experimental, not browser- or cluster-verified; gaps listed in `tasks.md`, `docs/finance-ui.md` and the PR. |

## Design decisions in brief

1. Session: server-side, in memory, SHA-256 of a 256-bit id; bounded (5000, 5 per subject); privileged sessions re-check every minute.
2. A route table carries `{pattern, scope, own, handler}`; one wrapper applies session, rate limit, CSRF, scope, error mapping.
3. Redirect targets after POST are fixed local paths built from ids the Store returned, never from request input; flash text is an allow-list of catalogue keys.
4. The public link is rendered in the POST response, once; there is no GET that can show it again.
5. Line editor = repeated named inputs (`desc`, `qty`, `price`, ...), aligned by index; blank rows are skipped; no JavaScript.
6. Timesheet grid = one `h` input per weekday per row (H:MM or hours); a save replaces the week (the Store's `SetEntries`), with the version in the form.
7. Error pages share one template and never mark the section the path belongs to; every failure lists the Store's own validation problems.

## Testing

`go test -race ./core/finance/...` including: sessions and revocation, secure cookies, CSRF and fetch metadata,
invoice lifecycle with SoD and double submit, immutability, credit note cap, void rules, public link show-once and
revoke, printable view equals the renderer's output, hostile text in every context, party scope 404 equals missing,
own-only tokens, erasure report, expenses with receipt metadata and CSV injection and audit, timesheet grid and
period lock, contracts from templates, recurring, purchase orders, reports, dashboard aggregates, bounded lists,
route contract with scope enforcement for every route, catalogue completeness, a11y checklist, contrast, goldens
(`-update` rewrites them), source guard, log redaction.
