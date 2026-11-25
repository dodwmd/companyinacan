# Feature Specification: Performance reviews and time off (Epic E14b)

**Feature Branch**: `epic/E14b-reviews-timeoff` | **Created**: 2026-11-25 | **Status**: Implemented offline (no cluster)

**Source**: SPEC.md (people and HR: reviews, 360 feedback, time off, leave; sensitive classes; approvals;
timesheets), `docs/epics/README.md` (E13, E14), `specs/012-people-directory` (the policy layer, sealed
fields, audited reads, status masking, erasure, retention), `specs/015-approvals-notifications` (approval
types declared in `module.yaml`, the reporting-line resolver, separation of duties), constitution
Principle V (secure and private by default).

> **This is a tool, not legal, HR or payroll advice.** Leave entitlements, accrual rules, notice
> periods, holiday calendars and the rules for performance data differ by jurisdiction and by
> contract. The module models what the operator configures as data and runs **no legal rule engine**.
> It never computes pay.

## Scope of this slice

Extends the in-house `people` module (no new service, no new always-on component) with:

1. **Reviews**: cycles (draft, open, calibration, closed), participants, templates (rating and text
   questions), self-assessment, manager review, peer and upward feedback with nominations approved by the
   manager, per-cycle anonymity, aggregation thresholds, calibration (lite), release with a separate
   approver, acknowledgement by the employee, history, read-only links to the subject's goals.
2. **Time off**: leave types (vacation, sick, parental, unpaid, custom) with accrual policy as data,
   an append-only balances ledger with as-of dates, requests (draft, pending, approved, rejected,
   cancelled) approved by the manager through the approvals engine interface, half days, public holiday
   calendars per location, a team calendar that only ever says "out", blackout periods, balance checks
   under concurrency, cancellation with reversal entries, and the interaction with the `leave`
   employment status.
3. Retention, legal hold and erasure for the new tables; new tombstone classes.
4. Manifest (`people.reviews.*` and `people.timeoff.*` permissions, approvals, data classes, events),
   settings schema, OpenAPI, event schemas with fixtures, documentation, RAM re-measured.

Not in this slice: a UI, notifications (the E11a engine can notify on the declared approval types and the
events), compensation or promotion decisions, goal editing from a review, pay or payroll, statutory leave
rules, working-time law, public holiday data sets, the wiring of can-core's approval client into
`cmd/can-people` (the seam and a fake exist, as for equity), and timesheet consumption of the events (the
finance module's side).

## User scenarios

### US1: A review cycle runs from open to release (P1)

HR creates a template and a cycle, adds participants and opens it (event `review.cycle.opened.v1`). The
employee writes and submits a self-assessment. The manager, once it is submitted, reads it and writes and
submits their review with an overall rating. HR moves the cycle to calibration, sets a calibrated rating and
notes, and asks for release. A different HR person approves; the review is released
(`review.released.v1`); the employee sees the outcome, reads the manager's comments, and acknowledges.

1. Before release the employee sees only their own self-assessment: not the manager review, rating,
   calibration or feedback. Calibration notes are never visible to the employee, in any state.
2. The release approver is none of: the subject, the manager who wrote the review, whoever calibrated it,
   or anyone who ever requested its release. Rejecting and re-requesting does not clear that history
   (no laundering). Compared canonically (NFKC, case-folded, invisible characters removed) on SSO subject
   and person id alike, one function (`sodOK`).
3. A manager sees their direct reports' reviews (live reporting line; a manager who no longer manages the
   person, or who is on leave or terminated, sees nothing). A skip-level manager sees only the released
   summary (rating and summary text). HR sees everything except their own review, and every read is audited.
   Colleagues, the `hiring` role and every other role get 404.
4. Ratings, comments, calibration notes and feedback are sealed at rest with an AAD that binds each value
   to its row and part; a value copied to another row does not open.

### US2: Peer and upward feedback keeps its authors safe (P1)

The employee proposes peers; the manager approves or declines; the manager or HR can add reviewers
directly. Upward feedback comes only from the subject's direct reports. A reviewer sees the question list
and the person to review, not who nominated them.

1. In an anonymous cycle, an answer carries no reviewer and no timestamp, its id is random, and comments
   are returned in an order derived from a hash of response and question (different per question), never
   submission order. No response id, timestamp or ETag is exposed; nothing says who has not answered.
2. Feedback (aggregates and comments) is shown only once the cycle has left `open` and the cycle's
   `min_responses` answers exist (at least 3, never configurable below that; upward feedback at least 5), to
   everyone including HR, and never incrementally: while the cycle is open the view does not change when
   someone answers, and nobody sees who has answered. After release the employee sees aggregates, never comments, and never who was asked.
3. The employee sees only the nominations they proposed themselves.
4. In an attributed cycle the manager and HR see who wrote each comment; the employee never does.

### US3: A person asks for time off and the manager decides (P1)

The employee drafts a request (type, dates, optional half days, a private note), submits it, and the
manager approves or rejects. Working days are computed from the person's location calendar. The balance is
checked on submit (with pending requests reserved) and again on approval, in one transaction.

1. A request that overlaps another pending or approved one, falls in a blackout period (except types that
   ignore them, such as sick leave), has no working days, or exceeds the balance (beyond the type's
   negative-balance allowance) is refused with 409.
2. The decider is the live direct manager (HR when there is none), never the requester. With an approval
   engine configured the engine must have approved and its decider is held to the same rule. A rejected
   request is final: the person files a new one.
3. Approval appends a `use` entry to the append-only ledger and publishes `timeoff.approved.v1` (ids,
   dates and a count of half days only: never the type or the reason).
4. Cancellation: the person until the leave starts, HR afterwards. An approved cancellation appends a
   `reversal` for the days not yet taken and publishes `timeoff.cancelled.v1`.
5. Concurrent submissions cannot exceed a balance: the service holds one connection, so balance
   checks and writes serialise (tested with goroutines).

### US4: Colleagues see "out", never why (P1)

The team calendar shows who is out between two dates. Colleagues get `out` only: no type, no reason, no
request id, no status, and pending requests are invisible to them. The person, their direct manager and HR
also see the type and status. A long leave that moves the person to the `leave` employment status shows to
colleagues as active (the directory masks it) and as "out" on the calendar exactly like a holiday.

### US5: Accruals and carry-over are data (P2)

Leave types carry an accrual mode (none, annual, monthly), a grant in half days, a carry-over cap, and a
negative-balance allowance with a floor. A job (hourly, or on demand by HR) grants and accrues for the
current year and expires the balance above the cap at the turn of the year. Every entry is keyed by person,
type, kind and period, so reruns write nothing new. HR corrects with adjustment entries (a reason is sealed).

### US6: Retention, legal hold, erasure (P1)

Two new data classes (`performance-reviews`, `people-timeoff`, both sensitive) join the existing purge
order. Erasing a person removes their reviews, what they wrote as an attributed reviewer, their leave
requests and their ledger, writes tombstones for both classes, and checkpoints and vacuums the database so
nothing survives in free pages or the WAL. A legal hold blocks erasure and age-based purges. Closed review
cycles expire by age.

## Requirements

- **FR-001** One policy layer (`Decide`) decides every review and leave action; tests enumerate role (all 16
  subsets) x person status x relationship x cycle and review state against an independent oracle.
- **FR-002** Invisible means 404, visible but not allowed means 403 (409 for a state conflict).
- **FR-003** Sealed: self, manager, calibration, final outcome, acknowledgement and release notes, feedback
  answers, leave notes and adjustment reasons. Key rotation covers every sealed column.
- **FR-004** Every read of review content, leave requests, balances and ledgers is audited before the data
  is returned; if the audit cannot be written, the read is refused. Writes are audited in the same
  transaction as the change; refusals are audited after it.
- **FR-005** Events carry ids, dates and counts only. No names, ratings, summaries, feedback, leave types or
  reasons. Leave status changes publish nothing for active <-> leave.
- **FR-006** Separation of duties is one function (`sodOK`) applied to the actor and to an engine decider.
- **FR-007** Valid credentials are never throttled; authentication runs first (existing behaviour, tested on
  the new routes).
- **FR-008** Every database read and write runs under a deadline (10 s and 15 s).
- **FR-009** Migration `004` applies on an existing database and is idempotent (tested).
- **FR-010** The ledger is append-only at the database level; the only delete path is an erasure or
  retention purge in the same transaction as the purge.

## Success criteria

- `make test` and `go test -race ./core/people/... ./test/...` pass; the OpenAPI contract test covers every
  new route; event schemas have fixtures and the events the service really emits validate against them.
- Idle RAM stays well inside the declared 30 MB (measured, `measurements.md`).

## Known gaps

See `docs/people.md` (Gaps): no UI; the can-core approval client is not wired into `cmd/can-people`; leave
accruals do not prorate; `use` entries are dated at the start date of a request; the leave status change
cannot coexist cleanly with an HR-scheduled status change on the same person; anonymity holds against API
users and not against someone who can read the database file and the audit table; the audit record that an
answer was submitted names the reviewer and the request.
