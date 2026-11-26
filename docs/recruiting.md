# Recruiting (jobs module)

`can-jobs` is the in-house recruiting module (epic E16, first slice E16a): job requisitions and
postings, public careers pages that Google for Jobs can index, job feeds, application intake, an
applicant tracking pipeline, a talent pool, consent, retention and erasure. Library `core/jobs`,
binary `cmd/can-jobs`, module `modules/jobs`, contract `specs/013-jobs-recruiting/`.

**Status: experimental.** Built and tested offline and measured locally. The image has not been
run in production (can-core installs the chart; CI only waits for the pod to be Ready on k3d), there is a recruiter web
UI ([`recruiting-ui.md`](recruiting-ui.md), experimental, not browser-verified; the API and the careers pages also exist), and no real virus scanner or job-board connector ships.

> **This is a tool, not legal advice.** Recruiting data is personal data and is regulated
> differently in every jurisdiction (data protection, anti-discrimination, employment and
> record-keeping law). The retention defaults, the consent text and the diversity questions are
> starting points. The operator is responsible for reviewing them against local law, for the
> privacy notice, and for any approvals required before collecting candidate data. Nothing here is a
> compliance guarantee.

## What it does and does not do

| Layer | What ships | What it needs from you |
|---|---|---|
| 1. Careers page | `/careers` list and one page per open job with schema.org `JobPosting` JSON-LD (`datePosted`, `validThrough`, `hiringOrganization`, `jobLocation`, `applicantLocationRequirements` for remote, `baseSalary` when you publish pay), `/careers/sitemap.xml`, `/robots.txt`, `noindex` for paused, closed and expired jobs | Route the public paths to the service and submit the sitemap to Google Search Console. **No agreement with Google is needed**, but Google decides what to index and when |
| 2. Feeds | `/feeds/generic.xml`, `/feeds/indeed.xml` (Indeed-format XML), `/careers/jobs.json` (JobPosting array) with golden tests | Each aggregator has its own terms. Some accept an open feed URL; **Indeed accepts feeds only from approved partners and ATS vendors**, and Glassdoor is operated through Indeed. The feed is a correct file, not an integration. Verify each board's current programme yourself |
| 3. Partner API connectors | The `Syndicator` interface (`core/jobs/syndicate.go`), a recording fake used in tests, and a `PartnerStub` for LinkedIn, Indeed and Glassdoor that returns `needs_partner_access` and never touches the network | **LinkedIn, Indeed and Glassdoor expose posting and apply APIs to approved partners only. There is no open self-serve API.** Until you (or this project, as a registered ATS partner) hold that access, those boards stay `needs_partner_access`. Writing a connector is implementing `Syndicator` against the board's partner documentation |
| 4. Paid multiposting bridges | Not in this slice | An adapter to your own multiposting account is a later `Syndicator` implementation |

The `GET /v1/boards` endpoint and `GET /v1/jobs/{id}/syndication` show the status of every board:
`feed` (pulled from our feeds), `connected` (a real connector) or `needs_partner_access`. Nothing is
shown as working that has not been verified.

Inbound applications from boards (apply webhooks, email-to-apply) are not built: all applications
come from the careers form or are added by staff. The `InboundPuller` interface exists for connectors.

## Roles and visibility

A token has one role and optionally a job scope (a list of job ids).

| Role | Sees | Can do |
|---|---|---|
| `admin` | everything | everything, plus tokens, audit, erasure, legal hold, retention, diversity aggregates, stages (global operations need a token with no job scope) |
| `recruiter` | all jobs (or the token's jobs) | create and run jobs, move, reject and hire, add candidates, talent pool, edit candidate notes |
| `hiring_manager` | only jobs where `hiring_manager` is the caller (and their applications) | move and reject, assign interviewers, read feedback. No phone, address, salary expectation or notes; cannot hire |
| `interviewer` | only applications they are assigned to | read name, stage and the resume; write their own scorecard. No contact data, no other applications, no candidate records |

Anything outside a caller's scope is a 404, so ids cannot be probed. Scope is applied to every
query as a parenthesised clause, whatever filters are added.

**Blind feedback.** A scorecard is private to its author until submitted, and submitted scorecards
are final. Anyone who has **ever** been assigned as an interviewer on an application, or has any
scorecard row on it (draft or submitted), whatever their role, sees other people's submitted
feedback only after submitting their own. Unassigning is a soft removal: the record stays, so
removing yourself, reading and re-assigning does not work; you cannot remove your own assignment
before submitting, and assign and unassign are audited. Until then the response says only that
other feedback is hidden (`others_hidden`), not how many colleagues have finished.

Two edge cases, stated plainly. A **removed interviewer who never submitted stays blind for good**
on that application: removal does not release them, by design. Staff who were **never assigned**
(and never wrote a scorecard row) are **not blind**: a recruiter, hiring manager or admin who
reads an application without sitting on its panel sees every submitted scorecard, so assign people
to the panel before they start reading if independence matters. Finalising a scorecard
(`POST .../scorecards/mine/submit`) needs an active assignment, or the `recruiter` or `admin`
role; a removed interviewer keeps the draft but cannot publish it.

**Diversity (EEO) data.** Off by default (`collect_eeo`). Whether to collect it is a legal decision
that varies by country: required or encouraged in some, restricted in others. When on, the answers
are voluntary and stored only as **counters** per month, field and answer (`eeo_counts`): no
per-response row, no id, no job, no timestamp finer than a month, `WITHOUT ROWID` so there is no
insertion sequence. (A first design stored one row per response; its row order tracked application
order and could be lined up with candidates by anyone holding the file or a backup.) The answers
are never shown to interviewers or in any candidate view and are read only as organisation-wide
aggregates by an admin, and the report covers **closed months only** (months before the current
UTC month). The open month is counted but not reported: otherwise polling the report, or reading
it twice a few minutes apart, would let the reader subtract the reads and see which cell one new
application moved. So the report is empty until the first month ends. If any answer of a question has fewer than `eeo.min_cell` (default 5)
responses, the whole question is suppressed, so a hidden cell cannot be recovered from the total;
per-job reports are not offered, so a global and a per-job report cannot be subtracted.
**Residual risk:** this is not formal anonymisation. Someone who can read the database file before
and after a single submission (for example two backups taken minutes apart) can see which counters
moved. Because the answers are not linked to a person, an erasure request cannot remove one
person's answers; the counters are deleted after `eeo_days`.

**Upgrading from the first design.** Migration 003 drops the per-response `eeo_responses` table.
Its rows are **discarded, not migrated** into counters (they were the linkable data), so diversity
figures collected before the upgrade are gone; export them from a backup first if you need them.
The store then runs `VACUUM` and `wal_checkpoint(TRUNCATE)` so the dropped rows do not linger in
the database file or its write-ahead log. Backups taken before the upgrade still contain them
until they expire.

## Candidate data, consent and retention

* **Consent at intake.** The form has a required consent checkbox with the configured text; the
  consent version and time are recorded on the candidate. The talent pool is a separate, optional
  checkbox. Staff who add a sourced candidate must attest that the candidate agreed (audited).
* **Sealed fields.** Email, phone, address, salary expectation, notes, cover letters, rejection
  notes and scorecard comments are encrypted per field with XChaCha20-Poly1305 and bound to their
  row and column. The keyring is a mounted file (mode 0400, newest key first); `can-jobs rotate-keys`
  re-seals old values. The email dedupe hash is an HMAC under a separate key (`CAN_JOBS_HASH_KEY`),
  so a stolen database cannot be checked against a list of known addresses; changing that key
  breaks duplicate detection for existing candidates.
* **Plaintext name.** `candidates.name` is stored unsealed so lists and name search work; email,
  phone, address, salary expectation and notes are sealed. A database file therefore shows names.
  Erasure and retention replace it with `[erased]`.
* **Duplicates.** A candidate is unique by email hash. A repeat application adds an application to
  the same candidate; a repeat for the same job is accepted and dropped without telling the
  applicant. The public form never changes an existing candidate's profile.
* **Resumes.** Bytes live on the volume under random names, never in the database. Limits: size
  (`max_resume_bytes`, default 5 MiB), type (PDF, DOCX, ODT, RTF, plain text; the content must match
  the extension). The `Scanner` hook runs before the application is stored; a scan error refuses the
  upload. **The built-in scanner scans nothing** (it marks files `skipped`), so `apply.scanner_required`
  defaults to **true**: until a real scanner (for example a ClamAV client) is wired in, the form
  shows no resume field and uploads are refused; applying without a resume still works. Setting it
  to false accepts unscanned files, which readers can then download (the download carries
  `X-Resume-Scan: skipped`). The magic-byte check only proves the first bytes look like the claimed
  type: a DOCX or ODT is any ZIP, and a PDF can carry active content. Treat every resume as untrusted
  and open it in a sandboxed viewer.
* **Retention.** The timer is off while any application is active; after the last decision it runs
  `rejected_days` (default 365), `talent_pool_days` (730, with consent) or `hired_days` (180; 0
  keeps the record until erased). A sweep every `sweep_interval_hours` scrubs due candidates and
  expires jobs past `validThrough`. Legal hold blocks retention and erasure. A retention purge
  writes the same erasure tombstones as a request, so restoring an older backup cannot bring back
  candidates whose retention period had ended.
* **Erasure.** `POST /v1/candidates/{id}/erasure` (unscoped admin) removes resume files, scorecards
  and notes, scrubs name and contact data, leaves an id-only shell so reports stay consistent, then
  vacuums the database and truncates the WAL. It writes three tombstones (`candidate-pii`,
  `candidate-resumes`, `candidate-evaluations`) in the shape `core/backup/ledger.go` takes; the
  `POST /v1/purge` hook replays them after a restore. Backups taken earlier still hold the data
  until they expire.
* **Audit.** Every read that returns candidate data, and every write, is recorded in an
  append-only, hash-chained table (ids and field names only). If the audit write fails the read
  fails and no data is returned. `can-jobs verify-audit` checks the chain; keep its latest hash
  somewhere a database administrator cannot write.
* **Events carry no personal data.** `job.opened.v1`, `application.received.v1`,
  `application.stage_changed.v1` and `candidate.hired.v1` hold opaque ids and job facts. The people
  module (E13) consumes `candidate.hired.v1` and reads name and contact details through the `ats`
  capability with its own audited token; `candidate_id` is its idempotency key.

## Careers page and spam controls

The apply endpoint is protected like the forms gateway: a signed timing token (rejects instant
submissions and expired forms), a honeypot field (a bot is told it succeeded), a same-origin check,
a per-address hourly rate limit (only the public form is limited; API credentials are never
throttled), size limits, and an optional CAPTCHA. The timing token is stateless (signed, bound to
the job, valid for `token_ttl_minutes`), so it can be replayed within its lifetime; the rate limit,
the duplicate rule and the CAPTCHA are what bound abuse, not the token. Pages carry a strict CSP, `nosniff` and no
third-party resources. There is no email verification of the applicant's address in this slice.

Serve the public paths (`/careers`, `/feeds`, `/robots.txt`) through the ingress and keep `/v1`
cluster-internal. `/robots.txt` is only right on a host this service owns; on a shared company
host, list `/careers/sitemap.xml` in the site's own robots.txt.

## Operating

```sh
can-jobs serve --config /etc/can-jobs/jobs.yaml      # also: check-config, healthcheck, verify-audit, rotate-keys, version
```

Under can-core nothing here is set by hand: the installer takes `company.name` from `company.yaml` and
sets `company.base_url` to `https://careers.<domain>` (`http` when the cluster serves no TLS), and
changing either upgrades the module. Run by hand, it needs `company.name`, `company.base_url`, `CAN_JOBS_ADMIN_TOKEN` and `CAN_JOBS_HASH_KEY` (32+
characters), and a keyring file. Optional: `CAN_JOBS_WEBHOOK_SECRET` with `webhook.url`,
`CAN_JOBS_FORM_SECRET` for stable form tokens across restarts and replicas. Settings and defaults
are in `core/jobs/config.go`; the HTTP contract is
`specs/013-jobs-recruiting/contracts/openapi.yaml`.

Mint a recruiter token and open a job:

```sh
curl -sX POST $API/v1/tokens -H "Authorization: Bearer $ADMIN" -d '{"kind":"service","subject":"rita","name":"rita","role":"recruiter"}'
curl -sX POST $API/v1/jobs -H "Authorization: Bearer $RECRUITER" -d @job.json
curl -sX POST $API/v1/jobs/$ID/transitions -H "Authorization: Bearer $RECRUITER" -d '{"to":"open"}'
```

Requisition approval is a hook (`Approver`): by default a job is approved immediately; with
`approval.required` a job cannot open until an approver (never the requester) approves it. Editing
an approved job withdraws the approval and pauses it if it was open, so unapproved text is never live. Wiring
can-core's approvals capability is a later step.

## Known limits of this slice

The recruiter web UI ([`recruiting-ui.md`](recruiting-ui.md)) is read-only for stages and has no approval decision, retention or erasure screens; no interview scheduling or offer letters (E15/E17 modules); no real virus
scanner, board connector, inbound board applications or email-to-apply; no applicant email
verification; no reopen of a rejected application; no bulk operations; the careers pages are
server-rendered with a minimal stylesheet and are not themed from `company.yaml`; no `can-jobs`
CLI client beyond the service commands; not run on a cluster.
