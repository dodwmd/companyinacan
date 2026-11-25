# Implementation Plan: Approvals, notifications and RBAC (Epic E11a)

**Branch**: `epic/E11a-approvals-notifications` | **Spec**: [spec.md](spec.md)

## Summary

Three new packages inside can-core, wired through the existing store, audit, events, module API, admin
API and admin UI: `core/server/rbac`, `core/server/approvals`, `core/server/notify`. One SQLite
migration (`0002_platform_services.sql`). Choices in [research.md](research.md), tables in
[data-model.md](data-model.md), wire contracts in [contracts/](contracts/).

## Technical Context

Go (existing module); standard library only plus the existing `modernc.org/sqlite`. One new import of
`time/tzdata` (about 450 KiB of binary, no resident cost until a zone is loaded). Testing: `go test`,
`httptest`, fake clock, fake mailer, fake webhook, fake reporting line. No network.

## Constitution check

| Principle | Check |
|---|---|
| I. Spec-first | SPEC.md §3.7/§4.1/§4.3 are the source; the SPEC event catalogue gains `approval.decided.v1` and `notification.sent.v1` (the older `approval.approved/rejected.v1` names stay listed but are not emitted: one terminal event carries the outcome). |
| II. Footprint | Idle RSS +2 MiB measured; per-subject cache bounded to 2048 entries; all lists, ticks and fan-outs bounded. |
| III. Test-first | Tests were written before the implementation in every package (red on compile, then green); contract tests cover routes, schemas, fixtures and emitted events. |
| IV. Stable contracts | Additive: new manifest field `notifications`, new company.yaml keys, new routes and events. Existing routes keep their shape; operational routes gain a 403 for users without a role. |
| V. Secure and private by default | Least privilege (no role by default), fail closed, separation of duties fixed, escaped templates, header-injection-safe subjects, no PII in events, audit before returning module-supplied data, 404 instead of 403 for invisible requests. |
| VI. OSS hygiene | No new dependency. |
| VII. Simplicity and measured claims | One engine mutex instead of distributed locking; RAM measured and reported; gaps listed. |

## Design notes

- **Admin identity.** Subjects are strings: `user:<admin username>` or `party:<id>`. The admin_users table is unchanged, so login, TOTP and throttling are untouched. `store.CreateAdmin` makes the first admin an owner; the migration makes existing admins owners.
- **Single SQLite connection.** Code must not query while a transaction is open on the same goroutine, so the engine resolves approvers and eligibility before opening a transaction and serialises mutations with a mutex.
- **Events.** `events.Bus.PublishTo` limits webhook fan-out to one module; `SubscribeAll` feeds the notification rules.
- **Module API.** `POST /v1/check`, `POST/GET /v1/approvals`, `POST /v1/approvals/{id}/cancel`, `POST /v1/notifications`.
- **Admin API/UI.** Approvals, delegations, notifications, settings, preferences, delivery log, roles, users, overrides; UI pages `/approvals`, `/notifications`, `/access`.
- **Policy distribution.** `can apply` writes validated policies and webhook channels to the state database; can-core reads them.

## Project structure

```
core/server/rbac/        roles, assignments, overrides, cached Check
core/server/approvals/   engine, policy, delegation, directory reporting line
core/server/notify/      inbox, preferences, email, digests, webhook channels, rules, templates
core/server/{moduleapi,adminapi,adminui}/platform*.go
core/server/corecli/roles.go   break-glass role commands
core/cli/apply_platform.go     policy sync on apply
schemas/events/approval.*.json notification.sent.v1.json
```
