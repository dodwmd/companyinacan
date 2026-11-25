# Feature Specification: Work Tracker Web UI (Epic E14c)

**Feature Branch**: `023-work-ui` (git branch `epic/E14c-work-ui`)

**Created**: 2026-11-25

**Status**: Implemented offline (UI, tests, docs); not run against a real cluster or a real browser
beyond the local compose stack

**Input**: Epic E14, UI slice: a server-rendered, Jira-like web UI for the in-house `work` tracker
(E14a, `specs/011-work-tracker`): projects, backlog, board, sprint planning, issue detail, epics and
a manager dashboard. SPEC.md sections 3.3 (work tracking), 6.1 (footprint), clients (web and
Android/PWA).

## Scope of this slice

In: pages under `/ui/` served by `can-work` itself (package `core/work`, templates and stylesheet in
`core/work/webui`); sign-in with a personal access token; project list; project home with filters
kept in the URL and an inline create form; issue detail with every action; board per sprint with
move and rank buttons; sprint planning; epic progress; the manager dashboard; empty states, error
pages and a conflict page; message catalogue; golden HTML, accessibility, XSS, CSRF, scope and
route-contract tests; `docs/work-ui.md`; a UI check in `dev/compose/smoke.sh`.

Out (recorded in tasks.md as gaps): single sign-on to the module UI (waits for the SSO epic; PR #30
is the can-core side), a public route for the UI through can-core's installer, project creation and
workflow editing in the UI (use the admin UI or API), attachment upload, bulk edit, keyboard
shortcuts, saved filters with names, drag and drop, notifications, a people picker (no call to the
people service), burn-down charts, localisation beyond English.

## User Scenarios & Testing

### User Story 1 - Sign in and see only my projects (Priority: P1)

A person pastes a personal access token on the sign-in page and lands on the list of projects their
token covers. A token limited to project ACME never shows SEC, in any page, search or count.

**Why this priority**: every other story depends on a session, and the scope rules are the
security property of the tracker (E14a QA lessons: IDOR, 404 for invisible).

**Independent Test**: `TestUILoginSessionCookieAndNoEcho`, `TestUIScopeIsEnforcedOnEveryRoute`,
`TestUIRequiresLoginAndRedirectsSafely`.

**Acceptance Scenarios**:

1. **Given** a valid token, **When** the form is posted, **Then** 303 to `/ui/` (or a safe `next`),
   a new session cookie (`HttpOnly`, `SameSite=Strict`, `Secure` over TLS, random id), and the token
   is never shown, logged or placed in the cookie.
2. **Given** a wrong token, **Then** 401 with a generic message and the token not echoed; after the
   per-address failure limit, 429 after a delay; a valid token is never throttled.
3. **Given** a cookie value chosen by someone else sent with the login, **Then** it is ignored and
   a new id is issued (no session fixation).
4. **Given** a revoked or expired token, **Then** the next request ends the session (the token is
   checked on every request).
5. **Given** a token scoped to ACME, **When** any `/ui/` route names a SEC project, issue or sprint,
   **Then** 404, with the same page as a missing key; no SEC title appears anywhere.

### User Story 2 - Triage the backlog (Priority: P1)

A team member filters the backlog (status, priority, type, assignee, label, free text, the search
language), pages through it, bookmarks the filter, and creates an issue from the same page.

**Independent Test**: `TestUIBacklogFiltersPaginationAndEpics`, `TestUIIssueLifecycle`.

**Acceptance Scenarios**:

1. **Given** filter fields, **Then** the URL carries them and the server composes the existing safe
   search language from them; fields that could add extra terms are refused.
2. **Given** a query the language rejects, **Then** a 400 page explains why; a search that runs too
   long is stopped by the store's deadline and answered 503.
3. **Given** the inline create form submitted twice (double click), **Then** one issue exists.
4. **Given** hostile titles, descriptions, labels, assignees and comments, **Then** every page shows
   them as text (see story 5).

### User Story 3 - Work the board without a mouse (Priority: P1)

A person moves a card to the next or previous status and changes its order using ordinary buttons,
on a phone or with a keyboard only. If somebody else changed the card meanwhile, they are told and
nothing is overwritten.

**Independent Test**: `TestUIBoardMoveAndRank`, `TestUIIssueLifecycle` (stale version).

**Acceptance Scenarios**:

1. **Given** a card, **Then** it offers exactly the workflow's allowed moves as buttons, labelled
   with the target status; there is no drag and drop and no JavaScript.
2. **Given** two people moving the same card, **Then** the second gets a 409 page showing the
   issue as it is now; the first change stands.
3. **Given** the order buttons (up, down), **Then** the card is ranked relative to its neighbour in
   its column; a stale card is reported.

### User Story 4 - Plan a sprint (Priority: P2)

A lead creates a sprint, adds backlog issues, starts it, watches it on the board and completes it,
moving unfinished work to the next sprint or the backlog.

**Independent Test**: `TestUISprintPlanning`.

### User Story 5 - Hostile content stays inert (Priority: P1)

**Independent Test**: `TestUIHostileContentIsInert`, `TestRenderTextNeverEmitsActiveContent`.

**Acceptance Scenarios**: titles, descriptions, comments, labels, assignees, sprint names, goals,
attachment names and URLs, filter values and path parts never produce a live tag, an event handler
attribute or a `javascript:`/`data:` link; markdown is a tiny subset (paragraphs, lists, fenced
code, bold, inline code, `http(s)` links); raw HTML is text; links carry
`rel="noopener noreferrer nofollow"`.

### User Story 6 - Managers see P0 and P1 across the team (Priority: P2)

A manager enters the people on their team and sees each person's open P0 and P1 issues, limited to
projects the token can see. The list lives in the URL; no people service is called.

**Independent Test**: `TestUIBacklogFiltersPaginationAndEpics` (dashboard part).

### Edge Cases

- Read-only tokens see no write forms and get 403 on every write route (`TestUIReadOnlyTokenCannotWrite`).
- Archived projects reject changes with a clear 409.
- Oversized forms are 413; unknown paths 404 and a wrong method 405, as HTML.
- A page past the store's offset ceiling is clamped, not an error.
- Sessions live in memory: a restart signs everybody out.

## Requirements

### Functional Requirements

- **FR-001** Every route is gated by session and permission as listed in
  `contracts/ui-routes.md`; a contract test drives the table.
- **FR-002** The UI calls the store with the caller's own principal; no UI code re-implements scope.
- **FR-003** Sessions are server side, keyed by the hash of a random 256-bit id, bounded in number
  (global and per subject), expiring (8 h idle, 24 h absolute), and re-authenticated against the
  token on every request.
- **FR-004** Every post carries the session CSRF token and passes the `Sec-Fetch-Site` and `Origin`
  checks; the login form uses a double-submit cookie.
- **FR-005** Every change to an existing issue carries the version the page showed; a mismatch is a
  409 conflict page and no change.
- **FR-006** Responses carry a CSP without scripts; no inline script or style, no `style=`
  attribute, no JavaScript file.
- **FR-007** Text is escaped by `html/template`; user markdown goes through `renderText` only.
- **FR-008** Lists are paginated; epic progress, sprint pages, boards and the dashboard have fixed
  upper bounds on rows and queries; every request has a deadline.
- **FR-009** Every user-visible string is in the message catalogue (`ui_messages.go`).
- **FR-010** Accessibility: landmarks, skip link, one `h1`, labelled controls, table captions,
  keyboard-operable everything, visible focus, WCAG AA contrast in both colour schemes, no
  motion when the user asks for none.
- **FR-011** Responsive: tables become stacked cards and the board becomes one column on a phone;
  dark and light schemes follow the system.

### Key Entities

No new stored entity. Runtime-only: UI session (token, subject, CSRF token, times). See
[data-model.md](data-model.md).

## Success Criteria

- **SC-001** `go test -race ./core/work/... ./test/...` and `make test` pass, with the UI tests
  included.
- **SC-002** All 28 golden pages pass the static accessibility checklist.
- **SC-003** The compose smoke test signs in and shows the SMOKE project through the edge.
- **SC-004** The binary stays one static executable; no new dependency.
