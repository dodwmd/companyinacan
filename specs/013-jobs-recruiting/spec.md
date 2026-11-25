# Feature Specification: Jobs and Recruiting, first slice (E16a)

**Branch**: `epic/E16a-jobs` | **Created**: 2026-11-25 | **Status**: built offline, experimental
**Source**: SPEC.md *Recruiting & job-board syndication*, section 3.7 (sensitive data), section 4.3 (capabilities and events), constitution V.

## Summary

An in-house `jobs` module (`cmd/can-jobs`, `core/jobs`) that lets a company publish roles, collect applications and run a hiring pipeline, with job-board syndication built in as far as the boards allow. It follows the layered plan in SPEC.md: an own careers page with schema.org `JobPosting` data (works without anyone's permission), feeds, and connector interfaces for partner-only APIs (LinkedIn, Indeed, Glassdoor), which ship as honest stubs.

## User scenarios

1. **Recruiter publishes a role.** Creates a draft with sections, location, remote kind, employment type and an optional pay range, optionally has it approved, opens it. The careers page, sitemap and feeds list it; closing it makes the page `noindex` and drops it from them.
2. **Candidate applies.** Reads the role, submits a form (name, email, optional phone, cover letter, resume) with a consent checkbox and an optional talent-pool opt-in. Bots and floods are slowed; a duplicate is accepted without disclosing that it was known.
3. **Recruiter runs the pipeline.** Moves applications through configurable stages, rejects with a reason code, assigns interviewers, reads feedback, and hires from an offer stage, which publishes `candidate.hired.v1`.
4. **Hiring manager** sees only their own jobs and applications. **Interviewer** sees a name, the stage and the resume of applications they are assigned to, writes a scorecard, and sees colleagues' feedback only after submitting theirs.
5. **Admin / DPO** answers an erasure request, places a legal hold, reviews retention, reads diversity aggregates, audits who read what.

## Functional requirements

- **FR-1 Jobs.** Lifecycle draft, open, paused, closed; closing needs a reason from a fixed list; edits guarded by `If-Match`; department, location, remote kind, applicant countries, employment type, optional salary range with a separate `publish_salary`, structured description sections; `valid_through` defaulting to 60 days; expiry closes the job. An `Approver` hook gates opening when `approval.required` is set; the requester cannot approve their own request.
- **FR-2 Careers pages.** Server-rendered `/careers` (filters), `/careers/{slug}` with JSON-LD (`datePosted`, `validThrough`, `hiringOrganization`, `jobLocation`, `applicantLocationRequirements` + `TELECOMMUTE` for remote, `baseSalary` only when published), apply form, thank-you page, `/careers/sitemap.xml`, `/robots.txt`. Paused, closed and expired jobs are `noindex` (meta and header), carry no JSON-LD and no form. Drafts are 404. All user text is escaped; JSON-LD cannot close its script element.
- **FR-3 Feeds and boards.** Generic XML, Indeed-format XML and JSON feeds, each with golden tests. A `Syndicator` interface with a recording fake, feed-only boards, and a `PartnerStub` for LinkedIn, Indeed and Glassdoor that returns `needs_partner_access` without any network call. A board failure is recorded and never fails a lifecycle change. Board status is visible through the API.
- **FR-4 Intake.** Public multipart POST with: signed timing token, honeypot, same-origin check, per-address hourly limit, optional CAPTCHA, field and body caps, one resume (size and type limits, content sniffed against the extension, `Scanner` hook, a scan error fails closed). File bytes go to a `ResumeStore` (directory on the volume); the database holds metadata only. Consent version and time are recorded; the form never rewrites an existing candidate.
- **FR-5 Pipeline.** Stages configurable (first initial, last hired, optional offer); move, reject (reason code + sealed note), hire (from an offer stage only); interviewer assignment; scorecards (rating 1-4, recommendation, sealed comments) with draft/submitted states and blind-until-submitted visibility; talent pool; duplicate detection by email HMAC; manual add by staff with a consent attestation.
- **FR-6 Visibility.** Roles admin, recruiter, hiring_manager, interviewer plus an optional token job scope, applied to every query as a parenthesised predicate; out-of-scope objects are 404; global operations need an unscoped admin token; sealed and contact fields are masked per role identically in every representation; events carry no candidate data.
- **FR-7 Privacy.** Sealed fields (XChaCha20-Poly1305, AAD binds row and column, key rotation); every read of candidate data audited before it is returned (fail closed); append-only hash-chained audit; retention timers; legal hold; erasure with tombstones in the backup ledger's shape; `purge(subject, class)` replay hook; VACUUM and WAL truncate after erasure and retention; diversity answers kept apart and read only as suppressed aggregates.
- **FR-8 Events and contracts.** `job.opened.v1`, `application.received.v1`, `application.stage_changed.v1`, `candidate.hired.v1` through a signed webhook outbox; capabilities `jobs.v1` and `ats.v1`; OpenAPI 3.1 checked against the router.
- **FR-9 Operations.** REST API with scoped tokens, rate limiting that never throttles valid credentials, a request deadline bounding database work, structured logs without bodies or query strings, healthcheck, graceful shutdown.

## Non-goals (this slice)

Recruiter web UI; interview scheduling and offer letters (booking, `documents`); real board connectors and inbound board applications; email-to-apply; applicant email verification; a real virus scanner; reopening a rejected application; bulk operations; Helm templates; a cluster run.

## Success criteria

- `make test` and `go test -race ./...` pass; every route is in the OpenAPI document and the reverse.
- Scope and role tables prove no cross-job, cross-application or cross-candidate access for each role.
- After an erasure no plaintext of the candidate's data remains in the database file or its WAL, and the tombstones are accepted by `core/backup`'s ledger.
- Idle RSS is measured and recorded (research R8).
