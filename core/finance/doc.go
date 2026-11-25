// Package finance is the in-house finance module (SPEC.md section 3.3, "invoices",
// "contracts", "time", "expenses", "vendors"): customers and vendors, a product and tax
// catalogue, invoices and credit notes with gap-free numbering and legal-style
// immutability, payments, aging, recurring invoices, contracts with versions and
// e-signature, timesheets, expenses and purchase orders, behind a REST API with scoped API
// tokens and signed webhook events. It backs the `invoices`, `contracts` and `timesheets`
// capabilities. It is a tool, not accounting, tax or legal advice.
package finance
