# Feature Specification: Work Tracker, first slice (Epic E14a)

**Feature Branch**: `011-work-tracker` (git branch `epic/E14a-work-tracker`)

**Created**: 2026-11-25

**Status**: Implemented offline (service, API, CLI, contracts, tests); web UI, image build and cluster checks pending

**Input**: Epic E14, first slice: the in-house, API-first, Jira-like `work` tracker. P0 to P3
priorities, an API managers' automation can use to create and update stories, capabilities
`work` and `projects`, events, permissions. SPEC.md sections 3.3 (work tracking), 4.3
(capabilities, events, module contract), 6.1 (footprint: in-house `work` target about 40 MB;
Plane wrapper on 8 GB+ servers is a separate, later module).

## Scope of this slice

In: the `can-work` service (`cmd/can-work`, `core/work`), its REST API and OpenAPI contract,
API tokens and an `Authenticator` seam, webhook events, the `work.v1` and `projects.v1`
capability schemas, the module manifest, chart values and image fragment, the `can work` CLI,
user documentation, tests (unit, API, security, contract).

Out (recorded in tasks.md as gaps, not hidden): web UI and board UI (E12), the Plane wrapper,
`performance`, timesheets (`time`), file storage for attachments, SSO/RBAC integration (E11),
saved filters, components, source-control links, notification rules, MCP server, cluster
deployment and image build.

## User Scenarios & Testing

### User Story 1 - A manager's automation creates stories safely (Priority: P1)

A manager (or a script, or an assistant acting for them) turns a planning meeting into a list of
epics, stories and tasks and posts it. A network blip makes the script retry. Nothing is
duplicated, and each story knows its priority, owner, due date and parent.

**Why this priority**: SPEC.md section 3.3: the API "is required, not optional", with bulk
create and idempotency keys "so retries don't duplicate".

**Independent Test**: In-process handler tests: create with an `Idempotency-Key`, repeat it, repeat
it with a different body; bulk create with `$N` parent references, all-or-nothing on an invalid
item, retried with the same key.

**Acceptance Scenarios**:

1. **Given** a token with `work.write`, **When** it posts an issue with an idempotency key twice,
   **Then** the first call is `201`, the second `200` with `Idempotent-Replayed: true` and the same
   issue; the same key with a different body is `422 idempotency_mismatch`; keys are per caller.
2. **Given** a list of up to 100 items, **When** one item is invalid, **Then** nothing is created
   and the problem names `issues[N].field`.
3. **Given** a story's `parent` is `$0` in a bulk request, **Then** it is created under the epic
   at index 0 of the same request.
4. **Given** a body naming `reporter`, `status`, `key`, `version` or any unknown field, **Then**
   `400` (mass assignment is refused); the reporter is always the token's subject.

---

### User Story 2 - Work moves through a workflow with a clear priority (Priority: P1)

Everyone sees the same priority scale (P0 drop everything, P1 this sprint, P2 planned, P3
backlog) with response and resolution hints, and issues move through a per-project workflow
whose allowed transitions are enforced.

**Independent Test**: Store and API tests for the default and custom workflows, illegal moves,
resolved tracking, SLA overrides, reprioritising.

**Acceptance Scenarios**:

1. **Given** the default workflow, **Then** `todo` cannot jump to `done` (409 listing the allowed
   moves), and entering a `done`-category status sets `resolved_at`, leaving it clears it.
2. **Given** a project with a custom workflow, **Then** new issues start at its initial status.
3. **Given** a workflow change that drops a status in use, **Then** `422` names the missing statuses;
   changing a status's category re-derives `resolved_at`.
4. **Given** a project SLA override, **Then** issues show the hint for their priority and it follows
   a priority change.

---

### User Story 3 - People collaborate on an issue (Priority: P2)

Issues have a hierarchy (epic, story/task/bug, subtask), links (blocks, relates, duplicates),
comments, watchers, attachments (metadata) and an activity log of who changed what.

**Acceptance Scenarios**:

1. **Given** the hierarchy rules, **Then** a story under a story, a subtask under an epic, an epic
   with a parent, or a cross-project parent is `422`.
2. **Given** `A blocks B` and `B blocks C`, **Then** `C blocks A` and `B blocks A` are `409` (cycle),
   as are duplicates and repeats; `relates` is symmetric.
3. **Given** any change, **Then** the activity log shows the actor, the kind and, for updates, the
   before and after values; description text is never copied into events or activity.
4. **Given** an attachment registration, **Then** only metadata is stored; path-like names and
   non-http(s) URLs are refused; the URL is never fetched.

---

### User Story 4 - Plan sprints and see a board (Priority: P2)

A team plans a sprint, starts it, sees a board with one column per status in rank order, drags
issues (one fractional rank is written), and completes the sprint with unfinished work moving to
the next sprint or the backlog.

**Acceptance Scenarios**:

1. **Given** a project, **Then** at most one sprint is active; starting a second is `409`.
2. **Given** the board for the active sprint, **Then** columns follow the workflow order and issues
   follow rank.
3. **Given** a rank request (`before`, `after`, `first`, `last`), **Then** only the moved issue's row
   changes and the order is as asked, however many times it is repeated.
4. **Given** a sprint is completed, **Then** unresolved issues move to `move_to` or the backlog;
   resolved ones stay with the closed sprint; closed sprints take no issues.

---

### User Story 5 - Find work with a safe query language (Priority: P2)

`status:todo priority:P0,P1 assignee:me -label:wip due<2026-12-31 is:blocked "two words"`, with
paging and sorting, and bulk update of the results.

**Acceptance Scenarios**:

1. **Given** hostile input in any term, **Then** it is matched literally (it travels as a bound
   parameter; `%` and `_` are escaped) and the database is untouched.
2. **Given** an unknown field, a bad date, too many terms or values, **Then** `422` names the query.
3. **Given** `limit` and `cursor`, **Then** paging visits every issue once; `limit` is clamped to 100.
4. **Given** a bulk update, **Then** each key succeeds or fails on its own and the response says which.

---

### User Story 6 - Tokens, scopes and project isolation (Priority: P1)

An admin mints personal and service tokens with scopes and optional project limits; every call is
authenticated, authorised, rate limited and audited where it is an admin action.

**Independent Test**: API tests per scope and per project; IDOR tests over HTTP and the store.

**Acceptance Scenarios**:

1. **Given** `work.read` / `work.write` / `work.admin`, **Then** each surface needs its scope; admin
   implies write implies read.
2. **Given** a token limited to project `ACME`, **When** it addresses an issue, sprint, board, link
   target, bulk key or search in another project, **Then** the answer is exactly what a missing
   item gets (404 or an empty result), so existence cannot be probed.
3. **Given** a token, **Then** only its SHA-256 is stored, the secret is shown once, expiry and
   revocation apply immediately, `last_used_at` is recorded, personal tokens must expire.
4. **Given** a project-scoped admin, **Then** it cannot mint broader tokens, create projects, or list
   all tokens or the audit log.
5. **Given** repeated bad tokens from one address, **Then** that address is throttled (429), others
   are not; a noisy token is throttled per token.

---

### User Story 7 - Other modules and people react to changes (Priority: P2)

`work.issue.created.v1`, `work.issue.updated.v1`, `work.issue.transitioned.v1` and
`work.comment.created.v1` are delivered to a configured webhook, signed, at least once, surviving
restarts.

**Acceptance Scenarios**:

1. **Given** a change, **Then** an outbox row is written in the same transaction; no webhook means no
   rows.
2. **Given** a failing receiver, **Then** delivery is retried with backoff and never duplicated after
   success.
3. **Given** the schemas in `schemas/events/`, **Then** real events validate against them; fixtures
   cover valid and invalid shapes.

## Functional Requirements

- **FR-001**: Projects with keys (`^[A-Z][A-Z0-9]{1,9}$`), a workflow and SLA hints; archive makes a
  project read-only.
- **FR-002**: Issues with auto keys `KEY-N`, types epic/story/task/bug/subtask, priorities P0 to P3,
  assignee, reporter, labels, due date, estimate, parent, sprint, rank, version.
- **FR-003**: Configurable workflows with allowed transitions, categories, validation, and the
  default `todo/in_progress/in_review/done/cancelled`.
- **FR-004**: Hierarchy and link rules with cycle checks; comments, watchers, attachment metadata,
  per-issue activity log.
- **FR-005**: Sprints, board data and fractional-rank ordering.
- **FR-006**: Search language, paging, sorting, bulk update, bulk create.
- **FR-007**: Optimistic concurrency (ETag/`If-Match`, body `version`) and idempotency keys.
- **FR-008**: REST JSON API matching `contracts/openapi.yaml`, with request size and rate limits.
- **FR-009**: API tokens (hashed, scoped, project-scoped, expiring, revocable, last-used), an
  `Authenticator` interface, a bootstrap admin token.
- **FR-010**: Webhook events with schemas and fixtures; capability schemas `work.v1`, `projects.v1`.
- **FR-011**: Audit log of admin actions; retention purge per `data_classes`.
- **FR-012**: `modules/work/module.yaml` (in-house, valid, honest `experimental` status), chart values,
  image fragment with digest placeholders.
- **FR-013**: `can work` CLI (create, bulk-create, get, list, update, transition, comment) using an API
  token from the environment.
- **FR-014**: `docs/work-tracker.md` with curl examples; idle RAM measured and recorded.

## Edge Cases

Duplicate project key (409); archived project writes (409); a sprint in another project (422);
closing a sprint with a closed `move_to`; an issue whose parent is purged (parent becomes null);
a workflow that loses a status in use; a `$N` reference that points forward; a very long query;
empty bulk lists; clock moving backwards (timestamps are informational, versions order edits).

## Success Criteria

- `make test` and `go test -race ./...` pass; the contract tests tie OpenAPI, router, capability
  schemas, event schemas, fixtures, manifest and chart together so none can drift.
- The static binary idles below the declared 20 MB target on the development machine.
- No security finding in the tests listed under User Story 6 and the injection and
  mass-assignment tests.
