# Feature specification: People self-service web UI (Epic E13c)

**Status**: built offline and tested; not exercised in a browser or on a cluster ([V] items in
`tasks.md`). **Depends on**: `specs/012-people-directory` (service, policy layer),
`specs/022-reviews-timeoff` (reviews and time off), `specs/016-module-wiring` (personal access
tokens), `specs/010-admin-ui` (conventions). **Principle**: constitution V (people data is the most
sensitive data class; one in-house module, one policy layer, audit before return).

## Why

SPEC.md names clients for people/HR: a web UI and an Android/PWA client. Until now the module was a
JSON API. Employees and managers need a phone-friendly page for their own profile, the org chart,
1:1s, goals, time off and reviews, without anyone building a second authorisation layer.

## Hard rules (the QA lessons of earlier epics, applied)

1. **One authorisation path.** The UI calls the same `Service` methods as the JSON API. It never
   reads the database, never calls `Decide` or `FieldAccess`, never inspects roles to allow
   something. A test (`TestUISourceHasNoSecondAuthorizationPath`) fails if `ui*.go` does. Where the
   UI must know whether to draw a form it asks a `Service` helper that wraps the same decision
   (`CanWriteGoal`, `ReviewCapabilities`, `SensitiveFieldsFor`).
2. **404 for invisible records**, one identical page whether the record does not exist or is not
   yours (people, series, meetings, goals, reviews, leave requests, feedback requests). The error
   page never marks the section the path belongs to.
3. **No sensitive field in ordinary HTML.** Date of birth, national id, home address, personal phone
   and compensation appear only on the sensitive screen, only after a POST that reads through
   `GetSensitive` (audited per field before any value is returned). The GET page reads and audits
   nothing. Fields the caller may not read show "Not available to you".
4. **Leave stays masked** exactly as the directory masks it: org chart, directory search, person
   pages and the calendar all go through `GetPerson`, `ListPeople`, `Reports` and `LeaveCalendar`.
5. **Anonymous feedback**: while a cycle is open no page draws a response count or any per-reviewer
   state, even if the service returns counts to the viewer. The inbox says who to review, never who
   asked. Calibration data is never drawn for the subject.
6. **XSS-proof**: `html/template` everywhere, no `template.HTML`, no inline script or style, CSP
   `script-src 'none'`. Hostile names and notes are tested.
7. **CSRF** on every POST (session token, constant-time compare) plus `Sec-Fetch-Site` check;
   login uses a double-submit cookie.
8. **No caching of personal pages**: `Cache-Control: no-store`, never `ETag` or `Last-Modified`.
9. **Valid credentials are never throttled**: only failed logins draw on the per-address budget.
10. **Bounded work**: directory search scans at most 500 people and shows 50; the manager chain
    stops at 12 levels and never revisits an id (a corrupt cyclic chain terminates); lists are capped.
11. **Accessibility and phones**: labels on every field, one `h1`, landmarks, skip link, tables with
    captions, keyboard only, WCAG AA contrast in both colour schemes, `prefers-reduced-motion`,
    mobile-first CSS, 44 px targets, web app manifest (installable). Every string is in the message
    catalogue.

## Authentication

The user presents a personal access token (PAT, created in the admin UI; SSO users hold
`user:sso-...` PATs) once at `/login`. The server authenticates it with the same `Authenticator`
chain as the API and requires a person record that is active staff. It then keeps a server-side
session: the cookie value is 256 random bits, the server stores only its SHA-256; the cookie is
`HttpOnly`, `SameSite=Strict`, `Secure` (and `__Host-` prefixed) except in plain-HTTP development, scoped to
the people origin. A new session is always minted at login (a planted id is never adopted), logout and
timeouts delete it server-side (idle 30 minutes, absolute 8 hours, both configurable), and each
session re-checks its token every 5 minutes so a revoked PAT ends it; every request re-resolves the
caller so a leaver loses access at once. The admin UI's session cookie is never read here and is
never a module credential. **Follow-up**: SSO-integrated login (redirect through can-core's OIDC
flow and mint a module session without pasting a PAT); see `research.md`.

## User stories and acceptance

- **US1 Sign in and out.** Login, logout, timeouts, fixation, throttling, anonymous redirect.
- **US2 Home.** My tasks (onboarding checklist, complete), approvals waiting (only when the
  `ApprovalSource` seam is wired), upcoming 1:1s, goals in progress, notifications link.
- **US3 Profile and sensitive screen.** Edit preferred name and work phone; sensitive details for
  self and HR with per-field masking and an audit notice.
- **US4 Directory and org.** Search with leave masked; org chart (reporting line, reports); manager
  team page (reports with links to their goals and 1:1s).
- **US5 1:1s and goals.** Series, meetings, agenda, shared notes, action items, the manager-private
  notes box visible only to the manager; goals with key results, progress log, alignment and visibility.
- **US6 Time off.** Balances by leave type, request form with half days, draft/submit/cancel, the
  manager queue with approve and decline, the team calendar showing only "out".
- **US7 Reviews.** My review (self-assessment, nominate peers, released outcome, acknowledgement),
  feedback requests inbox, manager review form, nominations approval, HR sees calibration read-only.

## Out of scope

SSO-integrated login; HR administration screens (cycles, templates, calibration, release, leave
types, holidays, blackouts, balance adjustments: JSON API); service worker or push notifications
(CSP forbids script); editing sensitive fields; a browser or cluster verification pass.
