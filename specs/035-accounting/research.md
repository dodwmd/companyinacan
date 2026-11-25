# Research: Accounting interfaces (E15c)

**R1 Scope: interface layer, not an engine.** SPEC.md lists "full accounting engines" under integrate-do-not-build. What an operator
needs from the in-house module is a trustworthy journal of the module's own events, a way to reconcile bank money against invoices
and a file to hand to the accounting system they use. The slice stops there; tax, payroll, revaluation, year end and multi-entity are
the accounting system's job and are listed as gaps.

**R2 Single currency per entry.** A multi-currency entry needs an FX policy (rate source, rounding of the difference, an exchange
gain or loss account). Each of those is a jurisdiction-flavoured decision. One currency per entry keeps the balance check a
plain sum, lets the database enforce it, and reports are per currency with no conversion. The invoice's stored FX rate is not used.

**R3 Where the balance check lives.** SQLite triggers can check at statement time, not at commit. Inserting an entry as a draft,
adding lines, then updating `status` to `posted` gives one statement at which the whole entry exists, so a `BEFORE UPDATE OF status`
trigger can verify count, balance and period. `BEFORE INSERT` refuses a row that is already posted. Tested with raw SQL that
bypasses the store (`TestUnbalancedEntriesAreRefusedByStoreAndDatabase`).

**R4 Trigger columns.** `UPDATE OF col` fires when the column is listed in `SET`, not when its value changes; the first version of
`UpdateAccount` listed `code` on a rename and was refused by its own trigger. The store now names only the columns that change.

**R5 Payment path.** The bank confirm needs the payment, the invoice status, the `invoice.paid.v1` event and the journal entry to
agree. Calling `recordPaymentTx` (with the new unexported `cashAccount` on `PaymentInput`, invisible to the JSON API) does all four in
one transaction and reuses the idempotency on `(source, external_id)`: the external id is `bank:<account>:<transaction id>`.

**R6 Statement parsers.**
- CSV: `encoding/csv`, `FieldsPerRecord = -1`, `LazyQuotes` off. Delimiter detection is a source of silent mis-parsing, so it is off: the caller states it.
  Decimal text is read digit by digit; the number of fraction digits is checked against the currency exponent (extra zeros allowed, extra non-zeros refused), so `10.5` yen is an error, not 11 yen.
- OFX 1.x: a hand-written tag scanner. Leaves are `<TAG>value` up to the next `<`; aggregates have whitespace only. Any `<!` rejects the file, which removes DOCTYPE and ENTITY at the source; the five predefined entities are replaced in one `strings.Replacer` pass (not re-scanned), and anything else that looks like an entity is refused.
- CAMT.053: `encoding/xml` in strict mode with `Entity = nil`, no `CharsetReader` (UTF-8 only), a pre-scan for `<!DOCTYPE` and `<!ENTITY` (case-insensitive) and an explicit depth cap. Go's decoder does not expand custom entities anyway; the pre-scan makes the refusal explicit and cheap.
- All: 2 MiB, 5000 rows, 1000-byte fields; control characters stripped from text.
- Fuzzed (`FuzzParseCSV`, `FuzzParseOFX`, `FuzzParseCAMT053`, `FuzzPayrollCSV`, `FuzzAmountToMinor`) for 20 seconds each, 70 thousand to 1.4 million executions each: no panic, no invariant broken.

**R7 Dedup keys.** Bank ids are unique per account in the table (`UNIQUE (bank_account_id, txn_key)`) with `INSERT ... ON CONFLICT DO NOTHING`; a file that repeats an id with different content is rejected (a corrupt file). Rows without an id get `h:` plus a hash of their content and their occurrence number among identical rows in the file, so two identical rows both import and a re-import produces the same keys. The whole file also has a SHA-256 per account.

**R8 Seal key and AAD.** `CAN_FINANCE_SEAL_KEY` is a plain 32-byte key (the module has no keyring today, unlike people and jobs). Rotation would need a re-seal command and a key id; since the only sealed data is a number used to check a statement, the key is generated once and `NoRotate`. The bank account row id is the additional data, so a copied value does not open. The UI and API show only the last four characters, kept in clear in `last4`.

**R9 Footprint (measured 2026-11-25, linux/amd64 static build `CGO_ENABLED=0 -trimpath -ldflags "-s -w"`, local).** Method (script not kept in the repo): start with
a temp database and `ui.enabled: true`, rate limits off; read `VmRSS`/`VmHWM`; apply the default chart, create and approve 200 invoices
(each posts a journal entry); 400 list and aging reads; 800 ledger report reads (trial balance, balance sheet, journal, general ledger);
add a bank account and import a 5000-row CSV statement; list transactions; export; sign in and render 8 pages 100 times each including the accounting pages.

| State | VmRSS |
|---|---|
| idle | 18.5 MB (18484 kB) |
| after 200 invoices created and approved | 25.2 MB |
| after API and ledger report load | 28.1 MB |
| after the 5000-row import and an export | 30.8 MB |
| after 800 page renders | 32.5 MB (32460 kB, 31.7 MiB) |

Against the budget 28 MB: 31.7 MiB is under 28 x 1.2 = 33.6, so `ram_mb: 28`, `ram_limit_mb: 64` stay (the same rule as `specs/031-finance-ui` R2). The margin is small: the import holds the
2 MiB file, its parse and the rows at once, and a single heavy run is not a distribution. Not measured on a cluster.

**R10 Retention.** Journal entries are not deleted by the purge: they have no personal data (memos carry document numbers), and removing old entries would change every
later balance. Decided bank transactions are deleted with the retention period of their booking date, after being unlinked from the payment the
invoice purge deletes; unmatched ones are kept. Documented as a decision for the operator and their accountant, not as legal advice.

**R11 Known weaknesses.** Documents approved before the mappings are set are not back-filled: the cut-over posts their open balances (`docs/accounting.md`); other opening balances are a manual journal.
`period_locks` (timesheets and expenses) and `ledger_closes` are separate on purpose (a payroll cut-off and an accounting close are different decisions) but a person must set both.
An approval into a closed period fails rather than moving the date.
