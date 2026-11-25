# Feature Specification: Wiki, CRM and Status Page Modules (Epic E10, first slice)

**Feature Branch**: `epic/E10a-wiki-crm-status` | **Created**: 2026-11-25 | **Status**: Built offline (wiki and status experimental, crm not-deployable); adapters and cluster verification pending
**Input**: SPEC.md section 4.3 (capabilities), section 5 (catalogue, footprints, licences),
docs/epics/README.md E10, specs/007-starter-modules (the pattern), constitution principles II and VI.

## Summary

Three wrapped modules in the pattern of the E07 starters: `wiki` (DokuWiki), `crm` (EspoCRM) and
`status` (Uptime Kuma), each with manifest, chart that passes the installer allowlist, backup and
restore hooks, settings schema, README and a capability contract. Adapters are not built; their
capabilities are marked `planned`. Nothing runs on a cluster here, so the modules are `experimental`
(wiki, status) or `not-deployable` (crm). Picks and numbers are in [research.md](research.md).

## User Scenarios

### US1 - A small company gets a lightweight wiki and public help centre (P1)
An operator enables `wiki` and writes the handbook; a `help` namespace may be made public.
**Independent test**: manifest validates; chart renders (helm when present) with a restricted pod, one
PVC, ClusterIP service, `Host(wiki.<domain>)` route; footprint sum under the manifest; backup and
restore hooks parse and move the archive through the can-core dump endpoint.
1. Given the wiki module, then it needs no database and requests less than the ~200 MB catalogue estimate.
2. Given `wiki.v1`, then list/get/search/put verbs have valid and invalid fixtures.

### US2 - Leads become CRM contacts (P1)
A visitor submits a form; `lead.created.v1` is published; the CRM adapter (later) upserts a contact.
**Independent test**: the manifest declares `consumes: [leads]` and an optional, `planned`
`lead.created.v1` subscription to `crm-adapter` `/hooks/lead`; the design contract defines mapping,
idempotency and failure handling; `crm.v1` fixtures cover `upsert_contact` by `external_key`.
1. Given the adapter is not built, then `can validate` warns and the registry ignores the subscription.
2. Given personal data in the CRM, then data classes are `standard`, retention is declared and the README documents erasure and its backup caveat.

### US3 - A public status page without exposing the admin (P1)
**Independent test**: with `ingress.publicOnly` (default) the IngressRoute rules are
`Host && Path/PathPrefix` for the public paths only, no rule for `/dashboard`, `/socket.io`, `/setup`;
with `publicOnly=false` a single `Host` rule.
1. Given the default, then the first-user setup race is not reachable from the internet.
2. Given every rule, then it passes the installer's single-`&&`-chain matcher check.

### US4 - The catalogue is honest (P2)
`docs/modules-catalogue.md` lists every module (core, in-house, wrapped) with status, RAM, licence and
what is verified; a test fails when a module is missing or a row contradicts its manifest.

## Requirements

- **FR-001** Each module has `module.yaml` (provider, SPDX licence, `licence_class: osi`, upstream, runtime, requires, provides with `status: planned`, permissions, roles, data classes, hooks, health check, `status` marker) that validates under `module.v1`.
- **FR-002** Charts create only Deployment, Service, IngressRoute, PersistentVolumeClaim and NetworkPolicy objects in the module namespace and install through the installer's fakes (`TestStarterModuleChartsStillInstall`).
- **FR-003** Restricted pod security, no hostPath/hostPort, ClusterIP, entry points web/websecure, secrets only `can-module`, `can-database`, `can-smtp`.
- **FR-004** Backup and restore hooks move dumps through the can-core dump endpoint with the token in a 0600 header file, never on argv.
- **FR-005** Capabilities `wiki`, `crm`, `status` in `schemas/names.yaml` with `*.v1.json` schemas, every write verb flagged, valid and invalid fixtures for every request and response shape.
- **FR-006** Permission names are distinctive (`wiki.*`, `crm.*`, `status.*`), not reserved, not used by another module; roles are module-namespaced and avoid built-in names.
- **FR-007** No sensitive data classes (third-party modules, constitution V); CRM personal data is `standard` with erasure documented.
- **FR-008** No invented image digests or tags: placeholders (`REPLACE_WITH_*`) wherever unverifiable; `make release-check` fails while they remain.
- **FR-009** READMEs state footprint (estimate, unmeasured), licence and network-copyleft implications, SSO state, an alternative, and what is unverified.
- **FR-010** `THIRD_PARTY_LICENSES` regenerated with `can licences --write`.

## Out of scope

Simple tasks (Vikunja) from the epic title; the adapters (T030 to T032) and `purge` hooks (T033); a
non-root wrapper image for EspoCRM; SSO; cluster verification and measurement (T040 to T042); the
`contacts` and `content` capabilities; `deal.won.v1`; hibernation.

## Success criteria

`go test ./...` and `go test -race ./...` pass; `can licences --check` passes; all three manifests
validate with only the expected status warnings; CI is green.
