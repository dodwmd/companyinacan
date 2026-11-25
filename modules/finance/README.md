# finance

In-house finance module: parties, invoices and credit notes, payments, aging, recurring invoices,
contracts with e-signature, timesheets, expenses and minimal purchase orders. Service
`cmd/can-finance`, library `core/finance`, SQLite. Capabilities `invoices`, `contracts`,
`timesheets`. Status `experimental`: built and tested offline, not yet run on a cluster.

* User documentation: `docs/finance.md` (money and rounding rules, immutability, retention and
  erasure, access model, events).
* Specification: `specs/014-finance-basics/`.
* Image: `deploy/finance/` (digest placeholders until release).
* A tool, not accounting, tax or legal advice.
