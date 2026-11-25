---

description: "Task list for Epic E00: Phase 0 measurements (CI-runnable part)"
---

# Tasks: Phase 0 Measurements (Epic E00, CI-runnable part)

`[x]` = done and verified offline or in CI; `[ ]` = blocked (human-gated) or deferred.

## Phase 1: Parser and CLI (US1)

- [x] T001 [US1] Tests with canned kubectl JSON, cgroup samples and `kubectl top` text in `core/footprint/footprint_test.go`
- [x] T002 [US1] Implement `core/footprint` (ParsePods, ParseRaw, PodUID, Build, ParseTop, FromTop, Check)
- [x] T003 [US1] CLI tests in `core/cli/footprint_test.go` (report, check within/over/empty selection, `--module`, usage errors)
- [x] T004 [US1] `can footprint report|check` in `core/cli/footprint.go`; register in `root.go`

## Phase 2: Scripts and workflow (US2)

- [x] T005 [US2] `dev/footprint/sample.sh`, `measure.sh`, `summary.sh` (shellcheck clean), three k3d variants, can-static and forms fixture manifests
- [x] T006 [US2] `.github/workflows/footprint.yml` (non-required, SHA-pinned, `contents: read`)
- [x] T007 [US2] Iterate on CI until the jobs really measure; record numbers in `docs/measurements/phase0-ci.md`

## Phase 3: Proofs (US3)

- [x] T008 [US3] `test/integration/proofs_test.go` (template admission, egress denial with control, DNS from a module namespace, module isolation, module API vs admin port, TokenReview, can-core RBAC scope)
- [x] T009 [US3] Run in CI; failures recorded as findings and filed as issues

## Phase 4: Numbers and docs (US4)

- [x] T010 [US4] `docs/measurements/phase0-ci.md` with date, runner spec, run URLs, measured vs estimated, caveats
- [x] T011 [US4] SPEC.md §3.2 updated only where measured; epic index marks E00 partial
- [ ] T012 Idle RAM on a real 2 GB and 4 GB VPS (blocked: provider account)
- [ ] T013 `make up` timed end to end (blocked: provider account and token)
- [ ] T014 Licence and footprint checks of the remaining wrapped candidates (deferred)
