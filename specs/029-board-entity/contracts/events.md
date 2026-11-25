# Events: board and entity (E17b)

Tool, not legal advice. Jurisdiction rules are not implemented.

| Event | Source | Data | Schema |
|---|---|---|---|
| `board.meeting.scheduled.v1` | `board` | `meeting_id`, `body_id`, `starts_on` | `schemas/events/board.meeting.scheduled.v1.json` |
| `board.resolution.passed.v1` | `board` | `resolution_id`, `body_id`, `result` (always `passed`), `passed_on`; optional `meeting_id`, `external_type`, `external_ref` | `schemas/events/board.resolution.passed.v1.json` |
| `entity.filing.due.v1` | `entity` | `filing_id`, `obligation_id`, `entity_id`, `due_on`, `days_before` | `schemas/events/entity.filing.due.v1.json` |

Rules

- The payload carries **ids, dates and the result only**: never names, resolution text, minutes, vote counts or
  per-person votes, addresses, registration numbers or amounts. A consumer reads details through the `board` or
  `entity` capability with its own rights.
- Delivery is at least once through the outbox (signed webhook to can-core, exponential backoff). Consumers
  de-duplicate on the envelope `id`. The event row is written in the same transaction as the change.
- `entity.filing.due.v1` is sent once per reminder point of an obligation (`lead_days`, default 30, 7 and 0); if the
  sweep was down, only the latest point reached is announced.

## The seam for other modules (equity)

`equity` declares that grants need board approval and consumes an `ApprovalEngine` with `Request(type, ref,
requester)` and `Status(ref)`. The `board` module serves that seam without `equity` changing in this PR:

- `resolutions.v1` `request_resolution` (`POST /v1/resolution-requests`) maps to `Request`: `type` is `equity.grant`,
  `ref` the grant id, `requester` the subject who asked, `subjects` the grantee's person id (a grantee who is a
  director is recused automatically, and the requester can never be the sole approver).
- `resolutions.v1` `get_resolution_outcome` (`GET /v1/resolution-outcomes?type=&ref=`) maps to `Status`: `pending`,
  `approved`, `rejected` or `cancelled`, with the decision date and no names.
- `board.resolution.passed.v1` carries `external_type` and `external_ref` so a subscriber can react without polling.

Only the `integration` role (the can-core service account) may call these two verbs. The manifest declares the approval
type `resolution` (qualified name `board.resolution`: the manifest pattern allows no dot).
