# Reminders reach people (E11f)

SPEC.md section 3.7: "Modules publish reminders with a due date; can-core schedules and de-duplicates
them." The notification engine (specs/015, specs/025, specs/039) existed; only forms declared a rule, so
reminders reached nobody.

## Scope

1. Notification rules in the finance, jobs, work, people and entity manifests; the table of events and
   recipients is in `docs/notifications.md` ("Reminders") and fixed by `test/contract/reminders_test.go`.
2. New events: `invoice.overdue.v1`, `contract.declined.v1`, `expense.submitted.v1`,
   `expense.decided.v1`, `timesheet.submitted.v1`, `timesheet.decided.v1`, `work.issue.p0.v1`,
   `timeoff.requested.v1`, `timeoff.decided.v1`. `application.received.v1` gains an optional
   `hiring_manager` (a staff account); `entity.filing.due.v1` gains a summary form.
3. can-core: event data copied into a notification is `id`, `*_id` (text or whole number) and a numeric
   `count`; a rule that names a role and reaches no person falls back to the owners; project wording
   and an admin UI preference per type. Delivery (inbox, email, push, quiet hours, digests, limits,
   mutes) is unchanged.
4. entity (can-board) publishes through can-core; before this its events never left its outbox.

## Rules

- **No PII or free text.** Events carry ids, dates, counts and account subjects. Notification text is
  the project's, with ids and counts only. Decline reasons, rejection reasons, titles, names and amounts
  stay in the module.
- **Idempotent.** finance: `overdue_notices` (invoice, due date), marked in the transaction that queues
  the event. Contracts: the existing `reminded_for`. entity: the existing `reminders_sent`. can-core:
  one notification per event id per recipient (existing).
- **Backlog.** A sweep with more than five new items queues one event with `count`. The can-board
  migration `005_reminders.sql` retires events that were never attempted and re-arms pending filings
  that nobody heard of, so the first sweep announces them (summarised) once.
- **Isolation.** In-app, e-mail and push limits are per recipient and origin; approval notices have
  their own allowance and are not counted against a module's e-mail quota (its switch still applies).
  `application.received` and `lead.created` are digested beyond five an hour. A rule on an event its
  module publishes ignores other publishers. A P0 is repeated to a person at most hourly.
- **Standalone entity.** Migration 005 also runs there: see docs/entity.md.
- **Manifests.** Module versions and images are bumped (`core/modimages/images.txt`): finance, jobs,
  work 0.2.0; entity and board 0.2.0; people module 0.4.0 (image 0.2.0). New `publishes` entries and
  rules widen the manifests: where such upgrades are held for approval (PR #77), they need it.

## Not in scope

Purchase order drafts, review releases, equity and board approvals; re-announcing contract reminders
raised before the upgrade; digests for types other than the publicly triggered ones. There are no
security notices in the engine today; one added later needs its prefix in `IsPlatformType`.

## Verification

Unit tests in `core/finance`, `core/board`, `core/work`, `core/people`, `core/jobs`,
`core/server/notify`; schema fixtures and the manifest test in `test/contract`; the compose end-to-end
test `test/e2e/reminders_test.go` (an overdue invoice and a new application appear in the admin inbox).
Not verified on a cluster.
