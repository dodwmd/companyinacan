# Tasks: People Directory and Performance, first slice (E13a)

Order in which the work was actually done. `[x]` done and verified offline; `[ ]` open.
Test-first status is stated per phase rather than assumed.

## Phase 1: Spec artifacts
- [x] spec.md, plan.md (constitution check), research.md, data-model.md, contracts/people-api.yaml, measurements.md

## Phase 2: Crypto (tests written first, observed failing: undefined symbols)
- [x] Keyring parse/load (0400/0600 only), Seal/Open with AAD binding, rotation, NeedsRotation

## Phase 3: Policy (tests first; exhaustive invariants plus named scenarios)
- [x] `Decide` and `FieldAccess`: roles x person statuses x relationships; IDOR, privilege escalation,
      manager-of-manager, terminated and on-leave manager, service accounts

## Phase 4: Service (implemented, then tests; the first run found two real defects: initial status history was
dated at the start date, blocking an immediate onboarding transition; one wrong expectation in a test)
- [x] Directory, org units, manager cycle prevention, lifecycle with effective dating and `ApplyDue`
- [x] Sensitive fields: sealed, audited reads (fail closed), all-or-nothing writes
- [x] Checklists (templates, auto-instantiation, completion events)
- [x] 1:1s (series, meetings, agenda, shared and private notes, actions, follow-ups, access follows the live reporting line)
- [x] Goals (visibility, alignment with cycle check, key results, progress)
- [x] Retention sweep, legal hold, erasure with tombstones, purge hook, key rotation
- [x] Audit hash chain with verification; outbox with signed, retrying dispatcher

## Phase 5: API, config, CLI
- [x] REST handlers, strict decoding, size limit, security headers, `Routes()` for the contract test
- [x] Static-token and introspection authenticators (fail closed, short positive cache)
- [x] Config (strict, validated), secrets from env, `can-people` commands: serve, check-config, healthcheck,
      rotate-keys, retention, verify-audit, purge, version

## Phase 6: Contracts
- [x] `schemas/events/*` (6) with valid and invalid fixtures; `schemas/capabilities/{directory,performance}.v1.json` with fixtures
- [x] OpenAPI 3.1 document; contract test comparing it with the registered routes
- [x] Contract test: every event emitted by an end-to-end flow validates against its schema
- [x] `modules/people/module.yaml` (in-house, sensitive classes with retention, `experimental`), settings schema,
      chart values, events copies; sensitive-requires-inhouse negative test; manifest and code data classes agree
- [x] `deploy/people/Dockerfile` fragment and README

## Phase 7: Docs and measurement
- [x] `docs/people.md`, `modules/people/README.md`, SPEC.md event-catalogue note, `modules/README.md` row, epics status
- [x] Idle RAM measured locally (13.8 MiB idle, 22.3 MiB after load)

## Phase 8: Open (needs a cluster or other epics)
- [ ] Helm templates; render tests; k3d integration tests (NetworkPolicy, restricted PSA, PVC ownership)
- [ ] CI footprint check on k3d; image build, digest pins
- [ ] can-core: keyring Secret generation and backup, user-token introspection, event webhook route, ledger append from `GET /v1/erasures`, replay of `purge` on restore (E08/E11)
- [ ] Approval chains from reporting lines; `identity.provision` consumer of `checklist.task_completed.v1`; `work` issue creation from action items
- [ ] Reviews/360, feedback, growth plans, time off, admin and portal UI, search/report exclusions test
- [ ] SQL-side filtering for large directories; per-field audit of writes before commit
