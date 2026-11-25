# Recruiting web UI

Recruiters, hiring managers and interviewers use the `jobs` module from a browser or a phone:
requisitions, the pipeline, applications, interview feedback, the talent pool and the diversity
report. It is server-rendered Go templates with an embedded stylesheet: no JavaScript, no framework,
no build step. It is a client of the same service as the JSON API (see [`recruiting.md`](recruiting.md)),
so what a person may see or do is decided by the one permission matrix and scope, not by the pages.
Source: `core/jobs/ui*.go`. Specification: `specs/027-jobs-ui/`. Route table:
[`specs/027-jobs-ui/contracts/ui-routes.md`](../specs/027-jobs-ui/contracts/ui-routes.md).

This is a tool, not legal advice; candidate data is personal data. The UI is **experimental**: it is
unit- and contract-tested offline and has not been checked in a real browser, with a screen reader, or
on a cluster.

## Turning it on

Off by default. In `jobs.yaml`:

```yaml
ui:
  enabled: true
  # insecure_cookies: true   # plain-HTTP development only (drops Secure and the __Host- prefix)
  # idle_minutes: 30
  # absolute_hours: 8
```

The UI is served on the module's listener: `/v1/*`, `/careers*`, `/feeds/*`, `/robots.txt` and
`GET /healthz` stay the API and the public pages; every other path is the UI. In the dev compose stack
it is at `http://jobs.localhost:8080/` (`dev/compose/config/jobs.yaml` turns it on). Behind TLS leave
the cookie flags at their defaults. Put the UI behind an ingress that is **not** the public careers
route if you want it off the internet: the public paths are `/careers*`, `/feeds/*`, `/robots.txt`. The
Helm chart does not expose the `ui` block or a route yet (follow-up).

## Signing in

1. Create an access token for the role you need through `POST /v1/tokens` (admin) or the admin UI
   (can-core tokens introspected by the module). A token has one role (`admin`, `recruiter`,
   `hiring_manager`, `interviewer`) and optionally a list of jobs.
2. Open the site, paste the token at **Sign in**. The server checks it with the same authenticator
   chain the API uses.
3. You get a session: a random id in an `HttpOnly`, `SameSite=Strict` cookie (`Secure`, with a
   `__Host-` name, over TLS). The server stores only its SHA-256. Idle sessions end after 30 minutes,
   any session after 8 hours; **Log out** deletes it on the server. The token is held in server
   memory only (to re-check it) and never sent back to the browser. The re-check runs **every minute
   for sessions that can write requisitions, hire, edit candidates, read candidate contact data
   (recruiters and hiring managers) or read the diversity report**, every five minutes for the rest
   (interviewers): revoking a token or demoting someone takes effect within
   that time, and a token that cannot be verified ends the session. A restart signs everyone out.
4. A wrong, unknown or revoked token all get the same refusal. Only failed attempts count against the
   per-address limit; a valid token is never throttled.

## What each role sees

| Role | Pages |
|---|---|
| `admin` | everything below, plus the diversity report (needs a token without a job list) |
| `recruiter` | dashboard with funnel counts, all requisitions (or the token's jobs), create and edit, pipeline board, applications, hire, candidates, talent pool |
| `hiring_manager` | only requisitions where they are the hiring manager, their pipeline, move and reject, assign interviewers, read feedback; email and cover letter on reveal, no phone, address, salary expectation or notes; cannot hire |
| `interviewer` | **My interviews** and the applications they are currently assigned to: name, stage, resume, own scorecard. Nothing else; every other path is the same 404 or a 403 |

Navigation shows only what the role may use. A record outside your scope is one identical "not found"
page, whether it exists or not.

## Requisitions and the pipeline

* A requisition is a draft until opened; open, paused and closed follow the service's rules (closing
  needs a reason from a fixed list). When `approval.required` is on, ask for approval first; the
  decision happens in can-core or through the API.
* The salary range is always visible to staff; **Show the salary range on the public careers page**
  controls what the careers page and feeds publish. An open requisition links to its public posting;
  a draft says it is not public.
* The board shows each stage with at most 25 candidates and says how many more there are. Moving a
  candidate back or forward needs a **reason** (up to 500 characters). The form carries the stage
  the page showed; if the application has moved since (a stale page, a double click, a colleague's
  move) the move is refused with a conflict and nothing changes. The reason is stored sealed with
  the history entry, shown only to admins and recruiters, and only through the reveal described below. Rejecting needs a reason from the fixed
  list and an optional note (up to 2000 characters, sealed). There is no drag and drop.
* **Hire** appears only for an application in an offer stage and for roles that may hire. It moves the
  application to the hired stage and queues `candidate.hired.v1` for the people module.

## Candidate data

The application page shows the candidate's **name**, stage, source, resume file metadata (name, type,
size, virus-scan status) and the history of stage changes (who, when, from and to). It does not show
email, phone, address, salary expectation, cover letter, rejection note, recruiter notes or the
**reasons** recorded with stage moves: the history only says that a reason was recorded. The page
read does not decrypt any of those texts and its audit entry names only the name and resume metadata.

**Show contact details and notes** is a button (a POST). It decrypts and shows, for your role only,
the contact fields, cover letter, rejection note, recruiter notes and the move reasons. The service
writes the audit entry (naming `cover_letter`, `reject_note`, `stage_notes`, and the candidate fields)
**before** it decrypts, and the button is limited to 10 per minute per user. Fields your role may not
read say so. The recruiter notes form on that page carries the version it was drawn from; saving
over a newer edit is refused with a conflict (versions have one-second resolution, so two edits
within the same second are not told apart). Writes through the UI (move, reject, hire, scorecards)
do not decrypt or audit-read any of these texts either.

The resume downloads as an attachment (`application/octet-stream`, `nosniff`), never inside the page;
a file the scanner flagged is not offered. The scan status is shown ("Not scanned" when the module runs
without a scanner; the default `apply.scanner_required: true` refuses uploads in that case).

Names and notes are plain text: markup in a candidate's name appears as text.

## Interview feedback (blind)

Each person on the panel writes a scorecard (rating 1 to 4, recommendation, comments), saves drafts and
submits once; a submitted scorecard is final. Until you submit, you see **nothing** about your
colleagues' feedback, not even whether any exists: the page shows one constant notice. After you
submit, their submitted scorecards appear. This holds for anyone ever assigned, including someone who
was later removed (removal does not release them) and whatever their role. A person who was never
assigned and has no scorecard row on the application is **not blind**: a recruiter, admin or a hiring
manager reading an application on their own requisition sees every submitted scorecard, so assign
people to the panel (or have them write a draft) before they read if independence matters. You cannot remove your own assignment before submitting.

## Diversity report

Administrators only. Aggregate counts of the optional, anonymous diversity answers for the whole
organisation, **closed calendar months only** (empty until the first month ends), with the service's
suppression: a question where any answer has fewer than `eeo.min_cell` responses is shown as hidden,
without the answer names. Diversity answers appear on no candidate, application, job or board page.
The residual risks are in [`recruiting.md`](recruiting.md).

## Settings, dashboards and limits

* Settings is read-only: pipeline stages and the job boards with their status. Change stages through
  `PUT /v1/stages`. There are no email or interview templates in the module.
* Dashboards show counts per stage (aggregate, no names), the requisitions you can see and, for
  interviewers, your assignments.
* Every request is bounded by `db_timeout_seconds`; past it the page is a 503 ("try again").
* Reads are audited by the service like API reads, and only for what is actually read: lists and the
  application page audit names and resume metadata; the reveal audits the sealed texts (before
  decrypting them). Talent-pool search by name prefix is a case-insensitive `LIKE` over the talent
  pool and candidates table, which retention keeps small; it is not indexed, so a very large pool
  scans it (50 rows are returned per page).
* An expired database deadline is a 503, including on the scorecard and candidate lookups (these
  used to answer 404).

## Not done

Browser, phone and screen-reader verification; a run of the compose smoke test against a rebuilt
stack; Helm exposure; stage editing, approval decisions and retention or erasure screens (API only);
SSO redirect login; bulk actions.
