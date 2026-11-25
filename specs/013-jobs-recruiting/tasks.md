# Tasks: Jobs and Recruiting, first slice (E16a)

Order is the order the work was done. `[x]` = done and tested offline.

## Phase 1: pure units (red, then green)

- [x] T001 Tests for feeds, JSON-LD, sitemap (golden files), resume sniffing, filename safety, resume store, scanners, syndicators; observed failing (undefined symbols), then implemented `feeds.go`, `resume.go`, `syndicate.go`. Golden files reviewed by hand.

## Phase 2: store and API (tests first against the designed API)

- [x] T002 Harness and tests for authentication, throttling, token issuance, role matrix, IDOR table, query-scope and injection cases, job lifecycle, careers pages, intake and spam controls, duplicates, pipeline, stages, scorecard visibility, interviewer masking, events without personal data, audit and fail-closed reads, talent pool, diversity separation, sealing, key rotation, erasure and WAL scrub, ledger replay, retention timers. These did not compile until the implementation existed, so "red" here means a missing package surface, not assertion failures; assertion failures appeared and were fixed while implementing (three test mistakes and two real bugs: stub board status overwritten on close, test-data coincidences).
- [x] T003 Implement `model.go`, `scope.go`, `auth.go`, `tokens.go`, `store.go`, `jobs.go`, `candidates.go`, `pipeline.go`, `scorecards.go`, `retention.go`, `eeo.go`, `events.go`, `api.go`, `public.go`, `app.go`, `cmd.go`, migrations.
- [x] T004 Mutation checks: removing the token job-scope clause fails 3 tests; disabling blind feedback fails `TestScorecardVisibilityRules`.

## Phase 3: contracts and packaging

- [x] T005 Capability schemas `jobs.v1`, `ats.v1`; four event schemas with valid and invalid fixtures; names and event catalogue entries.
- [x] T006 OpenAPI 3.1 contract; contract tests compare it with the router (`api.go`, `public.go`, `app.go`), resolve refs, check capability verbs.
- [x] T007 `modules/jobs` (manifest `inhouse`, `experimental`), settings schema, chart values, `deploy/jobs/Dockerfile` fragment; contract tests for manifest, chart, Dockerfile.
- [x] T008 Live-event contract test (real service, webhook receiver, signature check) and ledger compatibility test (`core/backup`).
- [x] T009 Idle RAM measured locally (research R8).
- [x] T010 `docs/recruiting.md` with the honest limits of board access and the legal disclaimer.

## Known gaps (not done in this slice)

- [ ] Recruiter web UI; interview scheduling and offer letters.
- [ ] Real virus scanner (the hook and `scanner_required` exist); object-store `ResumeStore`.
- [ ] Real board connectors and inbound applications (partner access required); email-to-apply; paid multiposting bridges.
- [ ] Applicant email verification; reopening a rejected application; bulk operations.
- [ ] can-core wiring: approvals capability for the `Approver`, token introspection, keyring Secret, event subscription by people.
- [ ] Helm templates; image build in CI; run on a cluster; k3d integration tests.
- [ ] Theming the careers pages from `company.yaml`.
- [ ] Indeed-format values (`remotetype` etc.) confirmed against Indeed's current partner documentation.
