# Contracts (E11a)

- Module API: `specs/005-can-core-mvp/contracts/module-api.yaml` (`/v1/check`, `/v1/approvals`,
  `/v1/approvals/{id}`, `/v1/approvals/{id}/cancel`, `/v1/notifications`).
- Admin API: `specs/005-can-core-mvp/contracts/admin-api.yaml` (approvals, delegations, notifications,
  notification settings/preferences/deliveries, roles, users).
- Admin UI routes: `specs/010-admin-ui/contracts/ui-routes.md`.
- Events: `schemas/events/approval.requested.v1.json`, `approval.decided.v1.json`,
  `notification.sent.v1.json`, fixtures in `testdata/events/<name>/`.
- Manifest: `notifications:` rules in `schemas/module.v1.schema.json`; policies and webhook channels in
  `schemas/company.v1.schema.json`.

`test/contract/cancore_api_test.go` keeps the OpenAPI files equal to the routes served;
`test/contract/platform_test.go` validates fixtures, the events really emitted, and the schema additions.

## Webhook `approval.decided.v1`

Same envelope and signature headers as every event (`webhook` package). Data: `request_id`,
`module_id`, `type`, `ref`, `requester`, `outcome` (`approved rejected cancelled expired`), `approvers`
(subjects), optional `decided_at`, optional `reason` (`no_approver`). Receivers de-duplicate on the event
id and may confirm with `GET /v1/approvals/{id}`.
