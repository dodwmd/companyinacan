# Tasks: reviews and time off (E14b)

Order is the order of work. `[x]` done and covered by a test unless stated. Red means the test was written
first and failed (compile failure on missing symbols counts) before the implementation.

## Foundation
- [x] T001 Migration `004_reviews_timeoff.sql`; migration on an existing database (`TestMigrationOnExistingDatabase`).
- [x] T002 Policy: actions, Fact fields, `decideE14b`. Red then green: `TestE14bPolicyAgainstOracle` (all 16 role subsets x every status x
      relationship x cycle and review state, against an independent oracle), invariants and scenarios.
- [x] T003 `sodOK` with NFKC/casefold; `wtx`/`txAudit` with deadlines; `SQLAuditor.RecordTx`.

## Reviews
- [x] T010 Templates, cycles, participants, state machine, `review.cycle.opened.v1` (`TestCycleLifecycleAndEvents`).
- [x] T011 Self, manager, calibration, release request and decision, acknowledgement; sealing and AAD binding; 404-not-403
      (`TestReviewFlowVisibilityAndRelease`).
- [x] T012 Separation of duties and no laundering; engine decider (`TestReleaseSeparationOfDutiesAndNoLaundering`,
      `TestApprovalEngineDecidesRelease`). Mutation check: disabling the history check fails these.
- [x] T013 Nominations, anonymity, threshold, ordering, no attribution (`TestPeerFeedbackAnonymityThresholdAndNominations`,
      `TestAttributedFeedbackIsManagerAndHrOnly`, `TestUpwardFeedbackNeedsDirectReportsAndAudit`).
- [x] T014 Audit before returning, fail closed; goals linked read-only (`TestReviewGoalsLinkedReadOnly`).

## Time off
- [x] T020 Leave types, calendars, blackouts, working-day computation, half days (`TestLeaveTypesAreHrDataAndValidated`,
      `TestWorkingDaysHolidaysAndHalfDays`).
- [x] T021 Accruals, carry-over, idempotence (`TestAccrualsCarryOverAndIdempotence`).
- [x] T022 Request lifecycle, ledger append-only, separation, audit, events (`TestRequestLifecycleLedgerAndSeparation`,
      `TestApprovalEngineAndLeaveSeparation`).
- [x] T023 Concurrency: balance checks (`TestBalanceChecksUnderConcurrency`, also under `-race`). Mutation check: removing the
      reservation fails it.
- [x] T024 Overlap, half-day overlap, blackouts (`TestOverlapBlackoutsAndHalfDayOverlap`).
- [x] T025 Cancellation and reversal, partial reversal (`TestCancellationReversesTheLedger`).
- [x] T026 Calendar privacy (`TestCalendarShowsOnlyOutToColleagues`).
- [x] T027 Leave status without leaking the reason (`TestLongLeaveMovesStatusWithoutLeakingTheReason`).

## Retention, erasure, rotation
- [x] T030 Classes, purge, tombstones, WAL scrub, ledger guard (`TestReviewErasureRetentionAndLegalHold`,
      `TestTimeOffErasureAndLedgerGuard`, `TestRetentionPurgesClosedReviewsAndSkipsLegalHold`).
- [x] T031 Key rotation for every new sealed column (`TestRotateKeysReSealsReviewsAndLeave`).

## API, contracts, docs
- [x] T040 Routes (`api_e14b.go`), API tests (`TestAPIReviewsAndLeaveEndToEnd`: 401, 403, 404, validators, throttling).
- [x] T041 OpenAPI extended; route contract test green; event schemas, fixtures, embed, manifest, settings schema; the
      events-match-schemas test drives the new flows.
- [x] T042 `docs/people.md`, `modules/people/README.md`, this directory; RAM re-measured (`measurements.md`).
- [x] T043 Hourly accrual tick and per-minute leave-status reconcile in `cmd.go`; `reviews.min_responses` in the config.

## Not done (see spec, Known gaps)
- [ ] T050 can-core approval client wired into `cmd/can-people` (the seam and a fake exist).
- [ ] T051 UI.
- [ ] T052 Proration of accruals; per-day ledger entries; working-time rules.
- [ ] T053 Cluster verification.
