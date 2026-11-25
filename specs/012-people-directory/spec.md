# Feature Specification: People Directory and Performance, first slice (Epic E13a)

**Feature Branch**: `epic/E13a-people` (spec dir `012-people-directory`)

**Created**: 2026-11-25

**Status**: Implemented offline; cluster verification pending

**Input**: Epics E13 (`directory`/`people`) and E14 (`performance`: 1:1s and goals) in
`docs/epics/README.md`, and SPEC.md sections 3.7 (RBAC, retention and legal hold, audit, erasure
semantics), 4.3 (`directory` and `performance` capabilities, event catalogue), 5 (sensitive data) and
the *Managing people and work* design. This is the first, deliberately small slice of the people
domain: the directory, 1:1s, goals, onboarding and offboarding checklists, and the retention and
erasure machinery that must exist before personal data ships (Constitution V).

> **This is a tool, not legal, HR or payroll advice.** It stores and protects people data; it does not
> compute pay, tax or leave entitlements and does not know any jurisdiction's employment or privacy
> law. Retention defaults are starting points. The operator is responsible for compliance. Payroll is
> integrated, never built (SPEC.md section 6).

Everything is built and verified offline: no cluster, no container runtime. What that leaves
unverified is listed in [research.md](research.md) and `tasks.md` (Phase 8).

## User Scenarios & Testing *(mandatory)*

### User Story 1 - HR keeps a directory with sensitive fields protected per field (Priority: P1)

HR creates people (employees and contractors), assigns departments, teams, positions and managers, and
records sensitive details. Colleagues see basic fields; only the person and HR/admin see sensitive ones,
and every sensitive read is recorded.

**Why this priority**: Constitution V and SPEC.md section 5: nothing else in the people domain may ship
before per-field access and audited reads exist.

**Independent Test**: `go test ./core/people/...` (policy table tests, service tests, HTTP tests).

**Acceptance Scenarios**:

1. **Given** a person with a salary, **When** their direct manager, a skip-level manager or a peer
   requests it, **Then** the answer is 403 and a denied audit event is recorded; **When** the person,
   HR or admin requests it, **Then** it is returned and a read audit event listing the field names (no
   values) is recorded before the data leaves the service.
2. **Given** the audit hook is failing, **When** anyone requests a sensitive field, **Then** the read fails
   and no data is returned.
3. **Given** the database file, **Then** sensitive columns hold only ciphertext, and ciphertext copied
   to another person or field does not decrypt.
4. **Given** a caller with no recognised role, a former or candidate person record, or a person on leave
   or offboarding asking for elevated actions, **Then** they are denied; self-service remains for the
   latter.
5. **Given** a person the caller may not see (candidate, former, erased, nonexistent, malformed id),
   **Then** the answer is 404, indistinguishable between those cases.

---

### User Story 2 - The org structure and employment lifecycle are consistent (Priority: P1)

HR moves people through `candidate -> onboarding -> active <-> leave -> offboarding -> former`, with
future-dated changes, and keeps a manager chain without cycles.

**Acceptance Scenarios**:

1. **Given** a manager assignment that would make a person their own ancestor (self, direct, transitive),
   **Then** it is rejected; a manager must be active or on leave.
2. **Given** an invalid transition, a second pending change, or a date before the last applied change,
   **Then** it is rejected; **Given** a future-dated change, **Then** it applies when its date arrives.
3. **Given** a person becomes former, **Then** their reports lose the reporting line, they lead no team,
   `person.status_changed.v1` and `person.offboarded.v1` are emitted, and their token gets no access.
4. **Given** a former person is a terminated manager, **Then** they cannot read their reports, 1:1s or
   goals even though the SSO token is still valid.

---

### User Story 3 - Managers and reports run private-where-needed 1:1s (Priority: P2)

A manager and a direct report keep a recurring 1:1 with a shared agenda, shared notes, action items and
history. The manager has private notes the report never sees.

**Acceptance Scenarios**:

1. **Given** a series, **Then** only the report and the report's *current* direct manager can read or
   write it; HR, admin, skip-level managers and peers get 404.
2. **Given** private notes, **Then** the report's view has no `private_notes` field at all, the report
   cannot write them, they are encrypted at rest, and HR cannot read them.
3. **Given** the report gets a new manager, **Then** the old manager loses access (private notes
   included), the new manager does not inherit the series, and the report keeps their history.
4. **Given** an open action item, **Then** it appears as a follow-up in the next meeting and can carry a
   `work_issue_id`.

---

### User Story 4 - Goals with key results and honest visibility (Priority: P2)

Employees and managers keep goals and key results aligned up the management chain, to a team, or to a
company goal, with a visibility level enforced server-side.

**Acceptance Scenarios**:

1. **Given** a `private` goal, **Then** only its owner can read it (not the manager, not HR).
2. **Given** `manager`, `team` and `company` visibility, **Then** the matching audience, and only it, can
   read; reads of other people's goals are audited.
3. **Given** an alignment to a goal outside the owner's management chain, team or the company, or one that
   would create a cycle, **Then** it is rejected.
4. **Given** progress on a key result, **Then** progress is computed, `goal.updated.v1` is emitted, and a
   skip-level manager can read but not edit.

---

### User Story 5 - Onboarding and offboarding from templates (Priority: P2)

HR defines checklist templates; a checklist is created for a person (manually, or automatically on entering
`onboarding` or `offboarding`) with tasks assigned to the person, their manager, or an HR/admin role, with
due dates relative to an anchor date.

**Acceptance Scenarios**:

1. **Given** an auto template, **When** a person enters its status, **Then** one checklist is created (never
   a duplicate) with resolved assignees and due dates.
2. **Given** a task, **Then** only its assignee, HR or admin can complete it (others get 404) and completion
   emits `checklist.task_completed.v1` once, carrying the task's `action` (for example
   `identity.provision`).

---

### User Story 6 - Retention, legal hold and erasure (Priority: P1)

Data is purged on a per-class schedule, a legal hold stops that, and an erasure request purges a former
person and records opaque tombstones that the backup erasure ledger (E08) can consume.

**Acceptance Scenarios**:

1. **Given** a retention rule and a former person past the period, **Then** the class is purged, unless the
   person is on legal hold (then it is skipped and counted); `legal` retention never purges.
2. **Given** an erasure request for an active person, **Then** 409; under a legal hold, **Then** 409;
   from an employee or manager, **Then** 403.
3. **Given** an erasure of a former person, **Then** all five classes are purged, the record is a tombstone,
   five `(subject, class, date)` rows exist with a subject and class matching the E08 ledger patterns and no
   PII, and a repeat request is idempotent.
4. **Given** a restore, **Then** `can-people purge -subject S -class C` re-applies an erasure idempotently.
5. **Given** the keyring has a new top key, **Then** `rotate-keys` re-seals all old values and the old key
   can then be removed.

---

### User Story 7 - A documented, contract-tested API and manifest (Priority: P1)

**Acceptance Scenarios**:

1. **Given** the service's registered routes, **Then** they equal the paths and methods in the OpenAPI 3.1
   document, and every route except `/healthz` requires a bearer token.
2. **Given** every event the service emits in an end-to-end flow, **Then** it validates against its schema in
   `schemas/events/`, and fixtures (valid and invalid) pass and fail as labelled.
3. **Given** `modules/people/module.yaml`, **Then** it validates (with the expected `experimental` warning),
   declares the sensitive classes with retention, resources, provided capabilities and published events, and
   the sensitive-requires-inhouse rule rejects the same manifest as a wrapped app.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The service MUST deny by default. One pure policy layer (`core/people/policy.go`) MUST make
  every authorisation decision from the caller's roles, own employment status, and relationship to the
  record; handlers and storage MUST NOT inspect roles.
- **FR-002**: Roles are `employee`, `manager`, `hr`, `admin`. Elevated roles MUST require the caller's person
  to be active (service accounts without a person record may hold hr/admin, never manager).
- **FR-003**: Sensitive fields (`date_of_birth`, `national_id`, `home_address`, `personal_phone`, `salary`)
  MUST be encrypted per field with XChaCha20-Poly1305, bound to row and column, with a key id and rotation.
- **FR-004**: Reads of sensitive fields, of 1:1 meetings, and of other people's goals MUST be audited before
  data is returned, failing closed; writes and denied sensitive accesses MUST be audited. Audit entries MUST
  contain ids and field names only.
- **FR-005**: Employment status transitions MUST follow the lifecycle, support effective dating with one
  pending change per person, and keep a history.
- **FR-006**: The manager chain MUST reject self-management and cycles, and MUST terminate on corrupt data.
- **FR-007**: 1:1 content MUST be readable only by the report and the report's current direct manager;
  private notes only by that manager.
- **FR-008**: Goal visibility and alignment rules MUST be enforced server-side.
- **FR-009**: Checklists MUST be instantiated from templates, idempotently per person and template.
- **FR-010**: Retention MUST be per data class with `Nd|Nm|Ny|0|legal` syntax; a legal hold MUST block
  retention purges and erasure; erasure MUST tombstone with opaque subject ids compatible with the E08 ledger.
- **FR-011**: The service MUST publish `person.created.v1`, `person.status_changed.v1`, `person.offboarded.v1`,
  `oneonone.scheduled.v1`, `goal.updated.v1`, `checklist.task_completed.v1` through a transactional outbox;
  payloads MUST NOT contain names, contact data or free text.
- **FR-012**: The API MUST be REST JSON with an OpenAPI 3.1 contract, strict request decoding and size
  limits, and `no-store` responses.
- **FR-013**: Authentication MUST be behind an `Authenticator` interface; shipped implementations are static
  tokens and an introspection client; all errors MUST deny.
- **FR-014**: The manifest MUST declare the sensitive classes (in-house only), resources, capabilities and
  events; the module MUST be marked `experimental` until verified on a cluster.
- **FR-015**: The module MUST document its idle RAM, measured, and its gaps.

### Non-Functional Requirements

- **NFR-001**: Idle RSS at most the declared 30 MB; the limit is 64 MB (Constitution II).
- **NFR-002**: Stdlib plus dependencies already in `go.mod`; no new dependencies.
- **NFR-003**: All tests use fake data.

### Out of scope

Time off, payroll, reviews and 360 cycles, feedback and praise, growth plans, skip-level meeting
scheduling, mood check-ins, the admin and employee-portal UI, search indexing, reporting, import
(HiBob/BambooHR), `identity.provision` itself, approval-chain wiring, `work` issue creation from action
items, calendar invites, Helm templates, image publishing and cluster tests.

### Key Entities

See [data-model.md](data-model.md).

## Success Criteria *(mandatory)*

- **SC-001**: `make test` and `go test -race ./core/people/...` pass; the policy invariants are checked over
  every combination of role subset, person status and relationship.
- **SC-002**: The OpenAPI document and the registered routes are identical sets.
- **SC-003**: Every event emitted by an end-to-end flow validates against its published schema.
- **SC-004**: Measured idle RSS is recorded and at most the declared RAM.
