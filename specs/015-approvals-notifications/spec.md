# Feature Specification: Approvals, notifications and RBAC, first slice (Epic E11a)

**Feature Branch**: `epic/E11a-approvals-notifications` | **Created**: 2026-11-25 | **Status**: Implemented offline (no cluster)

**Source**: SPEC.md §3.7 (shared platform services: RBAC, approval workflows, notifications and
tasks), §4.1 (manifest `approvals`, `permissions`, `roles`), §4.3 (events), `docs/epics/README.md` E11.

## Scope of this slice

Inside can-core: (1) an approval engine, (2) notifications, (3) role-based access control. Not in this
slice: SSO groups (E11 SSO part), the retention engine, search push, the Android push channel, a real
`email.send` provider (the interface and a fake exist), `directory` reporting lines beyond the
capability call (people module E13a provides `directory`).

## User scenarios

### US1: A module asks for approval and learns the outcome (P1)

A module (expenses, vendors, time) creates an approval request through the module API with an
idempotency key. The company's policy decides who approves. When the request ends the module receives
a signed `approval.decided.v1` webhook, and can poll `GET /v1/approvals/{id}`.

1. Same key, same body: the original request, 200. Same key, different body: 409.
2. A module can only use the approval types declared in its manifest `approvals`.
3. If no step can be staffed (no policy and no default chain, role with no holder, requester has no
   manager and no fallback) the request is refused (422): fail closed.
4. Only the owning module receives the events of its requests.

### US2: Approvers decide in the admin UI (P1)

An approver sees "Waiting on me", opens a request, reads the details and the decision history, and
approves or rejects with a comment. The requester sees their own requests and can withdraw them.

1. A requester can never approve or reject their own request, directly or through a delegation.
2. One person counts once per request, so one person cannot approve two steps.
3. People who may not see a request get 404 (no existence oracle). Reading a request is audited.
4. Every decision, comment, cancellation, expiry, escalation and delegation is audited in the same
   transaction as the change.

### US3: Chains, thresholds, parallel steps, quorum (P1)

Policies live in `company.yaml` (`approvals:`), applied by `can apply`. A step names approvers (a role,
a named subject, or the requester's manager), may require a quorum, may run parallel with the previous
step, may apply only above or below an amount, and may have a fallback approver set.

### US4: Delegation, expiry, escalation (P2)

An approver can delegate while out of office (at most 90 days, at most 10 active). A policy may
escalate once after a time (adding approvers) and expire after a time (outcome `expired`).

### US5: Notifications (P1)

Each person has an in-app inbox with an unread count (nav badge) and mark-read. Approval events
notify approvers, escalation targets and requesters. Module manifests may declare rules
(`notifications:`) mapping events such as `lead.created.v1` to role, user, event-field or webhook
recipients. Email goes through the `email.send` interface with escaped templates, quiet hours, digests,
per-type per-channel preferences and an hourly rate limit; a Slack-style webhook channel adapter exists
with a fake. Every delivery is logged; duplicates (same recipient and dedupe key) are dropped.
`notification.sent.v1` carries identifiers only.

### US6: Roles and permissions (P1)

Modules declare `permissions` and default `roles`; the operator assigns roles to admin users and can
override a role's grants. `can.check(subject, permission, resource)` is available to modules. New admin
users hold no role (least privilege); the first admin is the owner (break-glass), and the last owner
cannot be removed. The admin UI and API enforce permissions on the existing operational pages.

## Requirements

- **FR-001** Approval types come from manifests; policies come from company.yaml; both validated
  (schemas and `approvals.ValidatePolicy`).
- **FR-002** Reporting-line resolution is an interface (`ReportingLine`) backed by the `directory`
  capability with a fake; a directory outage is an error, never "no manager".
- **FR-003** Separation of duties is not configurable.
- **FR-004** Events and notifications carry identifiers only (no titles, payloads, names or emails).
- **FR-005** Work per request and per tick is bounded (list limits, tick batch of 100, 50 approvers
  notified per step, 50 recipients per rule, 200 history rows).
- **FR-006** Valid credentials are never throttled. The only rate limit is on outbound email and
  webhook messages per recipient.
- **FR-007** Existing login semantics and throttling tests stay green.

## Success criteria

- `make test` and `go test -race ./core/server/...` pass; the OpenAPI contract test covers every new
  route; event schemas have fixtures and the events really emitted validate against them.
- Idle can-core RSS grows by about 2 MiB (measured, see `docs/approvals.md`).

## Known gaps

No real `email.send` provider (a failing `NoMailer` is wired: deliveries show as failed in the log);
approval notifications are synchronous with the request; SSO group claims for roles are not emitted;
RBAC cache is per process (CLI role changes apply within 30 s); scoped roles are matched against the
`resource` argument of `can.check` only, not against approval `ref`; no UI for role overrides (API
only); a module-facing "who may approve this" query is not provided.
