# Spec 065: projects in the work web UI, and the manager views (epic E14d)

Status: built. Unit and contract tests; the browser suite (`test/browser`) covers the pages against the
compose stack. Not verified on a cluster, with a screen reader or on a real phone.

## Problem

The work web UI could not create or manage a project, so a new company needed an API call with a token
(found by the acceptance journey, `specs/063-acceptance-journey`). Of what SPEC.md section 5 says a manager
needs, there was no "who is on what" view, no ageing of P0 and P1, no saved filters and no
`priority_changed` event.

## Requirements

1. Create, edit, archive and restore a project in the UI, and manage its members, through the same `Store`
   calls as the REST API, with the same permissions (`work.admin`; creating also needs a token that is not
   limited to projects) and CSRF protection.
2. A first sign-in with no project leads to creating one.
3. Views: workload by person, ageing of unresolved P0 and P1, stale work in progress.
4. Saved filters.
5. Publish `work.issue.priority_changed.v1`.
6. Everything above has a REST operation (SPEC.md: the API covers what the UI does), works without
   JavaScript under the existing CSP, on a 360 px phone, and passes the static accessibility checks.

## Decisions

- **Membership is a roster, not an access list.** Access stays what permissions and token scope say. Members
  are suggested as assignees and appear in the workload even with nothing to do. Turning it into access
  control would silently take projects away from people who can see them today; if wanted, it needs its own
  decision and an upgrade path. The settings page says so.
- **Workload** counts unresolved issues per assignee (P0 to P3, in progress, blocked, overdue) and names up
  to five in progress. Epics are containers and are left out; so are archived projects. A blocker in a
  project the caller cannot see is not counted.
- **Ageing** measures how long an issue has had its current priority, not how old it is: a year-old issue
  raised to P0 today is a new emergency. Both are shown. "Past target" compares with the project's SLA
  resolution hint. Migration 005 fills the date for existing issues from the activity history.
- **Stale** means in a status of category `active` with no activity for N days (default 7). Comments,
  edits and moves are activity; reordering is not. Epics are left out.
- **Saved filters** store the search language (`core/work/query.go`) with a project (or none) and a sort. A
  filter is private or shared; running it is an ordinary search by the caller, so it never widens access.
  A token limited to projects does not see other people's shared filters over every project. At most 50 per
  person. Delete and save again to change one.
- **`work.issue.priority_changed.v1`** is emitted for a change, not at creation, alongside
  `work.issue.updated.v1`. No notification rule is declared here: telling the manager about a P0 is
  `specs/060-reminders` (`work.issue.p0.v1`).
- **Version**: module and image 0.3.0 (0.2.0 is `specs/060-reminders`). The new event widens what the module
  may do, so an installed module waits for approval (`docs/releasing.md`).
- **Erasure** (`specs/056-erasure-finance-work`): membership of the erased person is removed, as are their
  private filters and any filter that searches for them by name; their shared filters stay under the
  pseudonym.

## Routes

UI: `specs/023-work-ui/contracts/ui-routes.md`. REST: `specs/011-work-tracker/contracts/openapi.yaml`.
Event: `specs/011-work-tracker/contracts/events.md`.

## Limits

- No components (SPEC.md section 5 lists them; labels are the substitute for now).
- No "what is everyone on" by team: that needs reporting lines from `people`.
- Identifiers are shown as they are (`user:ann`); there is no people picker.
- Reports read every unresolved issue on each view. Lists stop at 200 issues and 500 people.
- Workflows and SLA hints are still edited through the API.
