# Feature Specification: Identity Provisioning (Epic E13d)

**Branch**: `epic/E13d-identity-provision` | **Date**: 2026-11-25 | **Status**: built offline, experimental

SPEC.md references: people lifecycle and onboarding/offboarding checklists (3.7), `identity.provision`
(4.3), SSO (E11b), the hire and leave flows (section 8). People directory, events and checklists:
`specs/012-people-directory/`. Module wiring, signed webhooks and personal access tokens:
`specs/016-module-wiring/`. Approvals: `specs/015-approvals-notifications/`. SSO users and
`user:sso-...` subjects: `specs/021-oidc-sso/`. Not legal, security or HR advice.

## Summary

A new in-house module `provisioning` provides `identity.provision`: a SCIM 2.0 server over the people
directory and joiner, mover and leaver workflows that create, update and remove accounts and group
memberships in downstream systems, safely: effective-dated, retried, audited, approved by a second
person for privileged access, and with a can-core kill switch for the tokens and sessions of people who
left.

## User scenarios

1. **An identity provider syncs users.** An administrator creates a SCIM token (`scim.read`); the IdP
   lists users with `filter=userName eq "..."`, pages through them, and reads groups. It never sees a
   date of birth, salary, national id or a person's leave status, and never a candidate.
2. **A new hire gets accounts.** HR hires someone; people publishes `person.created.v1`; the joiner
   creates accounts at each connector, adds groups, and creates the manager and IT checklist. A repeat
   of the event, or the later `onboarding` to `active` change, does nothing more.
3. **Someone changes team.** A SCIM group change (or an API call) runs a mover that adds and removes
   only the groups this module manages.
4. **Someone leaves.** On their last day, not before, every account is disabled, group memberships
   removed, and the can-core tokens and sessions of the person ended. If it is a termination flagged
   urgent, it happens at once. If the person is privileged, a second person approves first.
5. **Something fails.** A connector times out: the step retries with backoff and the same key. It keeps
   failing: it is dead-lettered, an event is published, and an administrator retries it. A joiner that
   fails half-way is compensated.

## Requirements

**Functional**

- FR-1 SCIM 2.0 subset (RFC 7643, 7644): `/Users`, `/Groups`, `/ServiceProviderConfig`, `/Schemas`,
  `/ResourceTypes`; filter `eq` and `and`; `startIndex`/`count`; PATCH `add`/`replace`/`remove`; ETag and
  `If-Match`; scoped bearer tokens (`scim.read`, `scim.write`); strict `application/scim+json`; RFC 7644
  error documents.
- FR-2 The SCIM server reads the directory through the `directory` capability and never exposes
  sensitive fields or the `leave` status.
- FR-3 Workflows `joiner`, `mover`, `leaver` of ordered idempotent steps against a `Connector`
  interface (create, update, disable, delete account; add and remove group membership).
- FR-4 Adapters: `scim-client`, `webhook`; `google-workspace`, `github`, `slack` as documented
  stubs with no network access.
- FR-5 Dry-run and plan mode; per-step retries with backoff and a dead letter; compensation; a state
  machine persisted in SQLite so a crash resumes.
- FR-6 Effective-dated leaver (disable on the last day, not before); urgent terminations act at once.
- FR-7 Approvals gate for sensitive steps via the approvals engine; a privileged leaver needs a second
  person.
- FR-8 Manager and IT tasks are created through the people checklist API.
- FR-9 An audit trail of every provisioning action, ids only.
- FR-10 A kill switch: `POST /v1/identity/revoke` on can-core's module API ends the PATs and sessions of
  a person the people module reported offboarded, and of nobody else.
- FR-11 Module artifacts: manifest (`inhouse`, `experimental`, `provision.*` and `scim.*` permissions),
  chart, Dockerfile, capability schema `identity.provision.v1`, events `provision.step.completed.v1` and
  `provision.failed.v1` with fixtures, `docs/provisioning.md`.

**Security (QA lessons applied)**

- SR-1 Fail closed everywhere (no directory, no approvals, unknown input).
- SR-2 Bounded work and deadlines on every connector call, scan, body, filter and plan.
- SR-3 Audit before return: state and audit entry commit together.
- SR-4 Valid credentials are never throttled; only invalid attempts are slowed.
- SR-5 Idempotency keys on triggers, runs, steps and published events.
- SR-6 Outbound calls are SSRF-safe (`core/server/netguard`), no redirects, no proxies.
- SR-7 No personal data in events, audit entries or errors; secrets never logged or stored in clear.
- SR-8 Separation of duties: a person never approves their own access or removal or flags their own
  termination; checklist identity actions are never self-assigned or completed by this module.
- SR-9 Scope clauses in code are parenthesised (no unparenthesised `AND`/`OR` mixes).

## Out of scope

Real IdP or SaaS integrations (stubs only), a UI, SCIM user creation and deletion, SCIM bulk, sort and
`/.search`, cross-cluster networking changes, per-connector credential management in the platform.

## Success criteria

- `make test` and `go test -race` green; no test contacts a real service.
- The OpenAPI contract and the registered routes agree (contract test); events and runs the engine
  produces validate against the published schemas.
- Idle RSS at most 20 MiB, measured.
- `docs/provisioning.md` lists what is unverified.
