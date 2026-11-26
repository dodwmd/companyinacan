# Feature Specification: Starter Modules, Support, Newsletter, Analytics (Epic E07)

**Feature Branch**: `epic/E07-starter-modules` (spec dir `007-starter-modules`)

**Created**: 2026-11-25

**Status**: Built offline (manifests, charts, hooks, contracts, email policy); adapters pending. Images, databases,
admin bootstrap and cluster checks were delivered by [specs/041](../041-wrapped-modules-live/spec.md), which
supersedes the Secret names, database engine of `support`, status markers and footprint figures below.

**Input**: Epic E07 in `docs/epics/README.md`: "Module manifests and charts for FreeScout,
Listmonk and GoatCounter (or Umami), with resource declarations, probes, backup/restore hooks and
licence entries; the email policy (relay credentials injection, inbound IMAP/parse adapter);
per-module footprint checks."

Everything is built and verified offline: no cluster, no container runtime, no network. What that
leaves unverified is listed in [research.md](research.md) and `tasks.md` (Phase 7).

## User Scenarios & Testing *(mandatory)*

### User Story 1 - The three starter modules validate and are licence-clean (Priority: P1)

A maintainer runs `make test`. The manifests of `support`, `newsletter` and `analytics` validate
under `module.v1`, their upstream licences are on the accepted list, and `THIRD_PARTY_LICENSES`
lists them.

**Why this priority**: Constitution VI (licence hygiene) and IV (stable contracts) gate everything
else; a module that cannot pass `can validate` cannot be enabled.

**Independent Test**: `go test ./test/contract/...` and `can licences --check`.

**Acceptance Scenarios**:

1. **Given** each `modules/<id>/module.yaml`, **When** validated, **Then** there are no errors and
   no warnings (all capabilities are known names).
2. **Given** FreeScout (AGPL-3.0) and Listmonk (AGPL-3.0) and GoatCounter (EUPL-1.2), **When**
   licences are collected, **Then** all three are accepted and appear in `THIRD_PARTY_LICENSES`.
3. **Given** a module README, **Then** it states the footprint (marked as estimate) and the
   network-copyleft implication.

---

### User Story 2 - Each chart deploys safely into a module namespace (Priority: P1)

An operator enables a starter module. Its chart renders objects that the module namespace
(`restricted` pod security, default-deny network policy, quota) admits and that fit the footprint
the manifest declared.

**Independent Test**: Render each chart with `helm template` (skipped where helm is absent) and
assert the invariants; `values.yaml` invariants are asserted without helm.

**Acceptance Scenarios**:

1. **Given** a render with `ingress.host` set, **Then** it holds a Deployment, a ClusterIP
   Service on the manifest's port, a Traefik IngressRoute for the subdomain host, and a PVC, all in
   the release namespace and nothing cluster-scoped.
2. **Given** any container, **Then** it is non-root, drops all capabilities, forbids privilege
   escalation, has a seccomp profile, cpu and memory requests and a memory limit, and
   readiness and liveness probes; no hostPath or host namespaces.
3. **Given** requests and limits summed over the pod containers (adapter included), **Then** they
   stay within the manifest's `ram_mb` and `ram_limit_mb`.
4. **Given** no `ingress.host`, **Then** the render fails (can-core must supply it).
5. **Given** the adapter enabled, **Then** a same-namespace NetworkPolicy (both directions) is
   added and nothing else loosens the base policy; the public `/inbound` route exists for support.
6. **Given** a placeholder image digest, **Then** `scripts/check-image-pins.sh` (and so
   `make release-check`) fails, while development renders still work.

---

### User Story 3 - Contracts that other modules can code against (Priority: P1)

A module author reads `schemas/capabilities/*` and `schemas/events/*` and builds a consumer (the
CRM reads `tickets`, the site renderer reads `analytics`, the forms flow subscribes to
`newsletter`) without looking at FreeScout, Listmonk or GoatCounter.

**Independent Test**: Contract tests compile each schema and run valid and invalid fixtures.

**Acceptance Scenarios**:

1. **Given** `tickets.v1`, `newsletter.v1`, `analytics.v1`, **Then** each lists its verbs with
   method, path and permission, flags every write verb, and has valid and invalid fixtures per
   request and response definition.
2. **Given** `ticket.created.v1` and `subscriber.added.v1`, **Then** valid fixtures pass and
   malformed ones (wrong type or source, unknown keys, bad ids, empty lists) are rejected.
3. **Given** a `newsletter.subscribe` request without consent, **Then** it is invalid.
4. **Given** the module-local copies of the event schemas, **Then** they are byte-identical to
   `schemas/events/`.

---

### User Story 4 - Backup and restore hooks produce and consume consistent dumps (Priority: P2)

can-core (E03/E08) runs `hooks/backup.sh` nightly and `hooks/restore.sh` during recovery.

**Independent Test**: Scripts parse (`sh -n`), are POSIX with `set -eu`, and move the dump through
the can-core dump endpoint with the projected token. Execution against real databases is not
possible here.

**Acceptance Scenarios**:

1. **Given** a MariaDB or Postgres module, **Then** the dump is a single-transaction snapshot,
   written to a file, checked for plausibility and integrity, then uploaded.
2. **Given** the SQLite module, **Then** the snapshot uses SQLite's online backup, is integrity
   checked, and restore replaces the file only after the check.
3. **Given** a failed dump or a truncated file, **Then** the hook exits non-zero and uploads nothing.

---

### User Story 5 - The email policy is written down and the charts follow it (Priority: P2)

`docs/email-policy.md` states how relay credentials reach apps that speak SMTP themselves, how
inbound mail enters support (IMAP or an inbound-parse webhook on the adapter), and what stays
manual.

**Acceptance Scenarios**:

1. **Given** the support and newsletter charts, **Then** SMTP credentials are optional
   `secretKeyRef`s into the `can-smtp` Secret (the pod starts without a relay).
2. **Given** the doc, **Then** it covers port 587, SPF/DKIM/DMARC, the bulk relay for marketing
   mail, IMAP, inbound-parse, signature verification and idempotency.

### Edge Cases

- Disabling a module keeps its PVC (`helm.sh/resource-policy: keep`); purge is a separate step.
- SQLite modules must never run two pods on the volume (`Recreate`, one replica).
- FreeScout's image may refuse to run non-root (open item): the failure mode is an admission
  rejection, not silent root.
- No relay configured: support and newsletter start and keep their data; mail stays unsent.
- A form submission whose newsletter box was not ticked must not subscribe anyone.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: `modules/support`, `modules/newsletter`, `modules/analytics` MUST each contain
  `module.yaml` (valid under `schemas/module.v1.schema.json`), `chart/` (Chart.yaml, values.yaml,
  templates), `hooks/backup.sh`, `hooks/restore.sh`, `settings.schema.json` and a `README.md`.
- **FR-002**: Each manifest MUST declare provider, SPDX licence and class, upstream and upstream
  version, runtime subdomain and UI service, requires (database, capabilities), resources
  (`ram_mb`, `ram_limit_mb`, `disk_mb`), provides, permissions and roles, data classes with
  retention, hooks including `image`, and a health path.
- **FR-003**: `schemas/capabilities/{tickets,newsletter,analytics}.v1.json` MUST define the verbs
  (flagging writes) and request/response shapes; `schemas/events/{ticket.created,subscriber.added}.v1.json`
  MUST define the events; each has fixtures under `testdata/`.
- **FR-004**: `schemas/names.yaml` MUST know `newsletter` and `analytics`; `schemas/accepted-licences.yaml`
  MUST accept `EUPL-1.2` (AGPL was already there).
- **FR-005**: Charts MUST satisfy the invariants of User Story 2 and use the Secret and key
  contract in [contracts/module-charts.md](contracts/module-charts.md).
- **FR-006**: Hooks MUST follow [contracts/hooks.md](contracts/hooks.md).
- **FR-007**: `docs/email-policy.md` MUST describe relay credential injection and the inbound
  adapter design; `modules/README.md` and each module README MUST state the network-copyleft
  implication.
- **FR-008**: Image references MUST be pinned by digest at release; unresolved ones are the
  documented `REPLACE_WITH_` placeholders and `scripts/check-image-pins.sh` MUST catch them in
  module charts as well as Dockerfiles.
- **FR-009**: Resource figures that are estimates MUST be labelled as such wherever they appear;
  no measurement may be invented.
- **FR-011**: A manifest may mark a `provides` or `subscribes` entry `status: planned` (adapter
  not deployed); planned entries MUST be ignored by provider resolution, `primary` validation,
  the registry, event routing and module introspection, and draw warning
  `semantic.provides-adapter-disabled`. A module may carry `status: experimental` or
  `not-deployable` and then draws `semantic.module-status`. The three starter modules use these
  markers; a contract test keeps `planned` in step with each chart's `adapter.enabled`.
- **FR-010**: `THIRD_PARTY_LICENSES` MUST be regenerated and committed (`can licences --write`).

### Key Entities

See [data-model.md](data-model.md): Module manifest, Chart values contract, Module Secrets,
Capability contract, Event envelope, Dump.

## Success Criteria *(mandatory)*

- **SC-001**: `make test` (lint, unit, contract, licence check) passes with the new modules and
  fixtures.
- **SC-002**: With helm installed, every starter chart renders with and without the adapter and
  passes the invariants; without helm the structural `values.yaml` checks still run.
- **SC-003**: Idle RAM requests of the three modules are 250, 50 and 20 MB (estimates), 320 MB
  together, against the 1 GB-class starter server budget of SPEC.md section 3.4.
- **SC-004**: No new Go dependency (`go.mod` unchanged).

## Assumptions

- The module deployment epic (E03) will deliver can-core's namespace, database and Secret
  creation, hook Jobs and the dump endpoint; these modules target that contract, written down in
  `contracts/`.
- The in-house adapters that serve the capabilities and publish the events are a follow-up (see
  tasks T030 to T033); manifests declare the contract, charts ship the adapter slot disabled.
