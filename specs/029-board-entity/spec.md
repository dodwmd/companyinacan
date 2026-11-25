# Feature Specification: Board and entity modules (Epic E17b)

**Feature Branch**: `epic/E17b-board-entity` (spec dir `029-board-entity`)

**Created**: 2026-11-25

**Status**: Draft (implemented offline; cluster verification pending)

**Input**: Epic E17 (`equity`, `board`, `entity`) in `docs/epics/README.md`; SPEC.md sections 3.7 (RBAC, retention
and legal hold, audit, erasure semantics, append-only stores), 4.3 (capabilities, event catalogue: `board
resolution.passed.v1`), 5 (sensitive data) and the *Board & governance* and *Entity & legal calendar* rows of the
module table; the constitution (Principle V: board materials are confidential); the equity slice
(`specs/018-equity`: policy layer, separation of duties, sealed fields, audit-before-return, the approval
engine seam, legal retention versus erasure, and the QA findings: grantee and director self-approval,
`terminal_reason` leaks, the separation-of-duties chain including creators and attesters); the module wiring
slice (`specs/016-module-wiring`). Second slice of E17: the in-house `board` and `entity` modules, one binary.
The investor portal, can-core wiring and an admin UI are later slices.

> **This is a tool, not legal advice.** It records governance facts and computes quorum and tallies from data the
> operator enters. **Not implemented:** notice, quorum and voting law of any jurisdiction, proxies and casting
> votes, who may sign a written resolution beyond unanimity, which statutory registers a company must keep and
> what they must show, filing lists and deadlines, director duties, minute formalities. These are modelled only as
> data fields (fractions, days, thresholds, obligation records) and documented limits. Retention defaults are
> starting points. The operator is responsible for compliance and should have counsel review their constitution
> and the way it is entered here.

Everything is built and verified offline: no cluster, no container runtime. What that leaves unverified is listed
in [research.md](research.md) and `tasks.md`.

## Decision: one binary, two modules

`board` and `entity` have separate manifests (roles, permissions, data classes, capabilities, events) and are
served by one process, `can-board` (`core/board`), with one SQLite database, one keyring, one audit chain and one
listener (`:8082`). Justification in [research.md](research.md) (R1): they share ids and the audit chain (a
resolution authorises register entries and a filing), one process is ~14 MiB where two would be ~29, and an operator
can enable either. The cost is that can-core cannot yet deploy two manifests as one release (a documented gap).

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A resolution is voted by roll call with quorum and recusal (Priority: P1)

The secretary schedules a meeting (with the body's notice), records who is present, drafts a resolution and
proposes it. The chair opens the vote: the people who may vote are fixed (members in term, minus the resolution's
subjects and anyone who declared a recusal) and quorum is checked in exact integers. Members vote once; the chair
closes; the tally is recorded and can never change.

**Why this priority**: it is the core record of a decision and the thing equity's board approval needs.

**Independent Test**: `go test ./core/board/` (`TestQuorum...`, `TestRollCall...`, `TestRecusal...`,
`TestThresholdsAreDataAndExact`, tally property and fuzz tests).

**Acceptance Scenarios**:

1. **Given** five members and a 1/2 quorum, **When** two are present, **Then** opening the vote is refused (409,
   "no quorum: 2 present, 3 required"); with three present it opens.
2. **Given** a resolution naming a director as its subject, **When** voting opens, **Then** that director is not on
   the voter list, does not count towards quorum, and a vote from them is 403.
3. **Given** a closed resolution, **When** anyone tries to update the tally, add a vote, or delete anything, **Then**
   the service refuses and so does the database (triggers).
4. **Given** the proposer is the only vote "for", **When** the vote closes, **Then** it fails with reason
   `proposer_sole_approver`.

### User Story 2 - Written consent and secret ballot (Priority: P1)

A resolution can be passed by circulation: every eligible member signs (with a signature record reference) before
a deadline; one decline fails it; it passes by itself when the last signature arrives. A resolution can be a
secret ballot: choices are sealed and, once voting closes, cannot be tied to a person by the API, the audit trail
or a copy of the database (ballots are staged, then written in a shuffled order); nobody, the secretary included,
can read who voted how. Not protected: live file access during an open vote (docs/board.md).

**Independent Test**: `TestWrittenConsent`, `TestSecretBallotCannotBeLinkedToAPerson`.

**Acceptance Scenarios**:

1. **Given** a consent with three eligible members, **When** all sign, **Then** it passes (`all_signed`); **When** one
   declines, **Then** it fails (`declined`); **When** the deadline passes first, **Then** the sweep fails it
   (`deadline`).
2. **Given** a secret ballot, **When** votes are cast, **Then** `votes` stays empty, the ballots are only staged and
   no per-vote audit row is written, a second vote from the same person is refused; **When** voting closes,
   **Then** the ballots and receipts are written in a shuffled order, the staging is deleted and vacuumed, the audit
   trail gets one `voted` marker per voter sorted by person id, and no role can list who voted how.

### User Story 3 - Minutes are statutory records (Priority: P1)

The secretary drafts minutes; a director who attended, and who did not draft or edit them, approves. An approved
version cannot change; a correction is a new version that is approved again and supersedes the old one, which
stays. Attendance freezes once minutes are approved.

**Independent Test**: `TestMinutesLifecycleAndSeparationOfDuties`.

**Acceptance Scenarios**:

1. **Given** a draft, **When** its drafter (by subject spelling or person id) tries to approve, **Then** 403
   `separation_of_duties`; an observer or an absent member gets 404 for the draft.
2. **Given** approved minutes, **When** anyone edits them, **Then** 409, and a direct UPDATE of the content hash,
   the status back to draft, or a DELETE is refused by the database.

### User Story 4 - Confidential by default, 404 for what you cannot see (Priority: P1)

A director sees the bodies they sit on and nothing else; an observer sees agendas and approved minutes only; an
admin runs the service and sees no board content; an employee sees only their own action items. A record the
caller may not see answers 404, byte-identical to a missing one.

**Independent Test**: `TestPolicyMatrix` (every role × every relationship × every action),
`TestInvisibleIs404ForEveryRecordType`, `TestAPIAuthHeadersAndNoOracle`.

### User Story 5 - The equity approval seam (Priority: P2)

can-core's approval adapter for equity can ask for a board resolution (`resolutions` capability), read its
outcome (`pending`, `approved`, `rejected`, `cancelled`) and correlate the `board.resolution.passed.v1` event by
the external reference. The requester cannot be the sole approver; a grantee who is a director is recused. Equity
is not modified in this slice.

**Independent Test**: `TestResolutionSeamForOtherModules`.

### User Story 6 - Company records and registers as of any date (Priority: P2)

The secretary keeps legal entities (sealed registration numbers), the registered agent and addresses with
history, and the directors, members and charges registers as append-only ledgers. A director or officer attests an
entry (never their own, never one they recorded). The registers can be viewed as of any date and as known on any
date; mistakes are corrected by later entries.

**Independent Test**: `TestDirectorsRegisterAsOfViewsAndHistory`, `TestMembersRegisterNeverGoesNegative`,
`TestAttestationSeparationOfDuties`, `TestContactsAreEffectiveDated`.

### User Story 7 - A filings calendar that reminds (Priority: P2)

Obligations are data (name, authority, first due date, recurrence, reminder points). The sweep announces a
reminder once per point as `entity.filing.due.v1` (ids and dates only); filing records the receipt and creates the
next due date.

**Independent Test**: `TestFilingsCalendarAndReminders`, `TestMonthEndClamp`.

### User Story 8 - Statutory records survive erasure; the rest does not (Priority: P2)

Erasure of a person removes declared-interest text, addresses and dates of birth, and keeps terms, votes,
attendance, minutes and register entries (including the name a register must show), saying what was retained and
why; a legal hold blocks it.

**Independent Test**: `TestErasureRetainsTheLegalRecordAndPseudonymisesTheRest`,
`TestRetentionSweepHonoursLegalHold`.

### Edge Cases

- A director token with no person id sees nothing (membership comes from the person link).
- A written consent cannot abstain or be a secret ballot; a secret ballot needs two "for" ballots to pass.
- Quorum is re-checked at every vote and at close; a vote after quorum is lost is refused.
- Members whose term ended lose access; their past votes stay.
- A backdated register entry is replayed against every later date and refused if any balance would go negative.
- A sweep that was down over a due date announces one reminder, not one per missed point.
- An expired context fails reads and writes instead of hanging.

## Requirements *(mandatory)*

- **FR-001** Bodies (board, committee) with parent, members and effective-dated terms (chair, member, secretary,
  observer), one open term per person per body, one chair at a time, history kept.
- **FR-002** Meetings with the body's notice days as data (a waiver reference when short), agenda items,
  invitees, attendance (frozen after approved minutes), quorum computed in exact integers from data on the body.
- **FR-003** Board packs: document references (opaque id, sealed title and note, size, sha256), issued once;
  later additions are marked supplementary.
- **FR-004** Conflict declarations (recuse, abstain, disclosed) with a sealed nature; resolution subjects are
  recused automatically; declared before voting opens.
- **FR-005** Resolutions: draft → proposed → voting → passed | failed | withdrawn (forward only); amendments create
  versions; roll call, written consent with signature references, secret ballot (sealed, staged and shuffled at close); tallies
  immutable once closed; proposer never the sole approver; pass threshold as a fraction (strict or inclusive).
- **FR-006** Minutes: draft, approved, superseded; versions; immutable after approval; drafter and editors cannot
  approve; integrity hash on every open.
- **FR-007** Action items sealed, assigned to a person, linked to `work` issues by opaque id.
- **FR-008** `resolutions` capability: request a resolution for another module's (type, ref), read its outcome;
  `board.resolution.passed.v1` carries the ids and the result.
- **FR-009** Legal entities with sealed identifiers, effective-dated agent and addresses, three statutory
  registers as append-only ledgers (replay-validated, corrections by later entries, as-of and known-at views,
  attestation with separation of duties), filings calendar with reminder events, document references, share
  class links by opaque id.
- **FR-010** Policy: one pure `Decide`/`FieldAccess` layer; a table test over every role (director, officer,
  secretary, admin, observer, audit-viewer, employee, integration) × relationship; one `sodOK` function for every
  separation-of-duties rule (NFKC, case folding, invisible characters, person id).
- **FR-011** 404 for what the caller cannot see; 403 only when visible but not permitted; no existence oracle.
- **FR-012** Every read audited before it is returned (fail closed), lists audited per record, every write audited
  in the same transaction; refusals audited with a rate cap; the audit table is append-only and hash-chained.
- **FR-013** Sealed columns (XChaCha20-Poly1305, AAD bound to row and column) with key rotation by
  compare-and-swap; the keyring file must be 0600/0400, regular, owned by the service user or root.
- **FR-014** Events carry opaque ids, dates and the outcome only (no text, names, counts per person or amounts).
- **FR-015** Retention per data class; legal retention for minutes, resolutions, registers, filings, audit;
  erasure by person with a statement of what was retained; legal hold; tombstones for the E08 ledger; replayable
  purge hook.
- **FR-016** CSV exports protect against formula injection and are audited before a byte is written.
- **FR-017** A valid credential is never throttled because of failed ones from the same address; request bodies
  64 KiB, strict JSON; deadlines on every database operation; `Idempotency-Key` on resolution creation.
- **FR-018** Manifests (`board`, `entity`): inhouse provider, sensitive data classes with retention, `experimental`
  status stated honestly, distinctive permission names, roles that are not reserved, an `approvals` entry
  (`resolution`, qualified `board.resolution`), chart values, Dockerfile fragment.
- **FR-019** RAM target 20 MB idle (measured locally: 14.4 MiB).

## Success Criteria

- `make test` and `go test -race` green; policy matrix, tally property and quorum fuzz tests pass; contract tests
  cover manifests, schemas with fixtures, events actually emitted, the OpenAPI document against the routes.
- Idle RSS ≤ 20 MiB (measured), request 20 MB, limit 64 MB.
- Nothing in an event, an audit row or a CSV header reveals content (tests).
