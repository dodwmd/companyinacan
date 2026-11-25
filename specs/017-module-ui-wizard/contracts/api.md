# Contract: admin API additions (E12b)

OpenAPI: `specs/005-can-core-mvp/contracts/admin-api.yaml` (checked by `test/contract`). Two read-only
operations; changes go through the UI's preview-then-apply flow, where the acknowledgements, confirmation
and audit steps live.

| Operation | Permission | Notes |
|---|---|---|
| `GET /api/v1/catalogue` | `platform.view` | `modules[]` (status, licence class, deployable, enabled, RAM, requires with state, provides with `served`), `conflicts[]`, `footprint` (`verdict`: fits, tight, over, unknown) |
| `GET /api/v1/status` | `platform.view` | `modules[]` (state, ready, version, sanitised `last_error`), `reconcile`, `certificates[]` with `certificates_available`, `disk[]` with `disk_available` |

Both answer 401 without a session, 403 without `platform.view`, 503 when module management is not configured.
