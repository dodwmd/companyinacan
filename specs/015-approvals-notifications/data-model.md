# Data model (migration 0002_platform_services.sql)

| Table | Purpose |
|---|---|
| `rbac_assignments(subject, role, scope, granted_by, granted_at)` | Role held by a subject; scope `''` or a prefix ending `/*`. Migration makes existing admins `owner`. |
| `rbac_overrides(role, permission, effect, set_by, set_at)` | Operator grant/revoke relative to manifest defaults. |
| `approval_policies(type, policy_json, updated_at)` | From company.yaml via `can apply`. |
| `approval_requests` | One row per request: module, type, requester, ref, title, amount, payload, status (`pending approved rejected cancelled expired`), idempotency key and hash, policy snapshot, timers. `UNIQUE(module_id, idem_key)`. |
| `approval_steps(request_id, idx, stage, name, quorum, status, approvers)` | Steps of the snapshot; `approvers` is JSON `{roles, users}` resolved when the step opens (manager snapshot included). |
| `approval_decisions` | History: approve, reject, comment, cancel, escalate, expire, system. Unique `(request_id, step_idx, actor)` for votes. |
| `approval_delegations` | Out-of-office delegations (window, revoked flag). |
| `notifications` | Inbox rows; unique `(subject, dedupe_key)` when a key is given; ids-only `data_json`. |
| `notify_prefs`, `notify_settings` | Per type/channel switches; email address, quiet hours, time zone, digest mode. |
| `notify_deliveries` | Delivery log: `sent failed queued suppressed` per channel; queued rows feed digests. |
| `notify_channels` | Named outbound webhook channels from company.yaml. |

Step lifecycle: `waiting` -> `active` -> `approved | rejected`; `skipped` when the amount condition
fails. Stage = number of non-parallel steps before it; a stage completes when all its non-skipped steps
are approved.

Request lifecycle: `pending` -> `approved | rejected | cancelled | expired`, once.
