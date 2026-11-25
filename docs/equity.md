# Equity (cap table and employee stock options)

The in-house `equity` module: `cmd/can-equity` over library `core/equity`, image `deploy/equity`,
manifest `modules/equity/module.yaml`. "Stock" here means employee stock options and equity, not inventory.

> **This is a tool, not legal, tax or securities advice.** It records what you tell it and computes
> vesting, ownership and round figures from that data by documented simple formulas. **Not implemented:**
> US 409A valuation rules, ISO and NSO limits (including the ISO USD 100,000 first-exercisable rule),
> tax treatment of grants, exercises and sales, 83(b) elections, ESOP scheme rules of any country,
> securities exemptions, share-register law, and the legal text of any plan or SAFE. Where a rule
> matters, the module offers a data field (for example a valuation record or free-form notes) and a
> documented limit, never an enforcement. Equity law differs by jurisdiction and mistakes are expensive:
> have counsel review plans, grants, conversions and the figures this tool produces.

Status: **experimental**. Unit-, contract- and race-tested offline; not yet exercised on a cluster. See
"Not implemented, limits and gaps" at the end.

## What it holds

Legal entities; share classes (common or preferred, par value, authorised shares); equity plans (pools);
stakeholders (founders, employees linked by an opaque directory person id, investors, advisors); option
grants (ISO, NSO, RSU, SAR as an enumeration, free-form notes); vesting schedules; exercises; an
append-only share ledger (issuances, transfers, cancellations, repurchases, exercise issuances);
convertible instruments (SAFEs and notes) as data; valuation records (409A and others) as data; cap-table
views as of any date; illustrative round modelling; audited CSV exports.

Quantities are exact decimals held as integers of 1/10000 of a share (at most 4 decimal places, at most
100 billion shares), transported as decimal strings. Prices are integer minor units of the entity's
currency. No floats anywhere.

## Roles and access

Roles come from the caller's token: `employee`, `manager`, `hr`, `finance`, `board`, `admin`. A caller
with no known role is blocked. Deny by default: the single policy layer (`core/equity/policy.go`,
`Decide` and `FieldAccess`) is the only place that looks at roles. An employee is linked to their own
stakeholder record by the directory person id the token carries (`person_id` in the introspection
answer); "own" means that link.

A caller who may not see a record gets 404 (no existence oracle); a caller who may see it but not do the
action gets 403.

| Action | employee | manager | hr | finance | board | admin |
|---|---|---|---|---|---|---|
| Own grants, vesting, identity, exercises (self-service) | yes | yes (own only) | | | | |
| Someone else's grant, vesting, stakeholder | no | **never** (not even reports) | yes | yes | yes | yes |
| List grants of an entity | no | no | yes | yes | yes | yes |
| Create, edit, submit, amend, cancel, terminate, activate grants | no | no | yes | no | no | yes |
| Approve a grant (board step) | no | no | no | no | **yes** | no |
| Entities, share classes, plans | no | no | read | read | write | write |
| Create stakeholders, change identity | no | no | yes | yes | yes | yes |
| Request an exercise | own | own | on behalf | on behalf | no | on behalf |
| Approve or reject an exercise | no | no | no | yes | yes | yes |
| Record exercise payment | no | no | no | yes | no | yes |
| Issue shares for an exercise | no | no | yes | yes | no | yes |
| Ledger entries (issue, transfer, cancel) | no | no | no | yes | no | yes |
| Read ledger, cap table, what-if, exports | no | no | no | yes | yes | yes |
| Convertibles, valuations | no | no | no | yes | yes | yes |
| Legal hold, erasure, retention run, tombstones | no | no | no | no | no | yes |

The can-core service-account token carries `hr`, `finance` and `admin`; never `board`. So the service
account cannot approve a grant.

Per-field access (sensitive fields are read and written per field, and reads are audited per field):

| Field | Read | Write |
|---|---|---|
| `stakeholder_identity` (name, email, tax id) | the holder (own), hr, finance, board, admin | hr, finance, board, admin (never the holder) |
| `grant_terms` (quantity, strike, schedule) | the holder (own, once approved), hr, finance, board, admin | hr, admin, drafts only |
| `grant_notes` (internal free text) | hr, board, admin (never the holder) | hr, admin, drafts only |
| `holdings` (what a holder owns) | the holder (own), finance, board, admin | finance, admin |
| `valuation`, `convertible_terms` | finance, board, admin | finance, board, admin |
| `payment_reference` | finance, admin | finance, admin |

A grant that is still a draft or pending approval is not yet an offer: the holder gets 404 for it.

### Separation of duties

One function (`sodOK`) decides it for everything. Subjects are compared after Unicode NFKC normalisation,
removal of invisible format characters, trimming and case folding, so "Alice", "alice " and a zero-width
variant are one person.

- A grant cannot be approved by the person who created, edited or submitted it, nor by the grantee
  (compared by subject and by directory person id). The board role is required; `admin` is not enough.
- An exercise cannot be approved or paid by its requester or the holder; shares cannot be issued by the
  requester, the holder or the person who recorded the payment.
- The same rules are repeated as database triggers (`migrations/002_immutability.sql`) so a bug in the
  service layer cannot bypass them.

## Grant lifecycle and immutability

`draft` then `pending_approval` then `approved` then `active`, and `cancelled`, `forfeited` or `expired`
at the end. Terms (type, quantity, strike, currency, dates, schedule, expiry, exercise window, holder,
plan, version) are editable only in `draft` and frozen by a trigger afterwards. To change an approved
grant, amend it: that creates a new draft version (`supersedes_id`, `version + 1`); the old grant is
cancelled in the same transaction when the new version is approved. A grant that already has exercises
cannot be amended (cancel the rest and grant again).

- Submit checks the plan's pool; approval checks it again inside the transaction, so two pending grants
  can never together exceed the pool.
- Activate records that the agreement was accepted; exercises need an active grant.
- Terminate records the leaving date. Vesting stops on it (inclusive). Without `for_cause` the vested
  part stays exercisable until `termination date + post_termination_exercise_days` (or the expiry date if
  earlier), then the periodic sweep expires the grant. With `for_cause` the grant is forfeited at once
  (a simplification: real plans differ).
- Milestone achievements and acceleration trigger events are append-only facts recorded separately.

### Approval engine seam

`Options.Approvals` takes an `ApprovalEngine` (`Request`, `Status`). When set, submitting a grant (type
`grant`) or requesting an exercise (type `exercise`) is also registered with the engine, and approving
requires the engine to report `approved`; its decider becomes the approver of record and is bound by the
same separation of duties. Without an engine, the `board` role approves grants directly in this service
and `finance`, `board` or `admin` approve exercises. The manifest declares both approval types. **The
interface is not wired to can-core yet** (no adapter exists; only tests use a fake).

## Vesting calculator

A pure function (`core/equity/vesting.go`): the same input always gives the same answer; it reads no
clock. Inputs: total, vesting commencement date, schedule, as-of date, optional termination date,
achieved milestones, acceleration trigger events.

- Periodic schedules: `months` in total, `cliff_months` (nothing vests before it; at the cliff the
  cumulative amount for the months elapsed vests), `interval` monthly or quarterly (months and cliff must
  be multiples of 3 for quarterly). Maximum 600 months.
- Month counting adds whole months to the commencement date and clamps the day to the last day of the
  target month: a grant starting on the 31st vests on the last day of shorter months; a 29 February start
  vests on 28 February in common years and 29 February in leap years. A month counts on its anniversary
  date, not before.
- Tranche schedules: dated tranches and milestone tranches (vest when recorded as achieved, on or before
  the as-of date) with basis points summing to 10000.
- Termination cut-off: vesting is computed as of `min(as-of, termination date)`; a vesting date equal
  to the termination date counts.
- Rounding policy (`rounding`): the cumulative amount is rounded, never each instalment, so the total is
  exact and the result never decreases. `floor` (default): whole shares rounded down, the last
  instalment takes the remainder; `nearest`: whole shares, half up (clamped to the total); `fractional`:
  four decimal places rounded down.
- Acceleration clauses are data: `single` needs one trigger event, `double` needs two (any order). When
  the last trigger has happened, `percent_bp` of what was still unvested vests at that date. A clause
  whose date is after the termination date applies only if `applies_after_termination` is set. The
  service does not decide whether a transaction is a change of control; the person recording the event
  does, and it is audited.
- Properties tested (including a fuzz target): never negative, never above the total, never decreasing
  as the date moves forward, nothing before the start, everything after the term.

Exercisable quantity = vested minus exercised minus quantity reserved by open exercise requests, inside
the exercise window, on an active grant. Exercise cost = quantity x strike, rounded half away from zero to
the minor unit. SARs cannot be exercised through the service (see limits).

## Pool and cap table

Pool for a plan as of a date: authorised, granted outstanding (approved grants not yet cancelled,
forfeited or expired, minus exercised), exercised (issued shares), available, and the amount pending
approval. A lapsed grant releases its unexercised part. Plans of a class cannot reserve more than the
class authorises.

The cap table is computed as of any date from the ledger (effective dates) and the grants (approval,
termination and terminal dates). `basic` shows issued shares by holder and class; `fully-diluted` adds
outstanding options (vested and unvested) and the available pool. Ownership is in parts per million,
allocated by the largest remainder method so the parts always add up to exactly 1,000,000. Warnings are
returned when issued shares plus options and pool exceed a class's authorised shares. Names are included
only on request and with the identity permission.

The ledger is append-only (triggers refuse UPDATE and DELETE): correct a mistake with the opposite
entry. A holder's balance can never go negative on any date (a backdated removal is checked against the
whole history), a class can never have more shares in issue than authorised, and entries cannot be dated
in the future.

## Round modelling (illustrative)

**Everything here is illustrative.** A documented simple formula over the recorded data, not a legal
conversion calculation. Real SAFE and note terms (pro-rata rights, MFN, conversion mechanics, day counts,
which securities count in the capitalisation) differ per instrument and per lawyer. The result is
labelled `illustrative` with a notice, and nothing computed is ever written to the ledger.

```
S      = fully diluted shares before the round, plus any option-pool top-up, excluding
         the converting instruments below.
P      = pre_money / S                              (round price per share)
amount = principal (+ simple interest for a note: principal x rate x days / 365)
discount price  D = P x (1 - discount)              (D = P without a discount)
pre-money SAFE and convertible note:  price = min(D, cap / S);  shares = amount / price
post-money SAFE: the holder owns amount / cap of the capitalisation C that includes every
         post-money SAFE, so C = (S + shares of the other instruments) / (1 - sum of amount/cap)
         and shares = amount / cap x C; if the discount gives more shares (amount / D), that
         wins and the instrument leaves the cap group; repeated until stable.
new investor shares = investment / P
```

Shares are rounded down to 4 decimal places; prices are shown to 6 decimal places. Post-money SAFEs whose
amounts reach their caps are refused. Convertibles are recorded as data (terms sealed) and are never
converted by the service: conversion is a legal event you record as a ledger issuance after it happened.

## Encryption and key rotation

Sensitive columns are sealed with XChaCha20-Poly1305: stakeholder name, email and tax id; grant notes;
exercise payment references; convertible terms; valuation price and notes. The additional data binds
each ciphertext to its row and field (for example `stk_...:name`), so a value copied to another row or
field fails to open. The keyring is a mounted file of lines `<key-id> <64 hex chars>`, newest first,
mode 0400 or 0600, a regular file (not a symlink), owned by the service user or root; the service
refuses to start otherwise and the Secret must be mounted with `subPath`.

Rotation: add a new key at the top of the keyring, restart, run `can-equity rotate-keys`. Every value is
re-sealed with a compare-and-swap (the update applies only if the stored ciphertext is still the one
that was read), so a concurrent write is never overwritten; values that changed meanwhile are retried.
When it reports 0 re-sealed values, remove the old key.

## Audit

Every write, every denied attempt (throttled per actor so it cannot fill the log) and every read of a
sensitive field is recorded in an append-only table (triggers refuse UPDATE and DELETE) whose rows are
hash-chained: each hash covers the previous one. A read that returns many records (lists, cap table,
exports) is audited per record in one transaction. Reads are audited BEFORE the data is returned: if the
audit cannot be written, nothing is returned (fail closed). The log holds actor ids, action, resource ids
and field names only, never names, amounts or notes. `can-equity verify-audit` recomputes the chain. It
detects accidental damage and edits by anyone who does not rewrite the whole chain; to defend against a
database administrator, keep the latest hash somewhere they cannot write.

## Retention, legal hold and erasure

| Class | Default | Holds |
|---|---|---|
| `equity-ledger` | legal | issuances, transfers, exercises, grant terms, convertibles, structure |
| `equity-stakeholder` | 7y | name, email, tax id, person link |
| `equity-grants` | 7y | grant notes, payment references, draft grants |
| `equity-valuation` | 7y | valuation price and notes |
| `equity-audit` | legal | the audit trail |

Defaults are starting points to review against local law, not advice. The ledger and audit classes can only
be configured as `legal`.

**Legal retention overrides erasure.** Issued share records, approved grants and exercises are part of the
legal record and are kept. Erasure therefore pseudonymises: the stakeholder's name, email, tax id and
person link are removed and the row stays as an opaque id (`[erased]`); free-form grant notes and payment
references are cleared; draft grants and cached idempotency keys are deleted; open exercise requests are
cancelled. The response lists what was retained and why. Erasure is refused while a legal hold is set
(admin sets and lifts holds) and while the stakeholder has pending, approved or active grants (cancel or
end them first). It writes tombstones (`stk_` subject, classes `equity-stakeholder` and `equity-grants`)
that can-core pulls into the E08 erasure ledger so a restore re-applies the erasure, then checkpoints and
truncates the WAL and runs VACUUM (secure delete is on). Earlier backups keep the data until they expire.
`can-equity purge -subject stk_... -class all` is the replay hook; it is idempotent and refuses the legal
classes.

The retention sweep (hourly, or `can-equity retention`) pseudonymises stakeholders with no live grants,
no recent activity and no hold after the period, clears old notes and payment references of terminal
grants, and clears sealed valuation prices after the period. It never touches the ledger.

## Events

All carry ids and dates only (never names, quantities, prices or valuations); schemas in
`schemas/events/`, fixtures in `testdata/events/`. Signed webhook delivery with retries, at least once.

- `equity.grant.approved.v1`: `grant_id`, `stakeholder_id`, `entity_id`, `plan_id`, `approved_on`.
- `equity.grant.vested.v1`: `grant_id`, `stakeholder_id`, `entity_id`, `vested_through`. Announced by the
  periodic sweep when more has vested than was last announced; one event per increase seen by a sweep, so
  several instalments between two sweeps give one event.
- `equity.exercise.recorded.v1`: `exercise_id`, `grant_id`, `stakeholder_id`, `entity_id`,
  `recorded_on`. Emitted when the shares are issued into the ledger.

## Configuration

`/etc/can-equity/equity.yaml` (strict: unknown keys are errors):

```yaml
listen: ":8081"
db_path: /data/equity.db
keyring_file: /etc/can-equity/keys/keyring   # required
webhook: { url: "" }                          # can-core event endpoint; needs CAN_EQUITY_WEBHOOK_SECRET
introspect: { url: "", allow_insecure_http: false }
retention: { equity-stakeholder: 7y, equity-grants: 7y, equity-valuation: 7y }
```

Environment: `CAN_EQUITY_API_TOKEN` (at least 24 characters, the service account), `CAN_EQUITY_WEBHOOK_SECRET`,
`CAN_EQUITY_INTROSPECT_TOKEN`, optional `CAN_EQUITY_CONFIG`, `CAN_EQUITY_KEY_FILE`. The introspection
endpoint answers `{active, subject, roles, person_id}`; unknown roles are dropped and a malformed
`person_id` fails the request. Failed authentications are rate limited per address; a request with a
valid credential is never refused because of what else the same address sent. Request bodies are
limited to 64 KiB and strict JSON; database work has deadlines and list sizes are capped.

Commands: `can-equity serve | check-config | healthcheck | rotate-keys | retention | verify-audit |
purge -subject <stk id> -class <class|all> | version`.

Create endpoints that make records (grants, exercises, ledger entries) honour an `Idempotency-Key`
header: the same key and body replays the first result (200), the same key with another body is a 409.
The table stores resource ids only, so a replay re-reads the record under the caller's current rights.

## Not implemented, limits and gaps

- **Not implemented, by design:** 409A or any valuation methodology and the rule that a strike must be at
  or above fair market value; ISO and NSO limits and qualification; tax treatment and withholding; 83(b);
  ESOP and country scheme rules; securities exemptions; share-register and filing law. Valuation records
  are data only; nothing checks a strike against them or expires a grant because a valuation expired.
- SAR cash settlement is not modelled (exercise requests on a SAR are refused). RSU settlement is modelled
  as an exercise with a zero strike. Partial payment and net or cashless exercise are not modelled: the
  payment must equal quantity x strike exactly.
- Grant terms (quantity, strike, schedule) are stored unsealed so that pool and dilution queries and the
  immutability triggers work; only identity, notes, payment references, valuations and convertible terms
  are sealed. Protect the volume and backups accordingly.
- Strikes are integer minor units: sub-minor-unit strikes (for example USD 0.0001) cannot be represented.
- No per-subject crypto-shredding: erasure pseudonymises and clears; backups keep older data until they expire.
- No live can-core wiring: token introspection routing, the keyring Secret and the approval engine
  adapter are not injected or implemented; the Helm templates do not exist yet (values only).
- The vesting event only announces increases seen by a sweep and carries no amounts.
- Fully diluted figures count approved grants from their approval date; convertibles are shown by the
  what-if model only, not in the cap table; the model is illustrative.
- A for-cause termination forfeits everything unexercised; real plans differ. Acceleration is data plus
  recorded triggers.
- No admin UI, no investor or employee portal, no `board` or `entity` modules yet (later in E17), no
  import of Captable files, no PDF export.
- Not verified on a cluster; no load or soak measurements beyond the RAM figure in the module README.
