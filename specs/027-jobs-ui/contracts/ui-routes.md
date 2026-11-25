# Contract: jobs UI routes

Served by `can-jobs` on the same listener as the JSON API and the careers pages when
`ui.enabled: true` (in the dev compose stack: `http://jobs.localhost:8080/`). `/v1/*`,
`/careers*`, `/feeds/*`, `/robots.txt` and `GET /healthz` belong to the API and the public pages
(`specs/013-jobs-recruiting/contracts/`); every other path is the UI. Source: `core/jobs/ui*.go`.
Tests: `TestUIRoutesMatchContract` (this table against `jobs.UIRoutes()`),
`TestUISessionRoutesRequireASession` (every "yes" route answers `303 /login` without a session),
`TestUIContractGatesNameRealStoreMethods` (every method named below exists),
`TestUIContractRolesMatchThePermissionMatrix` (the **Roles** column equals the permission matrix of
`core/jobs/auth.go` for the **Action**, and every `GET` route with an action is probed with a token
of each role: allowed roles never get `403`, the others always do).

**Authorisation.** The UI has no authorisation logic. The **Gate** column names the Store
method(s) a route calls; each checks the caller's role (**Action**, **Roles**) and applies the
visibility clause for that role and for the token's own job list (a hiring manager reaches only the
requisitions where they are the hiring manager, an interviewer only the applications they are
assigned to). A record outside the caller's scope answers one identical `404` whether it exists or
not; a role that may not use the feature at all answers `403`. Page helpers (`Caps`, `Require`,
`IsAssigned`) wrap decisions the Store enforces again on every write.

**Unsafe requests** are `application/x-www-form-urlencoded` posts carrying the hidden field `csrf`
(the session's token; on the login form, the double-submit token from the login cookie) and, when the
browser sends `Sec-Fetch-Site`, only `same-origin` or `none` is accepted. Failures are `403`.
Unauthenticated requests to a session route get `303 /login`. Unknown paths get the 404 page. Every
response carries `Cache-Control: no-store` (static files excepted) and no `ETag` or `Last-Modified`.
An expired database deadline answers `503` with `Retry-After`.

| Route | Session | Gate (Store method) | Action | Roles | Purpose |
|---|---|---|---|---|---|
| `GET /login` | no | - | - | - | Sign-in form; sets the login CSRF cookie |
| `POST /login` | no | the shared Authenticator chain | - | - | Field `token`, `csrf`. 303 `/` on success; 401 generic (unknown, revoked and malformed tokens alike), 403 stale form, 429 after repeated failures (valid tokens are never throttled) |
| `POST /logout` | yes | none, ends only the caller's own session | - | - | Ends the server-side session, 303 `/login` |
| `GET /{$}` | yes | `FunnelCounts`, `ListJobs`, `ListMyInterviews`, `ListStages` | - | admin, recruiter, hiring_manager, interviewer | Dashboard by capability: pipeline funnel and requisitions (aggregate counts only), or the interviews assigned to me |
| `GET /jobs` | yes | `ListJobs` | job.read | admin, recruiter, hiring_manager | Requisition list, query `status`, `cursor` |
| `GET /jobs/new` | yes | `Require` | job.write | admin, recruiter | New requisition form |
| `POST /jobs` | yes | `CreateJob` | job.write | admin, recruiter | Create a draft; 303 to the job |
| `GET /jobs/{id}` | yes | `GetJob`, `FunnelCounts`, `ListApplications`, `ListStages` | job.read | admin, recruiter, hiring_manager | Requisition, lifecycle, approval status, careers link (open jobs only), pipeline board (at most 25 cards per stage) |
| `GET /jobs/{id}/edit` | yes | `GetJob`, `Require` | job.write | admin, recruiter | Edit form carrying the version |
| `POST /jobs/{id}` | yes | `PatchJob` | job.write | admin, recruiter | Save; field `version` is the `If-Match` (409 when stale); salary publish toggle `publish_salary` |
| `POST /jobs/{id}/transition` | yes | `TransitionJob` | job.write | admin, recruiter | Fields `to` (`open`, `paused`, `closed`), `reason` (required to close) |
| `POST /jobs/{id}/approval` | yes | `RequestApproval` | job.write | admin, recruiter | Ask the approver (can-core engine) |
| `GET /applications/{id}` | yes | `GetApplicationMeta`, `ListScorecards`, `IsAssigned`, `ListStages` | app.read | admin, recruiter, hiring_manager, interviewer | Candidate name, stage, resume metadata and scan status, interviewers, history, scorecards under the blind rules. No contact data, no cover letter, no notes, no move reasons (a marker only); nothing sealed is decrypted or audited as read |
| `POST /applications/{id}/move` | yes | `MoveApplicationNote` | app.move | admin, recruiter, hiring_manager | Fields `from` (the stage the page showed; 409 when the application is no longer there), `to`, `note` (1 to 500 characters, sealed with the history entry), optional `return` (a job page) |
| `POST /applications/{id}/reject` | yes | `RejectApplication` | app.move | admin, recruiter, hiring_manager | Fields `reason` (fixed vocabulary), `note` (at most 2000 characters, sealed) |
| `POST /applications/{id}/hire` | yes | `HireApplication` | app.hire | admin, recruiter | Only from an offer stage; publishes `candidate.hired.v1` |
| `POST /applications/{id}/interviewers` | yes | `AssignInterviewer` | app.assign | admin, recruiter, hiring_manager | Field `subject` |
| `POST /applications/{id}/interviewers/remove` | yes | `RemoveInterviewer` | app.assign | admin, recruiter, hiring_manager | Field `subject`; removing yourself before submitting is refused by the Store (409) |
| `GET /applications/{id}/resume` | yes | `OpenResume` | app.read | admin, recruiter, hiring_manager, interviewer | Attachment, `application/octet-stream`, `nosniff`, sanitised file name; never inline; infected files are 404 |
| `POST /applications/{id}/reveal` | yes | `GetApplication`, `GetCandidate` | candidate.read | admin, recruiter, hiring_manager | Explicit reveal of contact data, cover letter, rejection note, notes and stage-move reasons as the role may read them; audited before anything is decrypted; at most 10 per minute per user |
| `POST /applications/{id}/notes` | yes | `GetApplicationMeta`, `PatchCandidate` | candidate.edit | admin, recruiter | Fields `notes`, `version` (409 when the candidate changed since the form was drawn) |
| `POST /applications/{id}/scorecard` | yes | `SaveScorecard`, `SubmitScorecard` | app.score | admin, recruiter, hiring_manager, interviewer | Fields `rating`, `recommendation`, `comments`, `action` (`save` or `submit`); needs an active assignment |
| `GET /candidates/{id}` | yes | `GetCandidateSummary`, `ListApplications`, `ListStages` | candidate.read | admin, recruiter, hiring_manager | Name, tags, talent-pool flag, applications. No sealed field |
| `POST /candidates/{id}/tags` | yes | `PatchCandidate` | candidate.edit | admin, recruiter | Field `tags` |
| `POST /candidates/{id}/talent-pool` | yes | `SetTalentPool` | talentpool.read | admin, recruiter | Field `member` (`on` records the candidate's consent) |
| `GET /interviews` | yes | `ListMyInterviews` | app.score | admin, recruiter, hiring_manager, interviewer | Active applications I am assigned to, with my scorecard state |
| `GET /talent` | yes | `SearchTalentPool` | talentpool.read | admin, recruiter | Talent pool, queries `q` (name prefix, at most 100), `tag`, `cursor`; 50 per page |
| `GET /reports/eeo` | yes | `EEOAggregate` | admin | admin | Aggregate diversity report, closed months, the Store's suppression (a question with any rare answer is shown as hidden, with no answer names) |
| `GET /settings` | yes | `Require`, `ListStages`, `Boards` | job.read | admin, recruiter, hiring_manager | Read-only: pipeline stages and job boards |
| `GET /static/{file}` | no | - | - | - | Embedded stylesheet and icon (cacheable) |
| `GET /manifest.webmanifest` | no | - | - | - | Web app manifest (installable on a phone; no service worker) |

## Not in this build

Editing pipeline stages (read-only here; `PUT /v1/stages` exists), email and interview templates (the
module has none), approving a requisition (`POST /v1/jobs/{id}/approval`), tokens, audit log,
erasure, legal hold and retention screens (API only), drag and drop, and any JavaScript.
