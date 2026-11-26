# finance

In-house finance module: parties, invoices and credit notes, payments, aging, recurring invoices,
contracts with signature by emailed link (a simple electronic signature, `specs/049-contract-esign/`) or an
e-signature adapter, timesheets, expenses and minimal purchase orders. Service
`cmd/can-finance`, library `core/finance`, SQLite. Capabilities `invoices`, `contracts`,
`timesheets`. Status `experimental`: built and tested offline; the chart installs through can-core's
installer in unit tests; not yet run on a cluster.

* Installed under can-core (`docs/module-wiring.md`): can-core generates the admin token and the link
  secret once; personal access tokens are introspected through can-core (`finance.read`, `.write`,
  `.approve`, `.admin`); events go to can-core. **Invoices are approved through can-core's approvals
  engine:** a person with `finance.write` asks (`POST /v1/invoices/{id}/request-approval`), approvers
  decide in can-core, and `POST /v1/invoices/{id}/approve` then needs an approved request for the
  invoice's current version from someone other than the requester (editing the invoice invalidates it).
  Timesheets, expenses, purchase orders and voiding still use the `finance.approve` rule.

* User documentation: `docs/finance.md` (money and rounding rules, immutability, retention and
  erasure, access model, events).
* Specification: `specs/014-finance-basics/`.
* Image: `deploy/finance/` (digest placeholders until release).
* A tool, not accounting, tax or legal advice.
