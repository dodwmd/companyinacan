# jobs

In-house recruiting module: job requisitions and postings, public careers pages with schema.org
`JobPosting` data, job feeds, application intake, an ATS pipeline with scorecards, a talent pool,
consent, retention and erasure. Library `core/jobs`, binary `cmd/can-jobs`, image `deploy/jobs`.
It provides the `jobs` and `ats` capabilities and publishes `job.opened.v1`,
`application.received.v1`, `application.stage_changed.v1` and `candidate.hired.v1` (the hire
hand-off the people module consumes).

**Status: experimental.** Built and tested offline and measured locally (16 MiB idle). The chart
installs through can-core's installer in unit tests; the image has not been built or run on a cluster,
there is no recruiter UI, and no real virus scanner or job-board connector ships.

**Installed under can-core** (see [`docs/module-wiring.md`](../../docs/module-wiring.md)): can-core generates the admin token, the candidate
e-mail hash key, the form secret and the sealed-field keyring once. Personal access tokens are
introspected through can-core and become the strongest role their permissions cover (`jobs.admin`:
admin; the recruiter set: recruiter; `jobs.read`, `ats.read`, `ats.write`, `ats.interview`: hiring
manager; `ats.interview` alone: interviewer). Requisition approval is decided by can-core's approvals
engine (type `requisition`, chain `admin`): asking needs a personal access token (the requester must be a
person), the decision arrives as a signed `approval.decided.v1` and is applied once. Events go to
can-core, including `candidate.hired.v1` for people. LinkedIn, Indeed and Glassdoor need partner
access: they are documented stubs that never touch the network.

The full guide, including roles, privacy, retention and the honest limits of job-board access, is
[`docs/recruiting.md`](../../docs/recruiting.md). The wire contract is
[`specs/013-jobs-recruiting/contracts/openapi.yaml`](../../specs/013-jobs-recruiting/contracts/openapi.yaml);
the stable cross-module subset is `schemas/capabilities/jobs.v1.json` and `ats.v1.json`.

Not legal advice: candidate data is personal data and the operator is responsible for compliance.
