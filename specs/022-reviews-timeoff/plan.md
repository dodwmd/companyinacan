# Implementation Plan: Reviews and time off (E14b)

**Branch**: `epic/E14b-reviews-timeoff` | **Date**: 2026-11-25 | **Spec**: [spec.md](spec.md)

## Summary

Extend `core/people` (library) and the unchanged `cmd/can-people` binary. No new service, dependency or
always-on component. New files sit beside the existing ones and reuse their seams: the pure policy layer, the
keyring, the audit hook, the outbox, the rate limiter and the migration runner.

## Technical Context

**Language/Version**: Go 1.27. **Dependencies**: stdlib, `modernc.org/sqlite`, `golang.org/x/crypto`,
`golang.org/x/text` (already in `go.mod`; NFKC and case folding for separation of duties, as in equity and
finance). **Storage**: the existing SQLite database; migration `004_reviews_timeoff.sql`. **Testing**: `go
test -race`; contract tests in `test/contract`. **Constraints**: idle RAM 30 MB, limit 64 MB; fail closed.

## Constitution Check (before research)

| Principle | Assessment |
|---|---|
| I Spec-first | Refines SPEC.md (people/HR, approvals, timesheets) and E14. No contradiction; the new events are additions, listed in the manifest. |
| II Footprint | No new process. New tables are small; measured RSS recorded in `measurements.md`; manifest keeps 30/64 MB. |
| III Test-first | Policy, review and leave tests were written before the implementation of each part and run red (compile failure on the missing symbols) before green; mutation spot checks confirm the separation-of-duties and balance tests fail when the rule is removed. Honest order in `tasks.md`. |
| IV Stable contracts | `people-api.yaml` extended (route contract test); four event schemas with fixtures; the manifest declares the approval types and permissions; no access to other modules' internals; timesheets are linked by event only. |
| V Secure and private | Sensitive classes stay in the in-house module. Per-part sealing with AAD; audited reads (fail closed); 404 for the invisible; anonymity enforced in storage and in the API; threshold of 3; separation of duties in one function; erasure with WAL checkpoint and VACUUM; legal hold; retention; ids-only events; leave never leaks through events, the directory, the calendar or the status history. Disclaimer present. |
| VI Open-source hygiene | Apache-2.0; no new dependency; fake data only. |
| VII Simplicity and measured claims | Calibration is "lite" (a rating and notes). Leave rules are data, not an engine. RAM measured, not asserted. |

## Design

- **Policy** (`policy.go`): new actions and Fact fields (`CycleState`, `SelfDone`, `MgrDone`, `Released`,
  `ReleaseRequested`). HR/admin act "on someone else" (`hrOther`): no review or leave power over their own
  record, whatever roles they hold.
- **Reviews** (`reviews.go`, `reviews_feedback.go`): the review is one row per person per cycle
  (`review_participants`); each part is a sealed blob. A read builds the view from the policy decisions
  part by part, so absence is structural. The feedback view applies the threshold and the ordering rules.
- **Release**: `RequestRelease` freezes the outcome (calibrated rating, else the manager's) into a sealed
  blob; `DecideRelease` checks `sodChainOK`: the actor and (with an engine) the engine's decider against
  the stored history (SHA-256 of canonical identities of everyone who wrote, calibrated or requested, plus
  the subject and the author), so the history holds no names and never shrinks.
- **Time off** (`timeoff.go`): halves as the unit; slots (`day*2 + am/pm`) make overlap tests integers; the
  ledger is the single source of truth for balances; the single connection serialises balance checks.
- **Leave status**: `ReconcileLeaveStatus` (called from `ApplyDue`, hourly and each minute) applies
  `active -> leave` and back through the existing `applyStatus`, only for changes it made, with no note.
- **Audit**: `SQLAuditor.RecordTx` lets mutations commit with their audit row; refusals are audited after the
  transaction (the single connection cannot serve the audit while a transaction holds it).
- **Approvals**: `ApprovalEngine` (`Request`, `Status`, `Cancel`) mirrors equity's seam; types `timeoff` and
  `review-release` are declared in `module.yaml`; the service decides itself when no engine is configured.

## Project structure

```text
core/people/
  policy.go             + review and leave actions, Fact fields, decideE14b
  sod.go                canonSubject, sodOK, wtx/txAudit with deadlines
  audit_tx.go           RecordTx, auditIn, auditReadMany
  reviews.go            templates, cycles, participants, review view, writes, calibration, release, acknowledge
  reviews_feedback.go   ApprovalEngine seam, nominations, feedback, the feedback view
  timeoff.go            leave types, calendars, blackouts, requests, ledger, accruals, calendar, leave status
  api_e14b.go           the new routes
  retention.go          + classes, purge, age-based review sweep, key rotation for new columns
  migrations/004_reviews_timeoff.sql
modules/people/         module.yaml, settings.schema.json, events/ copies, README
schemas/events/         review.cycle.opened, review.released, timeoff.approved, timeoff.cancelled (+ testdata/events)
specs/012-people-directory/contracts/people-api.yaml   extended
specs/022-reviews-timeoff/                             this directory
docs/people.md
```

## Constitution Check (after design)

No change. One tension recorded: anonymity is a property of the API and the stored rows, not a defence
against a database administrator (see research.md, decision 3); the documentation says so.

## Complexity tracking

| Item | Why |
|---|---|
| Two stores of identity for separation of duties (hashes in `touched_by`) | A history of names would be personal data in a table that outlives erasure of the people in it; hashes compare equal without storing names. |
| `ledger_guard` | Erasure must delete ledger rows, the ledger must otherwise be append-only; a guard row opened inside the purge transaction keeps both true at the database level. |
