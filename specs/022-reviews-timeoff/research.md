# Research and decisions (E14b)

Each decision: the choice, the reason, what was rejected.

## 1. One review row per person, parts sealed separately

Choice: `review_participants` holds the self part, manager part, calibration, frozen outcome, acknowledgement
and release note as separate sealed columns, each with its own AAD (`review:<id>:<part>`).
Reason: visibility differs per part and per viewer, and a view built part by part makes "absent, not empty"
structural. A single blob would force a decrypt-then-filter pattern. Rejected: one table per part (more joins,
no benefit).

## 2. Visibility rules (who sees what)

| Part | Subject | Direct manager | Skip-level manager | HR/admin (not subject) | Others |
|---|---|---|---|---|---|
| That the review exists, status | yes | yes | only when released | yes | 404 |
| Self-assessment | yes | once submitted | no | yes | no |
| Manager review (answers, proposed rating) | after release (answers only, not the proposed rating) | yes | no | yes | no |
| Calibration notes and rating | never | no | no | yes | no |
| Final outcome (rating, summary) | after release | after release | after release | yes (once requested) | no |
| Peer/upward aggregate | after release | yes, past threshold | no | past threshold | no |
| Peer/upward comments | never | past threshold | no | past threshold | no |
| Goals (read-only links) | own | those the goal policy allows | no | those the goal policy allows | no |

HR gets no power over their own review: they are treated as the subject. Calibration is deliberately HR-only;
a manager seeing calibration notes would let them be quoted to the employee.

## 3. Anonymity

Rows: a response in an anonymous cycle has no reviewer and no timestamp, and a random id. The request row
only flips to `submitted` (no timestamp). Output: no response ids, no timestamps, no ETag; comments sorted by
`sha256(response id : question id)`, so the order differs per question and carries no time information.
Threshold: shown only from `min_responses` (>= 3) for everyone including HR; below it only a count.
Limits, stated plainly: someone who can read the database file can correlate by write order in the WAL or
by the audit table (which records that a reviewer answered a request, not what); the reviewer's free text
can identify them by style or content; a cycle where one person is asked by only three reviewers is weakly
anonymous. Rejected: per-cycle salting of ids (no gain against the same adversary); hiding the count
(managers need to know whether to wait).

## 4. Nominations

The subject proposes peers; the manager approves or declines; the manager and HR can add directly (approved
at once). Upward reviewers must be direct reports of the subject and are added by the manager or HR, never
by the subject. The subject sees only their own proposals; the reviewer sees the subject and the questions,
never who nominated them. Bounded at 12 reviewers per review.

## 5. Release workflow and separation of duties

HR requests, a different HR person approves. The approver must not be in `touched_by` (everyone who wrote
the manager review, calibrated, or requested a release, including earlier rejected requests) nor the subject
nor the author. One function (`sodOK`) compares subjects canonically (NFKC, invisible format characters
removed, case-folded) and a person across SSO subject and person id. The history stores SHA-256 hashes so it
holds no names (it must outlive the erasure of the people in it without becoming a PII table). With an
approval engine, the engine's decider is resolved to a person and held to the same rule. Rejected: letting
the employee's manager approve (they wrote the review); a configurable rule (FR-003 of spec 015: not
configurable).

## 6. Units, calendars and the ledger

Half days as integers: no floating point in balances. Overlap uses slots (`day*2 + 0|1`). Holidays and the
work week are data per location, matched to `people.location` (case-folded). No data set ships. Balance =
sum of the ledger; `use` is dated at the request start; accruals are keyed by period so reruns are no-ops;
carry-over expires the part of the prior balance above the cap, once per year, before the grant. No
proration (HR adjusts). Rejected: a rules engine (jurisdictional, out of scope by instruction); per-day
`use` entries (more rows, little gain).

## 7. Concurrency

The service opens SQLite with one connection, so transactions serialise; the balance check and the ledger
write share a transaction. Tested with goroutines (12 concurrent submissions against a balance for 5; five
concurrent approvals after a correction leaves room for 3). Rejected: `BEGIN IMMEDIATE` plumbing (not needed
with one connection; revisit if the pool ever grows).

## 8. Leave status and privacy

A leave type may set the leave status when a request of at least N half days runs. The status is applied by
a reconciler through `applyStatus`, with an empty note and `recorded_by = timeoff`; the directory, events and
history already mask or omit leave for people who may not see it. The reconciler only reverts changes it
made. Known limit: an HR-scheduled status change on the same person is dropped as stale if it no longer
matches (existing `ApplyDue` behaviour).

## 9. Events

`review.cycle.opened.v1`, `review.released.v1` (ids and dates), `timeoff.approved.v1`, `timeoff.cancelled.v1`
(ids, dates, half-day count; never the type). Dates reveal absence, so they are meant for modules that
subscribe deliberately (timesheets to mark non-working days); the manifest and schema say so. Cancellation
is published too because a consumer of the approval has no other way to undo it. Rejected: including the
leave type (reveals sick or parental leave); a release event with the rating.

## 10. Audit and transactions

Reads audit before returning and fail closed. Writes audit inside the mutation transaction when the auditor
can join it (`RecordTx`), so there is no mutation without a record. Refusals are audited best-effort after
the transaction ends: the single connection cannot serve the auditor while a transaction holds it.

## 11. Erasure

`performance-reviews`: the person's reviews (cascade to requests and responses), what they wrote as an
attributed reviewer, requests addressed to them. Anonymous feedback they gave is not linked to them and stays
(it names nobody). `people-timeoff`: requests and ledger (guard opened inside the purge transaction). The
generic basic purge replaces the person's id in `created_by`-style columns with `erased`. Scrub: checkpoint
and VACUUM, as for the other classes.
