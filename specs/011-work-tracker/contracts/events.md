# Contract: work events

Envelope (all events): `{ "id": "evt_<24 hex>", "type": "...", "time": RFC 3339 UTC, "source": "work", "data": {...} }`.
Schemas: `schemas/events/<type>.json` (copies in `modules/work/events/`, kept identical by a contract
test). Fixtures: `testdata/events/<type>/{valid,invalid}-*.json`. Delivered at least once to
`webhook.url`; de-duplicate on `id`.

Headers: `X-Can-Event-Id`, `X-Can-Event-Type`, `X-Can-Timestamp` (unix seconds),
`X-Can-Signature: sha256=hex(HMAC-SHA256(secret, timestamp + "." + body))`.

| Type | `data` |
|---|---|
| `work.issue.created.v1` | `issue_key`, `project`, `type`, `title`, `priority`, `status`, `reporter`, `assignee?`, `parent?`, `created_at` |
| `work.issue.updated.v1` | `issue_key`, `project`, `version` (after), `actor`, `changes` (field to `{from,to}`; fields: title, description, priority, assignee, due_date, estimate, parent, sprint_id, labels), `updated_at` |
| `work.issue.transitioned.v1` | `issue_key`, `project`, `from`, `to`, `to_category`, `actor`, `transitioned_at` |
| `work.comment.created.v1` | `issue_key`, `project`, `comment_id`, `author`, `created_at` |

No description or comment text is ever included. A priority change shows as `changes.priority`
(there is no separate `issue.priority_changed` event in this slice; SPEC.md lists it for later, with
manager notifications). A no-op update emits nothing.

## Delivery

At least once per event, retried with capped backoff (30 s doubling to 6 h) for 7 days, then
dead-lettered: logged, listed by `GET /v1/events/dead` for 90 days and replayable with
`POST /v1/events/{id}/replay`. Arrival order is not guaranteed once a retry is involved; use
`version` (`work.issue.updated.v1`) and `time`. Sprint completion emits `work.issue.updated.v1`
(`changes.sprint_id`) for each issue it moves. `work.issue.created.v1` and the title in
`changes.title` carry issue titles: the webhook receiver must be trusted with titles (descriptions
and comment text never travel).
