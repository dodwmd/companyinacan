# Contracts (E14b)

- **HTTP**: `specs/012-people-directory/contracts/people-api.yaml` (OpenAPI 3.1), extended in place with the
  `reviews` and `timeoff` tags (40 operations). `test/contract/people_test.go` (`TestOpenAPIMatchesRoutes`)
  keeps it equal to the routes the service registers.
- **Events** (`schemas/events/`, copies in `modules/people/events/`, fixtures in `testdata/events/<name>/`,
  at least one valid and eight invalid each):
  - `review.cycle.opened.v1`: `cycle_id`, `opened_on`, `participants`.
  - `review.released.v1`: `cycle_id`, `review_id`, `person_id`, `released_on`. Never a rating or text.
  - `timeoff.approved.v1`: `request_id`, `person_id`, `start_date`, `end_date`, `start_part`, `end_part`,
    `halves`. Never the leave type or reason. **Dates reveal absence**: deliver it only to modules that
    subscribed because they need it (for example timesheets, to mark non-working days).
  - `timeoff.cancelled.v1`: `request_id`, `person_id`, `start_date`, `end_date`. Same caution.
  `TestServiceEventsMatchSchemas` validates the events the service really emits.
- **Manifest** (`modules/people/module.yaml`): permissions `people.reviews.{read,write,admin}` and
  `people.timeoff.{read,write,approve,admin}` (roles are namespaced by can-core); approvals `timeoff`
  (default chain `manager`) and `review-release` (default chain `hr`); data classes `performance-reviews`
  and `people-timeoff`; the four events in `publishes`.
- **Settings** (`modules/people/settings.schema.json`): `reviews.min_responses` (3..50), and retention for
  the two new classes. Leave types, accruals, holidays and blackouts are API data, not settings.
- **Approval seam**: `core/people.ApprovalEngine` (`Request`, `Status`, `Cancel`), types `timeoff` and
  `review-release`; same shape as equity's.

Error contract: 400 invalid, 401 unauthenticated, 403 visible but not permitted (including separation of
duties), 404 not found or not visible, 409 state conflict, 429 rate limited.
