---
description: "Task list for Epic E10a: wiki, CRM and status page modules"
---

# Tasks: Wiki, CRM and Status Page (Epic E10a)

**Tests**: Required (Constitution III): the test tasks were written and observed failing first.

## Phase 1: Tests first

- [x] T001 `test/contract/starter_modules_test.go`: add wiki, crm, status to `starters`; capability fixture list; names list
- [x] T002 `test/contract/wrapped_modules_test.go`: wiki lightness, CRM wiring and erasure docs, distinctive permissions, status public-only routing, docs, spec artifacts, catalogue coverage
- [x] T003 `core/server/installer/hostile_test.go`: chart install list; `starter_charts_sync_test.go`: status route exemption
- [x] T004 Fixtures `testdata/capabilities/{wiki,crm,status}.v1`

## Phase 2: Contracts

- [x] T010 `schemas/capabilities/{wiki,crm,status}.v1.json`, `embed.go`, `schemas/names.yaml`

## Phase 3: Modules

- [x] T011 [P] `modules/wiki` (manifest, chart, hooks, settings schema, README)
- [x] T012 [P] `modules/crm` (same, plus daemon container)
- [x] T013 [P] `modules/status` (same, public-only ingress)

## Phase 4: Docs

- [x] T020 `docs/modules-catalogue.md`, `modules/README.md`, SPEC.md rows, epics README
- [x] T021 `can licences --write`, commit `THIRD_PARTY_LICENSES`
- [x] T022 spec artifacts and the lead-to-CRM adapter design

## Phase 5: Close-out

- [x] T023 `make test` and race green; rebase on origin/main; PR; CI

## Phase 6: Deferred (declared, not built)

- [ ] T030 `can-adapter-wiki`: serve `wiki.v1` over DokuWiki's XML-RPC/JSON-RPC API, create the first admin account
- [ ] T031 `can-adapter-crm`: serve `crm.v1`, implement `/hooks/lead` per contracts/lead-to-crm-adapter.md; non-root EspoCRM wrapper image (until then the module stays `not-deployable`)
- [ ] T032 `can-adapter-status`: serve `status.v1` over Uptime Kuma's API (socket.io or push endpoints), create the first admin
- [ ] T033 `purge` hooks (erasure tombstones) for crm and wiki; retention expiry jobs
- [ ] T034 Simple tasks (Vikunja) module, the remaining part of E10
- [ ] T035 `contacts` (directory party) and `content` capabilities from crm and wiki

## Phase 7: Needs a cluster, a container runtime or network

- [ ] T040 Measure idle RAM of each module on k3s (10 minutes settled, `kubectl top`); correct manifests if over 1.2x
- [ ] T041 Pull each image, resolve tags and digests, confirm non-root start under `restricted` (wiki, status), find or build a working crm image
- [ ] T042 Verify Uptime Kuma's public paths (`ingress.publicPaths`), data file name and probe; run backup and restore hooks against real data
- [ ] T043 Confirm DokuWiki licence "only" vs "or later" and EspoCRM attribution terms from the upstream LICENSE files
