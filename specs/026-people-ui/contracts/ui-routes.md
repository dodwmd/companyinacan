# Contract: people UI routes

Served by `can-people` on the same listener as the JSON API when `ui.enabled: true` (in the dev
compose stack: `http://people.localhost:8080/`). `/v1/*` and `GET /healthz` belong to the API
(`specs/012-people-directory/contracts/people-api.yaml`); everything else is the UI. Source:
`core/people/ui*.go`. Tests: `TestUIRoutesMatchContract` (this table against `people.UIRoutes()`),
`TestUISessionRoutesRequireASession` (every "yes" route answers `303 /login` without a session),
`TestUIContractGatesNameRealServiceMethods` (every method named below exists).

**Authorisation.** The UI has no authorisation logic. The **Gate** column names the Service
method(s) a route calls; each asks the policy layer (`core/people/policy.go`: `Decide`,
`FieldAccess`) with the signed-in person's roles and, for a personal access token, its manifest
permissions. A route the policy does not allow answers `404` for a record the caller may not know
exists (people, series, meetings, goals) and `403` for an action on a record they may see. The
**Policy action** is the `Decide`/`FieldAccess` action behind the method.

**Unsafe requests** are `application/x-www-form-urlencoded` posts carrying the hidden field `csrf`
(the session's token; on the login form, the double-submit token from the login cookie) and, when
the browser sends `Sec-Fetch-Site`, only `same-origin` or `none` is accepted. Failures are `403`.
Unauthenticated requests to a session route get `303 /login`. Unknown paths get the 404 page.
Every response carries `Cache-Control: no-store` (static files excepted) and no `ETag` or
`Last-Modified`.

| Route | Session | Gate (Service method) | Policy action | Purpose |
|---|---|---|---|---|
| `GET /login` | no | - | - | Sign-in form; sets the login CSRF cookie |
| `POST /login` | no | `ResolveActor` via the shared Authenticator | - | Field `token` (a personal access token), `csrf`. 303 `/` on success; 401 generic, 403 stale form, 429 after repeated failures (valid tokens are never throttled) |
| `GET /login/ticket` | no | - | - | Single sign-on interstitial (query `t`, a launch ticket from the admin UI's Apps page): only renders a form with hidden `csrf` and `ticket`, never consumes the ticket; sets the login CSRF cookie; 404 when can-core is not wired, 400 for a malformed ticket |
| `POST /login/ticket` | no | `ResolveActor` over the principal built from can-core's redeem answer | - | Fields `ticket`, `csrf`. Redeems over the module API; the person must be linked in the directory (`sso_subject`). 303 to the validated landing path (default `/`) with a new session; 401 generic (replay, foreign, no people permission, unlinked), 403 stale form, 429 after repeated failures (valid tickets are never throttled), 503 when can-core cannot answer, 404 when can-core is not wired |
| `POST /logout` | yes | none, ends only the caller's own session | - | Ends the server-side session, 303 `/login` |
| `GET /{$}` | yes | `MyTasks`, `ListSeries`, `ListMeetings`, `ListGoals` | check.list_own, 1on1.read, goal.read | Home: my tasks, upcoming 1:1s, goals in progress, approvals waiting (only when the approvals seam is wired), notifications link |
| `POST /tasks/{id}/complete` | yes | `CompleteTask` | check.complete | Mark one of my checklist tasks done |
| `GET /me` | yes | `GetPerson`, `SensitiveFieldsFor` | dir.read | My profile and the edit form |
| `POST /me` | yes | `UpdatePerson` | dir.update_self | Fields `preferred_name`, `work_phone` only |
| `GET /me/sensitive` | yes | `SensitiveFieldsFor` | FieldAccess read | Notice and the list of fields I may reveal; reads and audits no value |
| `POST /me/sensitive` | yes | `GetSensitive` | FieldAccess read, per field | Reveal; audited per field before any value is returned |
| `GET /people` | yes | `ListPeople`, `ListOrgUnits` | dir.read | Directory search, query `q` (at most 64 characters, at most 500 people scanned) |
| `GET /people/{id}` | yes | `GetPerson`, `Reports`, `ListGoals`, `ListSeries`, `SensitiveFieldsFor` | dir.read, goal.read | A person; 404 when invisible |
| `GET /people/{id}/sensitive` | yes | `SensitiveFieldsFor` | FieldAccess read | As `/me/sensitive` for HR or admin; 404/403 for everyone else |
| `POST /people/{id}/sensitive` | yes | `GetSensitive` | FieldAccess read, per field | Reveal, audited |
| `GET /org` | yes | `GetPerson`, `Reports` | dir.read | My reporting line (at most 12 levels, no id twice) and reports |
| `GET /org/{id}` | yes | `GetPerson`, `Reports` | dir.read | Same for another visible person |
| `GET /team` | yes | `Reports`, `ListSeries` | dir.read, 1on1.read | My direct reports with links to their goals and 1:1 series |
| `GET /oneonones` | yes | `ListSeries` | 1on1.read | My 1:1 series |
| `POST /oneonones` | yes | `CreateSeries` | 1on1.create | Fields `report_id`, `cadence`; manager of that report only |
| `GET /oneonones/{id}` | yes | `ListSeries`, `ListMeetings` | 1on1.read | A series and its meetings; 404 for non-participants |
| `POST /oneonones/{id}/meetings` | yes | `ScheduleMeeting` | 1on1.read | Field `scheduled_at` (`YYYY-MM-DDTHH:MM`, UTC) |
| `GET /meetings/{id}` | yes | `GetMeeting` | 1on1.read, 1on1.private | Agenda, shared notes, actions; the private notes box only when the service returns private notes (the manager) |
| `POST /meetings/{id}/agenda` | yes | `AddAgendaItem` | 1on1.write | Field `text` |
| `POST /meetings/{id}/notes` | yes | `SetNotes` | 1on1.write | Shared notes, field `text` |
| `POST /meetings/{id}/private-notes` | yes | `SetNotes` | 1on1.private | Manager-private notes; 404 for the report |
| `POST /meetings/{id}/actions` | yes | `AddAction` | 1on1.write | Fields `text`, `assignee_id`, `due_date` |
| `POST /meetings/{id}/status` | yes | `SetMeetingStatus` | 1on1.write | Field `status` (`scheduled`, `held`, `cancelled`) |
| `POST /actions/{id}/status` | yes | `UpdateAction` | 1on1.write | Field `status` (`open`, `done`) |
| `GET /goals` | yes | `ListGoals`, `ListOrgUnits` | goal.list_own, goal.read | My goals; query `owner` (a person id) or `scope` (`team`, `company`) |
| `POST /goals` | yes | `CreateGoal` | goal.create | Fields `scope`, `team_id`, `title`, `description`, `period`, `visibility` |
| `GET /goals/{id}` | yes | `GetGoal`, `ListUpdates`, `ListGoals`, `CanWriteGoal` | goal.read, goal.write | A goal with key results, alignment and the progress log; 404 when not visible |
| `POST /goals/{id}` | yes | `UpdateGoal` | goal.write | Fields `title`, `description`, `period`, `status`, `visibility`, `parent_id` |
| `POST /goals/{id}/key-results` | yes | `AddKeyResult` | goal.write | Fields `title`, `unit`, `start_value`, `target_value` |
| `POST /key-results/{id}/progress` | yes | `RecordProgress` | goal.write | Fields `value` (finite), `comment` |
| `GET /timeoff` | yes | `ListLeaveTypes`, `LeaveBalances`, `ListLeaveRequests` | leave.balance, leave.read | Balances by leave type, the request form (half days), my requests |
| `POST /timeoff` | yes | `CreateLeaveRequest`, `SubmitLeaveRequest` | leave.write | Fields `type_id`, `start_date`, `start_part` (`full`, `pm`), `end_date`, `end_part` (`full`, `am`), `note`, `action` (`submit` or `draft`) |
| `GET /timeoff/queue` | yes | `ListLeaveRequests` | leave.read | Pending requests of my direct reports (HR: everyone); never my own, never drafts, no notes |
| `GET /timeoff/calendar` | yes | `LeaveCalendar`, `ListLeaveTypes` | leave.calendar | Who is out; query `from`, `days` (at most 92). Colleagues show only as out; type and status only where the service returns them |
| `GET /timeoff/{id}` | yes | `GetLeaveRequest` | leave.read | One request with its note; 404 for anyone but the person, the manager and HR, and for drafts |
| `POST /timeoff/{id}/submit` | yes | `SubmitLeaveRequest` | leave.write | Submit a draft |
| `POST /timeoff/{id}/cancel` | yes | `CancelLeaveRequest` | leave.cancel | Cancel; the ledger is reversed by the service |
| `POST /timeoff/{id}/decide` | yes | `DecideLeaveRequest` | leave.decide | Field `decision` (`approve`, `reject`); the requester is refused |
| `GET /reviews` | yes | `ListReviews`, `ListCycles` | rev.meta | Reviews I may know about (mine, my reports', released ones down my chain; HR all) |
| `GET /reviews/{id}` | yes | `GetReview`, `GetCycle`, `ReviewCapabilities`, `ListNominations`, `ListPeople` | rev.self.read, rev.manager.read, rev.calibration.read, rev.outcome.read, rev.peer.read | The review as the service filters it; no response counts or per-reviewer state while the cycle is open; calibration never for the subject; 404 when unknown |
| `POST /reviews/{id}/self` | yes | `SaveSelf` | rev.self.write | Fields `r_<question>` (rating), `t_<question>` (text), `submit` |
| `POST /reviews/{id}/manager` | yes | `SaveManager` | rev.manager.write | As above plus `rating`, `summary`; the report is refused |
| `POST /reviews/{id}/nominations` | yes | `Nominate` | rev.nominate | Fields `reviewer_id`, `kind` (`peer`, `upward`) |
| `POST /reviews/{id}/acknowledge` | yes | `Acknowledge` | rev.ack | Field `comment`; once, after release |
| `POST /nominations/{id}/decide` | yes | `DecideNomination` | rev.nomination.decide | Fields `decision`, `review_id` (only used to choose the page shown next) |
| `GET /feedback` | yes | `MyFeedbackRequests` | rev.feedback.give | The reviewer's inbox; says who to review, never who asked |
| `GET /feedback/{id}` | yes | `MyFeedbackRequests` | rev.feedback.give | The answer form; 404 when it is not addressed to me |
| `POST /feedback/{id}` | yes | `SubmitFeedback` | rev.feedback.give | Fields `r_<question>`, `t_<question>`; once; an anonymous cycle stores no reviewer |
| `GET /static/{file}` | no | - | - | Embedded stylesheet and icon (cacheable) |
| `GET /manifest.webmanifest` | no | - | - | Web app manifest (installable on a phone; no service worker) |

## Not in this build

HR administration of reviews (templates, cycles, calibration, release) and of leave types, holidays
and blackouts stays in the JSON API; the review page shows HR the calibration data read-only and
says where to act. A can-core approval for leave or release (`ApprovalEngine`) is decided in the
admin UI approvals inbox, which the home page links to when wired.
