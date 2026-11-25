# Feature Specification: Equity and cap table, first slice (Epic E17a)

**Feature Branch**: `epic/E17a-equity` (spec dir `018-equity`)

**Created**: 2026-11-25

**Status**: Draft (implemented offline; cluster verification pending)

**Input**: Epic E17 (`equity`, `board`, `entity`) in `docs/epics/README.md`, and SPEC.md sections 3.7
(RBAC, retention and legal hold, audit, erasure semantics, append-only stores), 4.3 (`equity`
capability, event catalogue), 5 (sensitive data) and the *Stock and options (cap table)* design. This is
the first, deliberately small slice: the in-house `equity` service. `board` and `entity` modules, the
investor and employee portals and the can-core wiring are later slices. "Stock" means employee stock
options and equity, not inventory.

> **This is a tool, not legal, tax or securities advice.** It records equity facts and computes vesting,
> ownership and round figures from that data by documented simple formulas. **Not implemented:** US 409A
> valuation rules, ISO and NSO limits, tax treatment, 83(b) elections, ESOP scheme rules of any
> jurisdiction, securities exemptions and share-register law. These are modelled only as data fields
> (valuation records, free-form notes, enumerations) and documented limits. Retention defaults are
> starting points. Equity law differs by jurisdiction and mistakes are expensive: the operator is
> responsible for compliance and should have counsel review plans, grants and any conversion.

Everything is built and verified offline: no cluster, no container runtime. What that leaves unverified
is listed in [research.md](research.md) and `tasks.md`.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - The board approves option grants with separation of duties (Priority: P1)

HR drafts a grant for an employee against a plan's pool; submitting it freezes the terms; a board member
who did not create, edit or submit it, and is not the grantee, approves it; HR activates it once the
agreement is accepted. An approved grant cannot be edited: amending creates a new version.

**Why this priority**: the approval step and the immutability are what make a grant a record rather than
a draft, and Constitution V requires strict access before any equity data ships.

**Independent Test**: `go test ./core/equity/...` (lifecycle, separation of duties, immutability, trigger
tests).

**Acceptance Scenarios**:

1. **Given** a submitted grant, **When** its creator, an editor, the submitter or the grantee (by subject,
   with case, width and invisible-character variants, or by person id) tries to approve, **Then** the
   answer is 403 `separation_of_duties`; **When** a different board member approves, **Then** the grant is
   approved and `equity.grant.approved.v1` is queued.
2. **Given** an `admin`, `hr` or `finance` caller, **When** they try to approve, **Then** 403: only the
   `board` role takes the board step.
3. **Given** a submitted or approved grant, **When** anyone updates its terms, **Then** the service answers
   409 and the database trigger also refuses a direct UPDATE.
4. **Given** two pending grants that each fit the pool but together do not, **When** both are approved,
   **Then** the second approval fails: the pool is re-checked in the approval transaction.
5. **Given** an approved grant, **When** HR amends it, **Then** a new draft version is created; the old
   grant is cancelled only when the new one is approved.

### User Story 2 - Employees see their own equity, managers never see their reports' (Priority: P1)

An employee reads their own approved grants, vesting schedule, exercisable amount and exercise window.
Managers get nothing about their reports.

**Acceptance Scenarios**:

1. **Given** an employee with a linked person id, **When** they list their grants, **Then** they get their own
   approved or later grants only (never drafts or pending grants), without internal notes.
2. **Given** the employee's manager (employee plus manager role), **When** they request the report's grant,
   vesting, stakeholder record or identity, **Then** the answer is 404, and the attempt is audited.
3. **Given** a caller with the manager role only, **Then** self-service is unavailable.
4. **Given** the audit hook is failing, **When** anyone reads a grant, the cap table, the ledger, a valuation or
   an export, **Then** nothing is returned or written (fail closed).

### User Story 3 - Vesting is computed deterministically (Priority: P1)

A pure calculator gives the vested quantity as of any date for cliff plus monthly or quarterly schedules,
dated or milestone tranches and single- or double-trigger acceleration held as data.

**Acceptance Scenarios**:

1. **Given** a 48-month schedule with a 12-month cliff starting 15 Jan 2024, **Then** nothing vests on
   14 Jan 2025, a quarter vests on 15 Jan 2025 and the total vests on 15 Jan 2028.
2. **Given** a start on the 31st or on 29 February, **Then** shorter months vest on their last day.
3. **Given** a termination date, **Then** vesting stops on it (inclusive); acceleration dated after it
   applies only if the clause says so.
4. **Given** any schedule, **Then** the result is never negative, never above the total and never decreases
   (property and fuzz tests); rounding policy floor, nearest or fractional is applied to the cumulative
   amount so the total is exact.

### User Story 4 - Exercises go through approval, payment and issuance (Priority: P2)

The holder (or HR or finance on their behalf) requests an exercise of vested options; finance approves,
records the exact payment, a third person issues the shares into the ledger.

**Acceptance Scenarios**:

1. **Given** vested options, **Then** a request above the exercisable amount (vested minus exercised minus
   reserved, inside the window) is refused; a second request cannot reuse reserved options.
2. **Given** the chain requested, approved, payment recorded, issued, **Then** the requester cannot approve,
   pay or issue, and the payer cannot issue (also enforced by triggers).
3. **Given** issuance, **Then** a ledger row is appended, the grant's exercised count rises, the pool
   shows the shares as exercised and `equity.exercise.recorded.v1` is queued, all in one transaction.

### User Story 5 - Finance reads the cap table as of any date and models a round (Priority: P2)

Finance, the board and admin read the basic and fully diluted cap table (holders, classes, options, pool,
ownership in parts per million) as of any date, export it as CSV, record valuations and convertibles and
model a round.

**Acceptance Scenarios**:

1. **Given** ledger entries on several dates, **Then** the table as of each date reflects only entries on or
   before it; ownership parts always add up to exactly 1,000,000.
2. **Given** a ledger, **Then** it cannot be edited or deleted, balances never go negative on any date, a class
   never exceeds its authorised shares.
3. **Given** a holder named `=HYPERLINK(...)`, **Then** exports prefix the cell so a spreadsheet does not run it;
   every export is audited per record before a byte is written.
4. **Given** the round model, **Then** the result is labelled illustrative and nothing is stored.

### User Story 6 - Retention, legal hold and erasure respect legal records (Priority: P1)

An erasure request pseudonymises a stakeholder's identity and clears free text, while issued share
records stay (legal retention). A legal hold blocks erasure. Keys can be rotated safely.

**Acceptance Scenarios**:

1. **Given** a former holder with issued shares, **When** admin erases them, **Then** the identity reads
   `[erased]`, the ledger and exercise records stay, tombstones are written, the database file and WAL no
   longer contain the old ciphertext, and the response says what was retained and why.
2. **Given** a legal hold, or live grants, **Then** erasure is refused.
3. **Given** a rotation while a value is concurrently updated, **Then** the update is never overwritten.

## Requirements *(mandatory)*

- **FR-001** In-house only; deny by default; one policy layer (`Decide`, `FieldAccess`); per-field access.
- **FR-002** Sensitive reads audited BEFORE data is returned, per record, failing closed; audit log append-only
  and hash-chained; denials audited and throttled.
- **FR-003** Sensitive columns sealed (XChaCha20-Poly1305, AAD binds row and field); key rotation with
  compare-and-swap.
- **FR-004** Quantities are exact decimals (1/10000 share), prices integer minor units, no floats.
- **FR-005** Grant terms immutable after submission; legal status transitions only; ledger, milestones,
  acceleration events, valuations append-only; enforced by triggers.
- **FR-006** Separation of duties through one function; subjects compared NFKC, invisible characters removed,
  case-folded; repeated in triggers.
- **FR-007** Pool and authorised-share limits are checked in the transaction that consumes them.
- **FR-008** Events carry ids and dates only; schemas have fixtures; names are in the catalogue.
- **FR-009** Erasure pseudonymises, keeps legally retained records, writes tombstones compatible with
  `core/backup` ledger, checkpoints the WAL and vacuums.
- **FR-010** Valid credentials are never throttled; failed authentications are; bodies are bounded and strict;
  database work has deadlines; lists are capped.
- **FR-011** Exports escape formula injection and are audited before output.
- **FR-012** Modelling is labelled illustrative, uses a documented formula and stores nothing.
- **FR-013** Jurisdiction-specific rules (409A, ISO/NSO limits, tax, 83(b), ESOP) are NOT implemented; they exist
  as data fields and documented limits only.

## Success Criteria

- **SC-001** `go test -race ./core/equity/... ./test/contract/...` passes offline.
- **SC-002** Idle RAM within the declared 16 MB (limit 64 MB), measured, not asserted.
- **SC-003** Every route is in the OpenAPI contract and every emitted event validates against its schema.
