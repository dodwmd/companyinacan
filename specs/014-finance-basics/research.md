# Research: Finance basics (Epic E15a)

**R1 Wrap or build.** SPEC 5 prefers a wrapped OSS app when one passes the licence and footprint check. Invoice Ninja (Elastic
License 2.0 as of v5, to be re-verified) and Akaunting (source-available terms) do not meet the OSI rule; InvoiceShelf (AGPL) and
ERPNext (GPL, heavy) pass the licence but not the footprint or the shared-party-model needs, and none gives per-record scoping,
separation of duties and read auditing in the shape section 3.7 requires. Decision: in-house and lean, behind the `invoices`
capability so an adapter can replace it.

**R2 Money.** `int64` minor units with an ISO 4217 exponent table; quantity milli-units, discount basis points, tax parts per
million, FX 1e-8 fixed point. Multiplications use `math/big` and are range-checked; per-document cap 2^50 minor units. Rounding is
half away from zero. Rejected: floats (inexact), a decimal library (a new dependency for a few operations), banker's rounding
(surprising on receipts, no stronger legal footing).

**R3 Numbering.** Gap-free numbers need the counter to commit with the document. `INSERT ... ON CONFLICT DO UPDATE ... RETURNING`
inside the approval transaction on the single write connection; a rollback restores the counter. Numbers are given at approval,
not at creation, so deleted drafts leave no gap. `UNIQUE (series, number)` and a CHECK tie "has a number" to "is not a draft".

**R4 Immutability.** Enforced twice: store rules (409) and SQLite triggers on `invoices`, `invoice_lines`, `payments`,
`contract_versions` and `audit_log`. The retention purge opens a one-row gate for its own transaction only (and names the audit
cut-off, never younger than 365 days). Someone with schema access can drop triggers; contract `verify`, the stored hashes, backups
and the audit trail are the defence beyond that.

**R5 Scope.** One helper per dimension returns a parenthesised clause joined to the query with AND, so an OR inside a filter cannot
widen it (an earlier sibling had exactly that bug). No handler composes scope SQL by hand, and tests request another party's data
through every filter. An out-of-scope id is a 404. Timesheets filter entries to the caller's projects and require full visibility
to decide a sheet.

**R6 Separation of duties.** One function (`authorizeApproval`) checks the permission, the creator, the last editor and the owner,
then the pluggable `Approver`. Invoices, timesheets, expenses and purchase orders all call it; the bootstrap token is not exempt.

**R7 Public link.** `base64url(id.epoch.expiry).base64url(HMAC-SHA256(secret, tag+payload))`; the epoch stored on the invoice
revokes by increment; no database access is needed to reject a forged or expired token; every failure returns the same 404. Views
are audited, coalesced to one per invoice per ten minutes so a valid link cannot flood the log. Bad links are throttled per address;
valid ones never.

**R8 Footprint (measured 2026-11-25, linux/amd64 static build, local, `CGO_ENABLED=0 -ldflags "-s -w"`).** Idle RSS 16.1 MiB
(`VmRSS` 16440 kB, 3 s after start); after 200 invoice creates, 200 list reads (100 per page) and 200 aging reports, 24.8 MiB
(25388 kB). Binary 15 MB. Manifest: `ram_mb: 20`, `ram_limit_mb: 48`. Not measured on a cluster.

**R9 Adapters.** `PaymentProvider` and `ESigner` are small interfaces; the fakes verify Stripe-style `t=,v1=` HMAC signatures with a
5-minute tolerance and use the reserved `.invalid` TLD for URLs, so no call can leave the machine. Real adapters (Stripe,
Documenso) are separate, later modules; the Documenso path is described in `docs/finance.md`.

**R10 Erasure versus retention.** Pseudonymise the party row; keep issued documents with the snapshot they were issued with (name,
address, tax id; never email or phone); report what is kept and until when; truncate the WAL; delete when retention ends. The
retention clock for a document is its own date, not the date of the erasure request.

**R11 Time arithmetic.** Minutes are summed per task, then converted once to milli-hours with the same rounding rule. Month steps
for recurring schedules clamp to the month end and do not remember the anchor day (documented; days 1 to 28 are stable).
