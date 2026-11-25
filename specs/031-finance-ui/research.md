# Research: Finance web UI (E15b)

**R1 Why server-rendered Go templates.** Same decision as `specs/026-people-ui` R1: the module stays one static
binary, there is no build step, no JavaScript to audit, and a phone is served by plain forms. A lines editor without
JavaScript is repeated rows (three blank rows offered; "add more" is: save, edit again).

**R2 Footprint (measured 2026-11-25, linux/amd64 static build, local, `CGO_ENABLED=0 -trimpath -ldflags "-s -w"`).**
Method: start the binary with a temp database and `ui.enabled: true`, wait 3 s, read `VmRSS`/`VmHWM` from
`/proc/<pid>/status`; then 200 invoice creates and 200 list reads (100 per page) and 200 aging reports through the
API; then sign in through the UI and render `/`, `/invoices`, `/parties`, `/approvals`, `/reports/aging`, `/settings`,
`/timesheets` 100 times each (700 renders) and create 100 parties through the form; then one CSV export.

| State | UI off | UI on, templates parsed at start (first try) | UI on, parsed on first use (shipped) |
|---|---|---|---|
| idle | 17580 kB | 21352 kB | 17624 kB |
| after API load | 26548 kB | 31728 kB | 26708 kB (second run 25840 kB) |
| after UI load | - | 31072 kB | 28576 kB |

Parsing 31 page templates (each with the layout) at start cost about 3.8 MB of heap on a module budgeted at 20 MiB, so
templates are parsed on first use; idle is then the same as with the UI off, and a fully used UI adds about 2 MiB to the
loaded figure. Loaded RSS ranged 27.9 to 29.9 MiB over runs, above 20 x 1.2, so the budget is raised to `ram_mb: 28` and `ram_limit_mb: 64` (as the work UI); `module.yaml`, the chart values and `test/contract/finance_test.go` agree. The earlier figure of `specs/014-finance-basics` R8 (16.1 MiB idle) was measured
with an older toolchain and fewer dependencies; today the API-only build idles at 17.2 MiB on the same machine. Not
measured on a cluster.

**R3 Money.** `FormatMinor` (existing) prints integer minor units with the currency exponent; `ParseMinor` is its
inverse (digits, one optional point, at most the exponent in decimals, at most 18 digits, then the service's cap).
Quantities are milli-units (3 decimals), discounts basis points (2 decimals of a percent). `1e3`, `1,5`, `NaN`, `0x10`,
`1_000` and spaces inside are refused with a message; nothing is rounded silently.

**R4 Separation of duties in the UI.** Hiding a button is not a control. The page computes `selfMade` from the record's
creator, last editor and owner with `sameSubject` (the Store's folding of Unicode and case) to disable the button and
explain; the Store still decides. A test posts the forged request and expects 403. The queue leaves such records out so
the list is the person's real work.

**R5 Public links.** The token is an HMAC-signed, expiring value the Store issues. The UI shows it in the response to the
POST that creates it, with `no-store`; it is not redirected (a redirect URL would land in history and logs). The access
log used to record the full path of `/public/invoices/{token}`; fixed (`logPath`) because anyone who could read the log
could open invoices. Revocation bumps the invoice's link epoch.

**R6 Idempotency keys.** A key made when the form is rendered (128 random bits) is carried in a hidden field. A missing
or malformed key is a 422, not a server-made substitute (which would defeat the purpose). The Store replays the original
record for the same key and request, and refuses the same key with different content (422).

**R7 Approver queue and counts.** The queue asks the Store for drafts, submitted sheets and claims (each page of 50) and
removes what the caller made, edited or owns. A per-currency total is never produced across currencies.

**R8 Erasure.** The Store's `ErasePartyData` already returns a report; the page prints it as is. It also deletes drafts,
so the confirm field must read `ERASE`. The retained list is the legal-retention outcome, not a failure.

**R9 Why `PartyNames`.** Reports need a customer's name only. `GetParty` returns contact data and is an audited read; calling
it per row would write an audit entry per customer and hold PII the page does not show. `PartyNames` is scoped by the
caller's parties and returns names only.

**R10 Sessions and the token.** Same trade-off as `specs/026-people-ui` R2: the token is held in memory to detect
revocation; the interval is shorter (60 s) for tokens that can approve or administer. Restarting signs everyone out.
