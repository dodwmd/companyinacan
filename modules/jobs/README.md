# jobs

In-house recruiting module: job requisitions and postings, public careers pages with schema.org
`JobPosting` data, job feeds, application intake, an ATS pipeline with scorecards, a talent pool,
consent, retention and erasure. Library `core/jobs`, binary `cmd/can-jobs`, image `deploy/jobs`.
It provides the `jobs` and `ats` capabilities and publishes `job.opened.v1`,
`application.received.v1`, `application.stage_changed.v1` and `candidate.hired.v1` (the hire
hand-off the people module consumes).

**Status: experimental.** Built and tested offline and measured locally (16 MiB idle). The image has
not been built or run on a cluster, there are no Helm templates yet, there is no recruiter UI, and no
real virus scanner or job-board connector ships. LinkedIn, Indeed and Glassdoor need partner
access: they are documented stubs that never touch the network.

The full guide, including roles, privacy, retention and the honest limits of job-board access, is
[`docs/recruiting.md`](../../docs/recruiting.md). The wire contract is
[`specs/013-jobs-recruiting/contracts/openapi.yaml`](../../specs/013-jobs-recruiting/contracts/openapi.yaml);
the stable cross-module subset is `schemas/capabilities/jobs.v1.json` and `ats.v1.json`.

Not legal advice: candidate data is personal data and the operator is responsible for compliance.
