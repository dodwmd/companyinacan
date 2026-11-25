# Feature specification: Recruiting web UI (Epic E16b)

**Status**: built offline and tested; not exercised in a browser, with a screen reader, or on a
cluster ([V] items in `tasks.md`). **Depends on**: `specs/013-jobs-recruiting` (service, roles, blind
feedback, sealed fields, EEO counters, retention), `specs/023`/`026` (UI conventions that passed QA:
the work UI is still in review, so only the merged people UI of `specs/026-people-ui` was copied),
`specs/016-module-wiring` (tokens). **Principle**: constitution V (candidate data is personal data:
in-house, least privilege, audited reads).

## Why

SPEC.md lists a web UI and an Android/PWA client for recruiting. Until now `can-jobs` was a JSON API
plus public careers pages. Recruiters, hiring managers and interviewers need a phone-friendly UI for
requisitions, the pipeline, candidates and interview feedback, without anyone building a second
authorisation layer.

## Hard rules (the QA lessons of E16a and E13c, applied)

1. **One authorisation path.** The UI calls the same `Store` methods as the JSON API. Each decides by
   the caller's role (`auth.go` matrix) and scopes every query by role and by the token's own job
   list. The UI files never read the database, never call `Can`, never compare a role. A test
   (`TestUISourceHasNoSecondAuthorizationPath`) fails if `ui*.go` does. Where a page must know what
   to draw it asks `Principal.Caps()`, `Store.Require` or `Store.IsAssigned`, which wrap the same
   decisions the write path enforces again.
2. **Scope is the service's.** A hiring manager sees only requisitions where they are the hiring
   manager; an interviewer only applications they are actively assigned to; a job-scoped token keeps
   its scope. Anything outside is one identical `404` (a test compares the pages).
3. **Blind feedback.** An interviewer (or anyone ever assigned, or with a scorecard row, including a
   soft-removed interviewer) sees colleagues' scorecards only after submitting their own. Before
   that the page carries a constant notice: no count, no names, no status, not even whether any
   exists. Submitting needs an active assignment (the service says so).
4. **Diversity data never on candidate pages.** No page of a candidate, application, job or board
   mentions the diversity answers. The only view is the admin aggregate report: closed months, the
   service's suppression; a question with any rare answer is drawn as hidden **without** the answer
   names, because a rare answer's name is itself a disclosure.
5. **Sensitive fields only on an explicit, audited reveal.** The application page shows the name,
   stage, resume metadata and history. Email, phone, address, salary expectation, cover letter,
   rejection note and recruiter notes appear only after a `POST .../reveal` (not a link, so a prefetch
   cannot trigger it), limited to 10 per minute per user, audited by the service before it returns,
   and only as far as the role may read them ("not available to you" otherwise).
6. **Resumes** are downloaded through a safe handler: `attachment`, `application/octet-stream`,
   `nosniff`, `no-store`, a sanitised file name, never inline, never an object or iframe; the storage
   key is never in a page; the virus-scan status is shown; infected files are not offered and answer
   404.
7. **Escaped text.** Candidate names, notes and descriptions are rendered by `html/template` as plain
   text (`white-space: pre-wrap`); no markdown, no `template.HTML`, no inline script or style, CSP
   `default-src 'none'; script-src 'none'`, no `data:` URL anywhere (templates and stylesheet are
   scanned by a test).
8. **No email enumeration.** The UI has no email search. Login refuses an unknown, a revoked and a
   malformed token with the same message and status.
9. **Reasons are required and bounded.** A stage move needs a reason (1 to 500 characters), sealed
   with the history entry (migration 004) and shown only to roles that may read rejection notes. A
   rejection needs a reason from the fixed vocabulary and an optional note of at most 2000 characters.
   Closing a requisition needs a reason from the fixed vocabulary.
10. **CSRF** on every POST (session token, constant-time compare) plus `Sec-Fetch-Site`; login uses a
    double-submit cookie. **Valid credentials are never throttled**: only failures draw on the
    per-address budget.
11. **Sessions**: server-side, SHA-256 of the cookie value, HttpOnly, SameSite=Strict, `__Host-` and
    Secure unless `ui.insecure_cookies`, at most 5 per subject, idle 30 min, absolute 8 h, token
    re-checked every 5 minutes and **every minute for sessions that can write requisitions, move
    hires, edit candidates or read the diversity report** (a revoked recruiter must not keep reading
    candidate data). Fails closed.
12. **Bounded work**: every request runs under `db_timeout_seconds`; past it the page is a `503` with
    `Retry-After`. The board draws at most 25 cards per stage and says how many are left out; lists
    show 50 per page.
13. **No caching** of personal pages: `Cache-Control: no-store`, never `ETag`/`Last-Modified`.
14. **Accessibility and phones**: labels on every field, one `h1`, landmarks, skip link, captioned
    tables, keyboard only, WCAG AA contrast in both schemes, `prefers-reduced-motion`, mobile-first
    CSS, installable manifest. Every string is in the message catalogue; no drag and drop.

## Scope

Login; requisition list, create, edit, lifecycle (draft, open, paused, closed), salary publish
toggle, approval request and status; job page with a pipeline board per stage (move forward and back
and reject through forms); application page (candidate summary, resume metadata and download,
history, scorecards under the blind rules, interviewer assignment with the service's self-unassign
rule, offer/hire producing `candidate.hired.v1`); candidate page (tags, talent-pool consent);
talent-pool search (bounded); my interviews with the scorecard form; dashboards by capability
(recruiter and admin: funnel counts per stage and requisitions; hiring manager: own requisitions;
interviewer: assigned applications); the diversity report for administrators; careers preview link
for open jobs; read-only settings (stages, job boards).

## Not in this build

Editing pipeline stages (the API has it), email and interview templates (the module has none),
deciding an approval (API and can-core), token, audit, erasure, legal-hold and retention screens
(API), bulk actions, JavaScript, a native client. SSO redirect login (a can-core endpoint is needed;
people UI has the same limit). Helm exposure of the `ui` block (follow-up, as for people).

## Authentication

As in `specs/026-people-ui`: the user pastes a personal access token once at `/login`; the
server authenticates it with the same `Authenticator` chain the API uses (static admin token, stored
tokens, can-core introspection) and keeps a server-side session. The token is held in memory only,
to re-check revocation. Unlike people there is no person record: the token's subject and role are
the identity.
