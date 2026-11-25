---
description: "Task list for Epic E07: starter modules (support, newsletter, analytics)"
---

# Tasks: Starter Modules (Epic E07)

**Input**: `specs/007-starter-modules/` (plan, spec, research, data-model, contracts)

**Tests**: Required (Constitution III). The test task of each group is written and observed
failing before its implementation.

## Phase 1: Tests first

- [x] T001 `test/contract/starter_modules_test.go`: manifests, licences, charts (values and helm render), hooks, schemas, fixtures, docs, pin script; observed red (schema packages and modules missing)
- [x] T002 Fixtures: `testdata/events/{ticket.created.v1,subscriber.added.v1}`, `testdata/capabilities/{tickets,newsletter,analytics}.v1`

## Phase 2: Contracts and licence plumbing (US3, US1)

- [x] T010 [P] `schemas/capabilities/*.v1.json` and `embed.go`
- [x] T011 [P] `schemas/events/{ticket.created,subscriber.added}.v1.json` and embed entries
- [x] T012 `schemas/names.yaml` (newsletter, analytics), `schemas/accepted-licences.yaml` (EUPL-1.2)

## Phase 3: Modules (US1, US2, US4)

- [x] T013 [P] `modules/support`: manifest, chart, hooks, settings schema, README
- [x] T014 [P] `modules/newsletter`: same, plus the migrate init container
- [x] T015 [P] `modules/analytics`: same (SQLite)
- [x] T016 `deploy/hooks` image fragment and README
- [x] T017 `scripts/check-image-pins.sh` scans module charts; `Makefile` label

## Phase 4: Docs (US5)

- [x] T020 `docs/email-policy.md`
- [x] T021 `modules/README.md` licence and network-copyleft section
- [x] T022 `can licences --write`, commit `THIRD_PARTY_LICENSES`

## Phase 5: Close-out

- [x] T023 `make test` green; rebase on origin/main; PR; CI

## Phase 6: Deferred to follow-up work (declared, not built)

- [ ] T030 `can-adapter-support`: serve `tickets.v1` over the FreeScout API, publish `ticket.created.v1`, signed `/inbound` webhook (Postmark/Mailgun/SES), IMAP mode configuration
- [ ] T031 `can-adapter-newsletter`: serve `newsletter.v1` over the Listmonk API, publish `subscriber.added.v1`, `/hooks/lead` subscriber, apply the relay settings
- [ ] T032 `can-adapter-analytics`: serve `analytics.v1`, create the GoatCounter site; FreeScout wrapper image if the community image cannot run non-root
- [ ] T033 Site renderer injects the analytics snippet when `analytics` is enabled (E04 follow-up)

## Phase 7: Verification that needs a cluster, a container runtime or network

- [ ] T040 Measure idle RAM of each module on k3s (10 minutes settled, `kubectl top`), record in `measurements.md`, correct the manifests if over 1.2x
- [ ] T041 Resolve tags and digests (`tiredofit/freescout`, `listmonk/listmonk`, `arp242/goatcounter`, `alpine`); verify non-root operation, flags, env names and probe paths (research R3, R4)
- [ ] T042 Build `deploy/hooks` (check package names); run each hook against a real database; round-trip backup then restore
- [ ] T043 Real mail: relay credentials in `can-smtp`, SPF/DKIM/DMARC, IMAP fetch and inbound-parse delivery
- [ ] T044 Reconcile the dump endpoint with E08 and the Secret/ConfigMap/Job conventions with E03

## Dependencies

T001, T002 before everything. T010 and T011 before T013 to T015 (manifests reference them).
T022 after T012 and T013 to T015.
